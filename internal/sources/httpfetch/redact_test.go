package httpfetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/httpx/client"
)

func TestURLParseCause(t *testing.T) {
	plain := errors.New("plain failure")
	cases := []struct {
		name        string
		input, want error
	}{
		{"nil", nil, nil},
		{"plain", plain, plain},
		{"wrapped", &url.Error{Op: "parse", URL: "secret", Err: plain}, plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := URLParseCause(tc.input)
			if !errors.Is(got, tc.want) {
				t.Fatalf("cause = %v, want %v", got, tc.want)
			}
		})
	}
	got := URLParseCause(&url.Error{Op: "parse", URL: "secret", Err: url.EscapeError("%zz")})
	if got == nil || got.Error() != "invalid URL escape" {
		t.Fatalf("escape cause = %v", got)
	}
}

func TestRedactedURL(t *testing.T) {
	raw := "https://user:s3cret@example.invalid/p"
	got := RedactedURL(raw)
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "user") || !strings.Contains(got, "example.invalid") {
		t.Fatalf("redaction: %q", got)
	}
	plain := "https://example.invalid/p"
	if got := RedactedURL(plain); got != plain {
		t.Fatalf("plain URL: %q", got)
	}
	if got := RedactedURL("https://user:s3cret@exa mple.invalid/"); got != "<unparseable url>" {
		t.Fatalf("invalid URL: %q", got)
	}
}

func TestDoNon200OverCapReturnsStatus(t *testing.T) {
	const capBytes = 1024
	cases := []struct {
		status, size int
		tooLarge     bool
	}{{503, 4096, false}, {503, capBytes, false}, {503, capBytes + 1, false}, {200, capBytes, false}, {200, capBytes + 1, true}, {200, 4096, true}}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			if _, err := w.Write([]byte(strings.Repeat("x", tc.size))); err != nil {
				t.Errorf("write: %v", err)
			}
		}))
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		status, _, body, err := Do(context.Background(), srv.Client(), req, time.Second, capBytes)
		srv.Close()
		if tc.tooLarge {
			if !errors.Is(err, client.ErrBodyTooLarge) {
				t.Fatalf("error = %v", err)
			}
			continue
		}
		if err != nil || status != tc.status || string(body) != strings.Repeat("x", capBytes) {
			t.Fatalf("status = %d, body = %d, error = %v", status, len(body), err)
		}
	}
}

func TestGetWithHeaderStatusRedactsPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	raw := strings.Replace(srv.URL, "http://", "http://user:s3cret@", 1)
	_, err := GetWithHeader(context.Background(), raw, nil, time.Second, 1024)
	if err == nil || !strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("status error = %v", err)
	}
}

func TestGetWithHeaderEscapeErrorRedactsPassword(t *testing.T) {
	_, err := GetWithHeader(context.Background(), "https://user:p4ss%zzw0rd@example.invalid/x", nil, time.Second, 1024)
	if err == nil {
		t.Fatal("expected invalid URL error")
	}
	for _, secret := range []string{"p4ss", "w0rd", "zz"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposes %q: %v", secret, err)
		}
	}
}

func TestDoAndGetErrorsRedactPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	raw := strings.Replace(srv.URL, "http://", "http://user:s3cret@", 1)
	cl := srv.Client()
	srv.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = Do(context.Background(), cl, req, time.Second, 1024)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("Do error = %v", err)
	}
	_, err = Get(context.Background(), raw, time.Second, 1024)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("Get error = %v", err)
	}
	_, err = Get(context.Background(), "https://user:s3cret@exa mple.invalid/", time.Second, 1024)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("Get invalid error = %v", err)
	}
}
