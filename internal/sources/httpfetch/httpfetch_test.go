package httpfetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/httpx/client"
)

func TestGet_Positive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello")) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer server.Close()

	body, err := Get(context.Background(), server.URL, time.Second, 1024)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want %q", body, "hello")
	}
}

func TestGet_Negative(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		if _, err := Get(context.Background(), "http://127.0.0.1:1", time.Second, 1024); err == nil {
			t.Fatal("Get() = nil error, want failure for an unreachable host")
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()
		if _, err := Get(context.Background(), server.URL, time.Second, 1024); err == nil {
			t.Fatal("Get() = nil error, want failure for a non-200 status")
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("late")) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Get(ctx, server.URL, time.Second, 1024); err == nil {
			t.Fatal("Get() = nil error, want failure for an already-canceled context")
		}
	})
}

// TestGet_Boundary confirms the byte cap refuses a response that is exactly
// one byte over maxBytes, and accepts one exactly at the cap.
func TestGet_Boundary(t *testing.T) {
	t.Run("exceeds cap by one byte", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("a", 11))) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		}))
		defer server.Close()
		_, err := Get(context.Background(), server.URL, time.Second, 10)
		if err == nil {
			t.Fatal("Get() = nil error, want failure for a response one byte over the cap")
		}
		if !errors.Is(err, client.ErrBodyTooLarge) {
			t.Fatalf("Get() = %v, want an error wrapping client.ErrBodyTooLarge", err)
		}
	})

	// A zero cap cannot bound anything; it must be refused, not read as
	// "accept an empty body".
	t.Run("non-positive cap is refused", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		if _, err := Get(context.Background(), server.URL, time.Second, 0); err == nil {
			t.Fatal("Get() = nil error, want failure for a zero byte cap")
		}
	})

	t.Run("exactly at cap", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("a", 10))) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		}))
		defer server.Close()
		body, err := Get(context.Background(), server.URL, time.Second, 10)
		if err != nil {
			t.Fatalf("Get() = %v, want success for a response exactly at the cap", err)
		}
		if len(body) != 10 {
			t.Fatalf("len(body) = %d, want 10", len(body))
		}
	})
}

// TestGetWithHeader covers the authenticated variant: the header reaches the
// server and its value never reaches an error.
func TestGetWithHeader(t *testing.T) {
	const secret = "Bearer do-not-print-me" // #nosec G101 -- synthetic input verifies credential redaction

	t.Run("header reaches the server", func(t *testing.T) {
		var got string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("Authorization")
			_, _ = w.Write([]byte("ok")) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		}))
		defer server.Close()
		header := http.Header{}
		header.Set("Authorization", secret)
		if _, err := GetWithHeader(context.Background(), server.URL, header, time.Second, 1024); err != nil {
			t.Fatalf("GetWithHeader() = %v", err)
		}
		if got != secret {
			t.Fatalf("Authorization = %q, want %q", got, secret)
		}
	})

	t.Run("rejected request keeps the header value out of the error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		header := http.Header{}
		header.Set("Authorization", secret)
		_, err := GetWithHeader(context.Background(), server.URL, header, time.Second, 1024)
		if err == nil {
			t.Fatal("GetWithHeader() = nil error, want failure for HTTP 401")
		}
		if strings.Contains(err.Error(), "do-not-print-me") {
			t.Fatalf("error leaked the header value: %v", err)
		}
	})
}

func TestDo(t *testing.T) {
	t.Run("returns status headers and body for non-200 without error", testDoNon200ReturnsStatusHeadersBody)
	t.Run("boundary byte cap", testDoBoundaryByteCap)
	t.Run("non-positive maxBytes and nil request refused", testDoNonPositiveMaxBytesAndNilRequest)
}

func testDoNon200ReturnsStatusHeadersBody(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom-Header", "test-val")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found body")) //nolint:errcheck // test httptest server response
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	status, header, body, err := Do(context.Background(), nil, req, time.Second, 1024)
	if err != nil {
		t.Fatalf("Do() = %v, want nil error", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if header.Get("X-Custom-Header") != "test-val" {
		t.Fatalf("header = %q, want test-val", header.Get("X-Custom-Header"))
	}
	if string(body) != "not found body" {
		t.Fatalf("body = %q, want 'not found body'", string(body))
	}
}

func testDoBoundaryByteCap(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("12345678901")) //nolint:errcheck // test httptest server response
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	_, _, _, err = Do(context.Background(), nil, req, time.Second, 10)
	if err == nil || !errors.Is(err, client.ErrBodyTooLarge) {
		t.Fatalf("Do() = %v, want ErrBodyTooLarge", err)
	}
}

func testDoNonPositiveMaxBytesAndNilRequest(t *testing.T) {
	t.Helper()
	if _, _, _, err := Do(context.Background(), nil, nil, time.Second, 0); err == nil {
		t.Fatal("Do(maxBytes=0) = nil error, want error")
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	//nolint:staticcheck // testing defensive check against nil ctx
	if _, _, _, err := Do(nil, nil, req, time.Second, 100); err == nil {
		t.Fatal("Do(ctx=nil) = nil error, want error")
	}
}

func TestDoAppliesCallTimeoutUnderCallerDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	began := time.Now()
	if _, _, _, err = Do(ctx, &http.Client{}, req, 100*time.Millisecond, 1024); err == nil {
		t.Fatal("Do(slow server, 100ms call timeout) = nil error, want deadline")
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("Do took %v, want the 100ms call timeout to apply under the 30s caller deadline", took)
	}
}
