package litellmgateway

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/httpx/client"

	"github.com/tekmora/epitrop/catalog"
)

var fixtureNow = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

func writeTokenFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func TestFetch_Positive(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"example/auto","object":"model","created":1,"owned_by":"openai"},{"id":"example/chat","object":"model","created":1,"owned_by":"openai"}]}`)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer server.Close()

	tokenPath := writeTokenFile(t, "shhh-secret-token\n")
	res := Fetch(context.Background(), server.URL, tokenPath, fixtureNow)

	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail = %q, want ok", res.Status, res.Detail)
	}
	if res.Count != 2 {
		t.Fatalf("Count = %d, want 2", res.Count)
	}
	if gotAuth != "Bearer shhh-secret-token" {
		t.Fatalf("Authorization header = %q, want trimmed token", gotAuth)
	}
	for _, rec := range res.Records {
		if err := rec.Validate(); err != nil {
			t.Fatalf("Validate() = %v", err)
		}
		if rec.AccessPath != catalog.AccessGateway {
			t.Fatalf("AccessPath = %v, want gateway", rec.AccessPath)
		}
	}

	t.Run("trailing slash handled", func(t *testing.T) {
		resSlash := Fetch(context.Background(), server.URL+"/", tokenPath, fixtureNow)
		if resSlash.Status != catalog.StatusOK {
			t.Fatalf("Status = %v, detail = %q, want ok with trailing slash", resSlash.Status, resSlash.Detail)
		}
		if resSlash.Count != 2 {
			t.Fatalf("Count = %d, want 2", resSlash.Count)
		}
	})
}

func TestUpstreamContractDocumentPinned(t *testing.T) {
	body := mustReadFixture(t, "testdata/upstream/openai-models-openapi.yaml")
	for _, marker := range [][]byte{
		[]byte("/models:"),
		[]byte("ListModelsResponse"),
		[]byte("owned_by:"),
		[]byte("id:"),
	} {
		if !bytes.Contains(body, marker) {
			t.Fatalf("OpenAI upstream contract missing marker %q", marker)
		}
	}
}

func TestFetch_UpstreamFixtureContract(t *testing.T) {
	body := string(mustReadFixture(t, "testdata/fixtures/models-response.json"))
	tokenPath := writeTokenFile(t, "tok")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body)) //nolint:errcheck // test server response
	}))
	defer server.Close()

	res := Fetch(context.Background(), server.URL, tokenPath, fixtureNow)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail = %q, want ok", res.Status, res.Detail)
	}

	mutated := strings.Replace(body, `"id":"vendor-a/model-1",`, "", 1)
	mutatedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(mutated)) //nolint:errcheck // test server response
	}))
	defer mutatedServer.Close()
	res = Fetch(context.Background(), mutatedServer.URL, tokenPath, fixtureNow)
	if res.Status != catalog.StatusFail {
		t.Fatalf("Status = %v, detail = %q, want fail for missing model id", res.Status, res.Detail)
	}
}

// TestFetch_NeverLeaksToken asserts the token never appears verbatim in any
// Result.Detail, across every failure path this source can hit.
func TestFetch_NeverLeaksToken(t *testing.T) {
	const secret = "do-not-print-me-12345"

	t.Run("http error", func(t *testing.T) {
		tokenPath := writeTokenFile(t, secret)
		res := Fetch(context.Background(), "http://127.0.0.1:1", tokenPath, fixtureNow)
		if strings.Contains(res.Detail, secret) {
			t.Fatalf("Detail leaked the token: %q", res.Detail)
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer server.Close()
		tokenPath := writeTokenFile(t, secret)
		res := Fetch(context.Background(), server.URL, tokenPath, fixtureNow)
		if res.Status != catalog.StatusFail {
			t.Fatalf("Status = %v, want fail for HTTP 403", res.Status)
		}
		if strings.Contains(res.Detail, secret) {
			t.Fatalf("Detail leaked the token: %q", res.Detail)
		}
	})

	t.Run("missing token file", func(t *testing.T) {
		missingPath := filepath.Join(t.TempDir(), "missing-token-file")
		res := Fetch(context.Background(), "http://example.invalid", missingPath, fixtureNow)
		if res.Status != catalog.StatusFail {
			t.Fatalf("Status = %v, want fail for a missing token file", res.Status)
		}
		if strings.Contains(res.Detail, secret) {
			t.Fatalf("Detail leaked the token: %q", res.Detail)
		}
	})
}

func mustReadFixture(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return body
}

func TestFetch_Negative(t *testing.T) {
	t.Run("missing token file", func(t *testing.T) {
		res := Fetch(context.Background(), "http://example.invalid", filepath.Join(t.TempDir(), "missing"), fixtureNow)
		if res.Status != catalog.StatusFail {
			t.Fatalf("Status = %v, want fail for a missing token file", res.Status)
		}
	})

	t.Run("empty token file", func(t *testing.T) {
		tokenPath := writeTokenFile(t, "   \n")
		res := Fetch(context.Background(), "http://example.invalid", tokenPath, fixtureNow)
		if res.Status != catalog.StatusFail {
			t.Fatalf("Status = %v, want fail for an empty token file", res.Status)
		}
	})

	t.Run("malformed json body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{not json")) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		}))
		defer server.Close()
		tokenPath := writeTokenFile(t, "tok")
		res := Fetch(context.Background(), server.URL, tokenPath, fixtureNow)
		if res.Status != catalog.StatusFail {
			t.Fatalf("Status = %v, want fail for a malformed body", res.Status)
		}
	})
}

// TestFetch_Boundary covers the oversized-token-file bound and the
// zero-models response, both edges of the size/skip contract.
func TestFetch_Boundary(t *testing.T) {
	t.Run("token file exceeds bound", func(t *testing.T) {
		oversized := make([]byte, maxTokenFileBytes+1)
		for i := range oversized {
			oversized[i] = 'a'
		}
		tokenPath := writeTokenFile(t, string(oversized))
		res := Fetch(context.Background(), "http://example.invalid", tokenPath, fixtureNow)
		if res.Status != catalog.StatusFail {
			t.Fatalf("Status = %v, want fail for an oversized token file", res.Status)
		}
	})

	t.Run("zero models fails as an empty response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":[]}`)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		}))
		defer server.Close()
		tokenPath := writeTokenFile(t, "tok")
		res := Fetch(context.Background(), server.URL, tokenPath, fixtureNow)
		if res.Status != catalog.StatusFail {
			t.Fatalf("Status = %v, want fail for zero models", res.Status)
		}
		if !strings.Contains(res.Detail, "empty response") {
			t.Fatalf("Detail = %q, want empty response", res.Detail)
		}
	})
}

// TestListModels_ResponseCap pins the /v1/models byte cap at its exact edge:
// a body of exactly maxResponseBytes is read, one byte more fails with
// client.ErrBodyTooLarge, so a caller can tell an oversized gateway response
// from a network or status failure.
func TestListModels_ResponseCap(t *testing.T) {
	serve := func(size int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := []byte(`{"data":[]}` + strings.Repeat(" ", size-len(`{"data":[]}`)))
			_, _ = w.Write(body) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		}))
	}

	t.Run("exactly at cap", func(t *testing.T) {
		server := serve(maxResponseBytes)
		defer server.Close()
		if _, err := listModels(context.Background(), server.URL, "tok"); err != nil {
			t.Fatalf("listModels() = %v, want success for a response exactly at the cap", err)
		}
	})

	t.Run("one byte over cap", func(t *testing.T) {
		server := serve(maxResponseBytes + 1)
		defer server.Close()
		_, err := listModels(context.Background(), server.URL, "tok")
		if !errors.Is(err, client.ErrBodyTooLarge) {
			t.Fatalf("listModels() = %v, want an error wrapping client.ErrBodyTooLarge", err)
		}
	})
}

func TestFetch_StampsInjectedNow(t *testing.T) {
	fixed := time.Date(2026, 4, 1, 10, 20, 30, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1","object":"model"}]}`)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer server.Close()

	tokenPath := writeTokenFile(t, "tok")
	res := Fetch(context.Background(), server.URL, tokenPath, fixed)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, want ok", res.Status)
	}
	if len(res.Records) != 1 {
		t.Fatalf("Records len = %d, want 1", len(res.Records))
	}
	if !res.Records[0].Provenance.FetchedAt.Equal(fixed) {
		t.Fatalf("FetchedAt = %v, want %v", res.Records[0].Provenance.FetchedAt, fixed)
	}
}
