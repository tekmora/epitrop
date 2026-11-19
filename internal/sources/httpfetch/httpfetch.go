// Package httpfetch performs a bounded, plain GET request. It is the one
// shared implementation of "request a URL, cap the response, report the
// first failure" that every catalog source needs: extracted after praetor's
// own dedupe scan flagged the sources' identical boundedGet functions
// (HISS-19, one behavior, one implementation).
//
// The client and the byte cap come from golusoris httpx/client
// (docs/adr/0004-golusoris-pinned-base.md): this package only adds the
// per-call deadline, the status check and the error context the sources
// report.
package httpfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	gerrors "github.com/golusoris/golusoris/core/errors"
	"github.com/golusoris/golusoris/httpx/client"
)

// clientName labels the outbound client in golusoris breaker logs and OTel
// spans.
const clientName = "epitrop.httpfetch"

// RedactedURL hides passwords because parse errors and URL.String can expose them.
func RedactedURL(raw string) string {
	parsed, err := neturl.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	return parsed.Redacted()
}

// URLParseCause allows only fixed parse causes; causes quoting input can expose passwords.
func URLParseCause(err error) error {
	var urlErr *neturl.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	var escapeErr neturl.EscapeError
	if errors.As(urlErr.Err, &escapeErr) {
		return errors.New("invalid URL escape")
	}

	if urlErr.Err == nil {
		return errors.New("invalid URL")
	}
	switch urlErr.Err.Error() {
	case "missing protocol scheme", "net/url: invalid control character in URL", "empty url",
		"invalid URI for request", "first path segment in URL cannot contain colon",
		"net/url: invalid userinfo", "invalid IP-literal", "missing ']' in host":
		return urlErr.Err
	default:
		return errors.New("invalid URL")
	}
}

// withheldRequestError hides the text of a request error that may echo a URL
// password, while it still unwraps to that error and reports its timeout.
type withheldRequestError struct{ err error }

func (e withheldRequestError) Error() string {
	return "request failed; detail withheld because it may carry URL credentials"
}

func (e withheldRequestError) Unwrap() error { return e.err }

func (e withheldRequestError) Timeout() bool {
	var timeout interface{ Timeout() bool }
	return errors.As(e.err, &timeout) && timeout.Timeout()
}

// sanitizeRequestError redacts the URL of an http.Client.Do error and
// withholds any inner text containing '@', because a URL password always
// sits before an '@'. A redirect error carries the raw Location as its URL,
// and a bad Location header is quoted in the inner text.
func sanitizeRequestError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *neturl.Error
	if errors.As(err, &urlErr) {
		inner := urlErr.Err
		if inner != nil && strings.Contains(inner.Error(), "@") {
			inner = withheldRequestError{err: inner}
		}
		return &neturl.Error{Op: urlErr.Op, URL: RedactedURL(urlErr.URL), Err: inner}
	}
	if strings.Contains(err.Error(), "@") {
		return withheldRequestError{err: err}
	}
	return err
}

// Get performs a GET against url with a per-call deadline (timeout) and a
// bounded response read (maxBytes), so a source's HTTP call can neither hang
// nor buffer an unbounded response (HISS-02). Callers choose their own
// timeout and byte cap, since sources genuinely differ here (a local Ollama
// daemon warrants a shorter timeout than a public catalog fetch). A body
// over maxBytes fails with an error wrapping client.ErrBodyTooLarge; a
// non-positive maxBytes is refused.
func Get(ctx context.Context, url string, timeout time.Duration, maxBytes int) ([]byte, error) {
	return GetWithHeader(ctx, url, nil, timeout, maxBytes)
}

// GetWithHeader is Get with extra request headers, for a source that must
// authenticate (litellm-gateway's bearer token). Header values are sent,
// never logged or wrapped into an error.
func GetWithHeader(ctx context.Context, url string, header http.Header, timeout time.Duration, maxBytes int) (body []byte, err error) {
	safeURL := RedactedURL(url)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", safeURL, URLParseCause(err))
	}
	if header != nil {
		req.Header = header.Clone()
	}
	resp, err := client.New(client.Options{Name: clientName, Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", safeURL, sanitizeRequestError(err))
	}
	// Closure rather than a bare defer so the bodyclose linter sees the body
	// reach a closer.
	defer func() { gerrors.CloseJoin(resp.Body, &err, "close response body from "+safeURL) }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", safeURL, resp.StatusCode)
	}
	body, err = client.ReadAllBounded(resp.Body, int64(maxBytes))
	if err != nil {
		return nil, fmt.Errorf("read response body from %s: %w", safeURL, err)
	}
	return body, nil
}

// Do performs req with httpClient (defaulting to client.New with timeout if
// httpClient is nil), enforcing a per-call deadline, and reading at most
// maxBytes of response body. It returns the HTTP status code, response headers,
// and bounded body without converting non-200 status codes into errors. A non-200
// body is truncated at maxBytes without error; a 200 body exceeding maxBytes
// returns an error wrapping client.ErrBodyTooLarge.
func Do(ctx context.Context, httpClient *http.Client, req *http.Request, timeout time.Duration, maxBytes int) (statusCode int, header http.Header, body []byte, err error) {
	if maxBytes <= 0 {
		return 0, nil, nil, fmt.Errorf("httpfetch: non-positive maxBytes %d", maxBytes)
	}
	if req == nil {
		return 0, nil, nil, fmt.Errorf("httpfetch: nil request")
	}
	if ctx == nil {
		return 0, nil, nil, fmt.Errorf("httpfetch: nil context")
	}
	safeURL := req.URL.Redacted()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// The call deadline applies even under a caller deadline; the earlier one wins.
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(reqCtx)

	cl := httpClient
	if cl == nil {
		cl = client.New(client.Options{Name: clientName, Timeout: timeout})
	}
	// #nosec G704 -- caller supplies target URL and client; bounded transport utility.
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("request %s: %w", safeURL, sanitizeRequestError(err))
	}
	defer func() { gerrors.CloseJoin(resp.Body, &err, "close response body from "+safeURL) }()

	reader := io.Reader(resp.Body)
	if resp.StatusCode != http.StatusOK {
		reader = io.LimitReader(resp.Body, int64(maxBytes))
	}
	body, err = client.ReadAllBounded(reader, int64(maxBytes))
	if err != nil {
		return resp.StatusCode, resp.Header.Clone(), nil, fmt.Errorf("read response body from %s: %w", safeURL, err)
	}
	return resp.StatusCode, resp.Header.Clone(), body, nil
}
