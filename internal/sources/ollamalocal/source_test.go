package ollamalocal

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/httpx/client"

	"github.com/tekmora/epitrop/catalog"
)

func newServer(t *testing.T, tags, ps string, psStatus int) *httptest.Server {
	t.Helper()
	if psStatus == 0 {
		psStatus = http.StatusOK
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(tags)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		case "/api/ps":
			w.WriteHeader(psStatus)
			_, _ = w.Write([]byte(ps)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

var fixtureNow = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

const sampleTags = `{"models":[{"name":"hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_M","model":"hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_M","modified_at":"2026-08-22T20:09:26Z","size":17395587762,"details":{"parameter_size":"27.3B","quantization_level":"Q4_K_M","context_length":262144},"capabilities":["tools","thinking"]},{"name":"gpt-oss:20b","details":{"parameter_size":"20.9B","quantization_level":"MXFP4","context_length":32768},"capabilities":["completion"]}]}`

const samplePS = `{"models":[{"name":"gpt-oss:20b","size":13000056503,"expires_at":"2026-09-18T19:44:37Z","size_vram":13000056503,"context_length":32768}]}`

func TestUpstreamContractDocumentPinned(t *testing.T) {
	body := mustReadFixture(t, "testdata/upstream/ollama-api-fragment.md")
	for _, marker := range [][]byte{
		[]byte("GET /api/tags"),
		[]byte("GET /api/ps"),
		[]byte(`"models"`),
		[]byte(`"details"`),
		[]byte(`"parameter_size"`),
		[]byte(`"quantization_level"`),
	} {
		if !bytes.Contains(body, marker) {
			t.Fatalf("Ollama upstream contract missing marker %q", marker)
		}
	}
}

func TestFetch_UpstreamFixtureContract(t *testing.T) {
	tags := string(mustReadFixture(t, "testdata/fixtures/tags.json"))
	ps := string(mustReadFixture(t, "testdata/fixtures/ps.json"))
	server := newServer(t, tags, ps, http.StatusOK)
	defer server.Close()

	res := Fetch(context.Background(), server.URL, fixtureNow)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail = %q, want ok", res.Status, res.Detail)
	}

	mutatedTags := strings.Replace(tags, `"name":"vendor-a/model-1",`, "", 1)
	mutatedServer := newServer(t, mutatedTags, ps, http.StatusOK)
	defer mutatedServer.Close()
	res = Fetch(context.Background(), mutatedServer.URL, fixtureNow)
	if res.Status != catalog.StatusFail {
		t.Fatalf("Status = %v, detail = %q, want fail for missing model name", res.Status, res.Detail)
	}
}

func TestFetch_Positive(t *testing.T) {
	server := newServer(t, sampleTags, samplePS, http.StatusOK)
	defer server.Close()

	res := Fetch(context.Background(), server.URL, fixtureNow)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail = %q, want ok", res.Status, res.Detail)
	}
	if res.Count != 2 {
		t.Fatalf("Count = %d, want 2", res.Count)
	}
	var found bool
	for _, rec := range res.Records {
		if err := rec.Validate(); err != nil {
			t.Fatalf("Validate() = %v", err)
		}
		if rec.ModelID == "gpt-oss:20b" {
			found = true
			if !contains(rec.Capabilities, "loaded") {
				t.Fatalf("Capabilities = %v, want loaded flag for a model /api/ps reports", rec.Capabilities)
			}
			if rec.ContextWindow == nil || *rec.ContextWindow != 32768 {
				t.Fatalf("ContextWindow = %v, want 32768", rec.ContextWindow)
			}
		}
	}
	if !found {
		t.Fatal("gpt-oss:20b record not found")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
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
	t.Run("tags endpoint unreachable", testFetchNegativeTagsUnreachable)
	t.Run("malformed tags body", testFetchNegativeMalformedTagsBody)
	t.Run("ps failure degrades, does not fail the source", testFetchNegativePSFailureDegrades)
}

func testFetchNegativeTagsUnreachable(t *testing.T) {
	t.Helper()
	res := Fetch(context.Background(), "http://127.0.0.1:1", fixtureNow)
	if res.Status != catalog.StatusFail {
		t.Fatalf("Status = %v, want fail when /api/tags is unreachable", res.Status)
	}
}

func testFetchNegativeMalformedTagsBody(t *testing.T) {
	t.Helper()
	server := newServer(t, "{not json", samplePS, http.StatusOK)
	defer server.Close()
	res := Fetch(context.Background(), server.URL, fixtureNow)
	if res.Status != catalog.StatusFail {
		t.Fatalf("Status = %v, want fail for a malformed /api/tags body", res.Status)
	}
}

func testFetchNegativePSFailureDegrades(t *testing.T) {
	t.Helper()
	server := newServer(t, sampleTags, "", http.StatusInternalServerError)
	defer server.Close()
	res := Fetch(context.Background(), server.URL, fixtureNow)
	if res.Status != catalog.StatusDegraded {
		t.Fatalf("Status = %v, detail = %q, want degraded with a note when only /api/ps fails", res.Status, res.Detail)
	}
	if res.Detail == "" {
		t.Fatal("Detail = empty, want a note explaining the /api/ps failure")
	}
	for i := 0; i < len(res.Records); i++ {
		rec := res.Records[i]
		if rec.Absent["capabilities.loaded"] == "" {
			t.Fatalf("Absent = %+v, want unknown loaded-state reason", rec.Absent)
		}
		if contains(rec.Capabilities, "loaded") {
			t.Fatalf("Capabilities = %v, want no loaded flag when /api/ps failed", rec.Capabilities)
		}
	}
}

// TestFetch_Boundary covers the zero-installed-models response, which must
// read as a skip rather than an empty-but-ok result.
func TestFetch_Boundary(t *testing.T) {
	server := newServer(t, `{"models":[]}`, samplePS, http.StatusOK)
	defer server.Close()
	res := Fetch(context.Background(), server.URL, fixtureNow)
	if res.Status != catalog.StatusSkip {
		t.Fatalf("Status = %v, want skip for zero installed models", res.Status)
	}
}

func TestToRecord_UnknownLoadedState(t *testing.T) {
	rec := toRecord(tagEntry{
		Name: "vendor-a/model-1",
		Details: modelDetails{
			ContextLength: 1024,
		},
	}, false, false, time.Unix(1, 0).UTC())
	if rec.Absent["capabilities.loaded"] == "" {
		t.Fatalf("Absent = %+v, want loaded-state reason", rec.Absent)
	}
	if contains(rec.Capabilities, "loaded") {
		t.Fatalf("Capabilities = %v, want no loaded flag", rec.Capabilities)
	}
}

func buildModelsJSON(count int) []byte {
	var b strings.Builder
	b.WriteString(`{"models":[`)
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"m"}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func TestGetTags_ResponseCap(t *testing.T) {
	serve := func(size int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			prefix := `{"models":[{"name":"m"}]}`
			body := []byte(prefix + strings.Repeat(" ", size-len(prefix)))
			if _, err := w.Write(body); err != nil {
				t.Errorf("write stand-in response: %v", err)
			}
		}))
	}

	t.Run("exactly at cap", func(t *testing.T) {
		server := serve(maxResponseBytes)
		defer server.Close()
		parsed, err := getTags(context.Background(), server.URL)
		if err != nil {
			t.Fatalf("getTags() = %v, want success at cap", err)
		}
		if len(parsed.Models) != 1 {
			t.Fatalf("models count = %d, want 1", len(parsed.Models))
		}
	})

	t.Run("one byte over cap", func(t *testing.T) {
		server := serve(maxResponseBytes + 1)
		defer server.Close()
		_, err := getTags(context.Background(), server.URL)
		if !errors.Is(err, client.ErrBodyTooLarge) {
			t.Fatalf("getTags() err = %v, want ErrBodyTooLarge", err)
		}
	})
}

func TestGetTags_MaxModelsCap(t *testing.T) {
	t.Run("maxModels passes", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := w.Write(buildModelsJSON(maxModels)); err != nil {
				t.Errorf("write stand-in response: %v", err)
			}
		}))
		defer server.Close()
		parsed, err := getTags(context.Background(), server.URL)
		if err != nil {
			t.Fatalf("getTags() = %v, want success at maxModels", err)
		}
		if len(parsed.Models) != maxModels {
			t.Fatalf("len(parsed.Models) = %d, want %d", len(parsed.Models), maxModels)
		}
	})

	t.Run("maxModels+1 fails", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := w.Write(buildModelsJSON(maxModels + 1)); err != nil {
				t.Errorf("write stand-in response: %v", err)
			}
		}))
		defer server.Close()
		_, err := getTags(context.Background(), server.URL)
		if err == nil || !strings.Contains(err.Error(), "lists more than") {
			t.Fatalf("getTags() err = %v, want error with 'lists more than'", err)
		}
	})
}

func TestFetch_PSMaxModelsDegrades(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			if _, err := w.Write([]byte(`{"models":[{"name":"local-m"}]}`)); err != nil {
				t.Errorf("write stand-in response: %v", err)
			}
		case "/api/ps":
			if _, err := w.Write(buildModelsJSON(maxModels + 1)); err != nil {
				t.Errorf("write stand-in response: %v", err)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	res := Fetch(context.Background(), server.URL, fixtureNow)
	if res.Status != catalog.StatusDegraded {
		t.Fatalf("Status = %v, want StatusDegraded", res.Status)
	}
	if res.Count != 1 || len(res.Records) != 1 || res.Records[0].ModelID != "local-m" {
		t.Fatalf("Records = %+v, want tags record preserved", res.Records)
	}
	if !strings.Contains(res.Detail, "/api/ps lists more than") {
		t.Fatalf("Detail = %q, want mention of '/api/ps lists more than'", res.Detail)
	}
}

func TestFetch_StampsInjectedNow(t *testing.T) {
	server := newServer(t, sampleTags, samplePS, http.StatusOK)
	defer server.Close()

	fixed := time.Date(2026, 10, 18, 12, 34, 56, 0, time.UTC)
	res := Fetch(context.Background(), server.URL, fixed)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail = %q, want ok", res.Status, res.Detail)
	}
	if len(res.Records) == 0 {
		t.Fatal("Records is empty, want at least one record")
	}
	for _, rec := range res.Records {
		if rec.Provenance.FetchedAt != fixed {
			t.Fatalf("FetchedAt = %v, want %v", rec.Provenance.FetchedAt, fixed)
		}
	}
}

// TestDataSync_DocBounds pins timeouts and caps to docs/data-sync.md ("Sources" table:
// ollama-local: 5 s, 8 MiB, 5,000 models).
func TestDataSync_DocBounds(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"requestTimeout", requestTimeout, 5 * time.Second},
		{"maxResponseBytes", maxResponseBytes, 8 << 20},
		{"MaxRecords", MaxRecords, 5000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}
