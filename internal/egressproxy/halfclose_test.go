package egressproxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type serverOutcome struct {
	body []byte
	err  error
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func serveOneConn(server net.Conn, writeAnswer bool) serverOutcome {
	body, readErr := io.ReadAll(server)
	var writeErr error
	if readErr == nil && writeAnswer {
		_, writeErr = fmt.Fprintf(server, "got:%s", body)
	}
	closeErr := ignoreClosed(server.Close())
	return serverOutcome{
		body: body,
		err:  errors.Join(readErr, writeErr, closeErr),
	}
}

func checkPositiveExchange(t *testing.T, iter int, client net.Conn, serverCh <-chan serverOutcome, payload []byte) {
	t.Helper()
	hc, ok := client.(halfCloser)
	if !ok {
		t.Fatalf("iter %d: client %T does not implement CloseWrite", iter, client)
	}
	if err := hc.CloseWrite(); err != nil {
		t.Fatalf("iter %d: client CloseWrite: %v", iter, err)
	}

	res := <-serverCh
	if res.err != nil {
		if isTimeout(res.err) {
			t.Fatalf("iter %d: server read timed out after %d bytes: %v", iter, len(res.body), res.err)
		}
		t.Fatalf("iter %d: server read failed after %d bytes: %v", iter, len(res.body), res.err)
	}
	if !bytes.Equal(res.body, payload) {
		t.Fatalf("iter %d: server body = %q, want %q", iter, res.body, payload)
	}

	clientAns, clientErr := io.ReadAll(client)
	if clientErr != nil {
		if isTimeout(clientErr) {
			t.Fatalf("iter %d: client read timed out after %d bytes: %v", iter, len(clientAns), clientErr)
		}
		t.Fatalf("iter %d: client read failed after %d bytes: %v", iter, len(clientAns), clientErr)
	}
	wantAns := "got:" + string(payload)
	if string(clientAns) != wantAns {
		t.Fatalf("iter %d: client answer = %q, want %q", iter, clientAns, wantAns)
	}
}

func checkNegativeTimeout(t *testing.T, iter int, serverCh <-chan serverOutcome, payload []byte) {
	t.Helper()
	res := <-serverCh
	if res.err == nil {
		t.Fatalf("iter %d: server read succeeded unexpectedly without CloseWrite", iter)
	}
	if !isTimeout(res.err) {
		t.Fatalf("iter %d: server read error = %v, want timeout", iter, res.err)
	}
	if !bytes.Equal(res.body, payload) {
		t.Fatalf("iter %d: server read %q before timeout, want %q", iter, res.body, payload)
	}
}

type acceptedConn struct {
	conn net.Conn
	err  error
}

func runOneIteration(t *testing.T, iter int, l net.Listener, payload []byte, doCloseWrite bool, deadline time.Duration) {
	t.Helper()
	acceptCh := make(chan acceptedConn, 1)
	go func() {
		c, err := l.Accept()
		acceptCh <- acceptedConn{conn: c, err: err}
	}()

	client, err := (&net.Dialer{}).DialContext(t.Context(), l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatalf("iter %d: dial: %v", iter, err)
	}
	defer func() {
		if closeErr := ignoreClosed(client.Close()); closeErr != nil {
			t.Errorf("iter %d: client.Close() = %v", iter, closeErr)
		}
	}()

	ac := <-acceptCh
	if ac.err != nil {
		t.Fatalf("iter %d: accept: %v", iter, ac.err)
	}
	server := ac.conn
	defer func() {
		if closeErr := ignoreClosed(server.Close()); closeErr != nil {
			t.Errorf("iter %d: server.Close() = %v", iter, closeErr)
		}
	}()

	if err := client.SetDeadline(time.Now().Add(deadline)); err != nil {
		t.Fatalf("iter %d: client SetDeadline: %v", iter, err)
	}
	if err := server.SetDeadline(time.Now().Add(deadline)); err != nil {
		t.Fatalf("iter %d: server SetDeadline: %v", iter, err)
	}

	serverCh := make(chan serverOutcome, 1)
	go func() {
		serverCh <- serveOneConn(server, doCloseWrite)
	}()

	if _, err := client.Write(payload); err != nil {
		t.Fatalf("iter %d: client write: %v", iter, err)
	}

	if doCloseWrite {
		checkPositiveExchange(t, iter, client, serverCh, payload)
		return
	}
	checkNegativeTimeout(t, iter, serverCh, payload)
}

func runHalfCloseLoop(t *testing.T, l net.Listener, iters int, doCloseWrite bool, timeout time.Duration) {
	t.Helper()
	payload := []byte("halfclose-probe-test-payload-bytes")
	for iter := 0; iter < iters; iter++ {
		runOneIteration(t, iter, l, payload, doCloseWrite, timeout)
	}
}

// TestUnixHalfCloseReachesThePeer tests that half-closing a connection reaches the peer as EOF.
func TestUnixHalfCloseReachesThePeer(t *testing.T) {
	t.Run("unix", func(t *testing.T) {
		socketPath := filepath.Join(shortDir(t), "hc.sock")
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socketPath)
		if err != nil {
			t.Fatalf("Listen(unix) = %v", err)
		}
		defer func() {
			if closeErr := ignoreClosed(l.Close()); closeErr != nil {
				t.Errorf("l.Close() = %v", closeErr)
			}
		}()
		runHalfCloseLoop(t, l, 200, true, testWait)
	})

	t.Run("tcp", func(t *testing.T) {
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen(tcp) = %v", err)
		}
		defer func() {
			if closeErr := ignoreClosed(l.Close()); closeErr != nil {
				t.Errorf("l.Close() = %v", closeErr)
			}
		}()
		runHalfCloseLoop(t, l, 200, true, testWait)
	})

	t.Run("unix_without_closewrite", func(t *testing.T) {
		socketPath := filepath.Join(shortDir(t), "neg.sock")
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socketPath)
		if err != nil {
			t.Fatalf("Listen(unix) = %v", err)
		}
		defer func() {
			if closeErr := ignoreClosed(l.Close()); closeErr != nil {
				t.Errorf("l.Close() = %v", closeErr)
			}
		}()
		runHalfCloseLoop(t, l, 1, false, 200*time.Millisecond)
	})
}
