package httpfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	_, invalidPort := url.Parse(strings.Join([]string{"https://user:s3cret", "@example.invalid/"}, "?"))
	_, fixedParse := url.Parse(strings.Join([]string{" ", "http://x"}, ""))
	cases := []struct {
		name  string
		input error
		want  string
		same  error
	}{
		{"nil", nil, "", nil},
		{"plain", plain, "plain failure", plain},
		{"nil inner", &url.Error{Op: "parse", URL: "https://user:s3cret@example.invalid/"}, "invalid URL", nil},
		{"unknown", &url.Error{Err: plain}, "invalid URL", nil},
		{"escape", &url.Error{Err: url.EscapeError("%zz")}, "invalid URL escape", nil},
		{"invalid port", invalidPort, "invalid URL", nil},
		{"real fixed", fixedParse, "first path segment in URL cannot contain colon", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkParseCause(t, URLParseCause(tc.input), tc.want, tc.same)
		})
	}
	for _, text := range []string{
		"missing protocol scheme",
		"net/url: invalid control character in URL",
		"empty url",
		"invalid URI for request",
		"first path segment in URL cannot contain colon",
		"net/url: invalid userinfo",
		"invalid IP-literal",
		"missing ']' in host",
	} {
		t.Run(text, func(t *testing.T) {
			cause := errors.New(text)
			input := &url.Error{Op: "parse", URL: "https://user:s3cret@example.invalid/", Err: cause}
			checkParseCause(t, URLParseCause(input), text, cause)
		})
	}
}

func checkParseCause(t *testing.T, got error, want string, same error) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Fatalf("cause = %v", got)
		}
		return
	}
	if got == nil || got.Error() != want || strings.Contains(got.Error(), "s3cret") {
		t.Fatalf("cause = %v, want %q", got, want)
	}
	if same != nil && !errors.Is(got, same) {
		t.Fatalf("cause = %v, want identity %v", got, same)
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
			if _, err := io.WriteString(w, strings.Repeat("x", tc.size)); err != nil {
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

func TestGetWithHeaderInvalidPortRedactsPassword(t *testing.T) {
	_, err := GetWithHeader(context.Background(), "https://user:s3cret?@example.invalid/", nil, time.Second, 1024)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error = %v", err)
	}
}

func TestSanitizeRequestError(t *testing.T) {
	plain := errors.New("plain")
	secret := errors.New("user:s3cret@host")
	timeout := fmt.Errorf("x user:s3cret@h: %w", context.DeadlineExceeded)
	cases := []struct {
		name              string
		input             error
		wantSame          error
		wantURL, wantText string
		isTimeout         bool
	}{
		{name: "nil"},
		{name: "plain", input: plain, wantSame: plain, wantText: "plain"},
		{name: "plain secret", input: secret, wantSame: secret},
		{name: "URL", input: &url.Error{Op: "Get", URL: "http://user:s3cret@host/", Err: plain}, wantSame: plain, wantURL: "http://user:xxxxx@host/", wantText: "plain"},
		{name: "timeout secret", input: &url.Error{Op: "Get", URL: "http://host/", Err: timeout}, wantSame: context.DeadlineExceeded, wantURL: "http://host/", isTimeout: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeRequestError(tc.input)
			checkSanitizedError(t, got, tc.wantSame, tc.wantText)
			if tc.wantURL != "" {
				checkSanitizedURL(t, got, tc.input, tc.wantURL, tc.isTimeout)
			}
		})
	}
}

func checkSanitizedError(t *testing.T, got, wantSame error, wantText string) {
	t.Helper()
	if wantSame == nil {
		if got != nil {
			t.Fatalf("error = %v", got)
		}
		return
	}
	if got == nil || strings.Contains(got.Error(), "s3cret") || !errors.Is(got, wantSame) {
		t.Fatalf("error = %v", got)
	}
	if wantText != "" && !strings.Contains(got.Error(), wantText) {
		t.Fatalf("error = %v", got)
	}
}

func checkSanitizedURL(t *testing.T, got, original error, wantURL string, wantTimeout bool) {
	t.Helper()
	var ue *url.Error
	if !errors.As(got, &ue) || ue.URL != wantURL || ue.Timeout() != wantTimeout {
		t.Fatalf("URL error = %v", got)
	}
	var originalURL *url.Error
	if !errors.As(original, &originalURL) {
		t.Fatalf("original is not a URL error: %v", original)
	}
	if ue == originalURL || ue.Op != originalURL.Op {
		t.Fatalf("original reused or operation changed: %v", got)
	}
}

func TestRedirectErrorsRedactPassword(t *testing.T) {
	for _, tc := range []struct{ name, location string }{{"loop", "loop"}, {"bad location", "http://user:s3cret@exa mple.invalid/"}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				location := tc.location
				if location == "loop" {
					location = "http://user:s3cret@" + r.Host + "/loop"
				}
				w.Header().Set("Location", location)
				w.WriteHeader(http.StatusFound)
			}))
			defer srv.Close()
			_, err := Get(context.Background(), srv.URL, time.Second*5, 1024)
			checkRedirectError(t, err, tc.name == "loop")
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = Do(context.Background(), srv.Client(), req, time.Second*5, 1024)
			checkRedirectError(t, err, tc.name == "loop")
		})
	}
}

func checkRedirectError(t *testing.T, err error, checkURL bool) {
	t.Helper()
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error = %v", err)
	}
	var ue *url.Error
	if checkURL && !errors.As(err, &ue) {
		t.Fatalf("missing URL error: %v", err)
	}
}
