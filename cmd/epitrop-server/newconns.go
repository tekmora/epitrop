package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
)

// newConns tracks the connections that have sent no request yet (http.StateNew), so a
// stop can close them. http.Server.Shutdown waits for such a connection until it is
// five seconds old (net/http, issue 22682), which lets one silent client hold every stop
// for that long (#247).
type newConns struct {
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	stopped bool
	err     error
}

func newNewConns() *newConns {
	return &newConns{conns: make(map[net.Conn]struct{})}
}

// track is the server's ConnState hook. A connection that arrives after closeAll is
// closed at once.
func (n *newConns) track(c net.Conn, state http.ConnState) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if state != http.StateNew {
		delete(n.conns, c)
		return
	}
	if n.stopped {
		n.err = errors.Join(n.err, closeNewConn(c))
		return
	}
	n.conns[c] = struct{}{}
}

// closeAll closes the tracked connections and, from then on, every new one; closeErr
// reports how those closes went.
func (n *newConns) closeAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stopped = true
	for c := range n.conns {
		n.err = errors.Join(n.err, closeNewConn(c))
		delete(n.conns, c)
	}
}

// closeErr returns the errors of the closes closeAll and track made so far.
func (n *newConns) closeErr() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.err
}

// closeNewConn closes c. A connection the server or the client closed first is no error.
func closeNewConn(c net.Conn) error {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("epitrop-server: close idle connection: %w", err)
	}
	return nil
}
