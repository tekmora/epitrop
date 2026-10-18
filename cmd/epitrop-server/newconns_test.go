package main

import (
	"errors"
	"net"
	"net/http"
	"testing"
)

// fakeConn records whether it was closed and fails its close with closeErr.
type fakeConn struct {
	net.Conn
	closed   bool
	closeErr error
}

func (c *fakeConn) Close() error {
	c.closed = true
	return c.closeErr
}

func TestNewConnsClosesOnlyConnectionsWithoutARequest(t *testing.T) {
	n := newNewConns()
	silent, active := &fakeConn{}, &fakeConn{}
	n.track(silent, http.StateNew)
	n.track(active, http.StateNew)
	n.track(active, http.StateActive)
	n.closeAll()
	if !silent.closed {
		t.Error("closeAll left a connection without a request open")
	}
	if active.closed {
		t.Error("closeAll closed a connection whose request is in flight")
	}
	if err := n.closeErr(); err != nil {
		t.Errorf("closeErr = %v, want nil", err)
	}
}

func TestNewConnsClosesALateConnectionAtOnce(t *testing.T) {
	n := newNewConns()
	n.closeAll()
	late := &fakeConn{}
	n.track(late, http.StateNew)
	if !late.closed {
		t.Error("a connection that arrived after closeAll stayed open")
	}
}

func TestNewConnsReportsCloseErrors(t *testing.T) {
	n := newNewConns()
	gone := &fakeConn{closeErr: net.ErrClosed}
	broken := &fakeConn{closeErr: errors.New("boom")}
	n.track(gone, http.StateNew)
	n.track(broken, http.StateNew)
	n.closeAll()
	err := n.closeErr()
	if err == nil || errors.Is(err, net.ErrClosed) {
		t.Fatalf("closeErr = %v, want only the failed close", err)
	}
	n.track(&fakeConn{closeErr: errors.New("late boom")}, http.StateNew)
	if got := n.closeErr(); got == nil || got.Error() == err.Error() {
		t.Errorf("closeErr after a failed late close = %v, want it to include that close", got)
	}
}
