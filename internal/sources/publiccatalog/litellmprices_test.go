package publiccatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golusoris/golusoris/httpx/client"
)

// sampleLiteLLMPrices mirrors what GET on the real price map returned live
// on 2026-09-18: sample_spec documents max_input_tokens as a *string*
// ("max input tokens, if the provider specifies it...") in a field every
// real model entry types as a number. A naive single-shot decode into
// map[string]litellmPriceEntry aborts on that mismatch before it reaches
// gpt-4 -- this fixture reproduces exactly that shape.
const sampleLiteLLMPrices = `{
  "sample_spec": {"input_cost_per_token": 0.0, "max_input_tokens": "max input tokens, if the provider specifies it. if not default to max_tokens", "litellm_provider": "one of https://docs.litellm.ai/docs/providers"},
  "gpt-4": {"input_cost_per_token": 0.00003, "output_cost_per_token": 0.00006, "max_input_tokens": 8192, "litellm_provider": "openai", "mode": "chat"}
}`

func TestLiteLLMPrices_UpstreamContractDocumentPinned(t *testing.T) {
	body := mustReadFixture(t, "testdata/upstream/litellm-sample-spec.json")
	for _, marker := range [][]byte{
		[]byte(`"sample_spec"`),
		[]byte(`"litellm_provider"`),
		[]byte(`"max_input_tokens"`),
		[]byte(`"input_cost_per_token"`),
	} {
		if !bytes.Contains(body, marker) {
			t.Fatalf("LiteLLM upstream contract missing marker %q", marker)
		}
	}
}

func TestLiteLLMPriceMapRevisionMatchesManifest(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "upstream-schemas.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Sources []struct {
			ID             string `json:"id"`
			PinnedRevision string `json:"pinned_revision"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	for _, source := range manifest.Sources {
		if source.ID != "litellm-price-map" {
			continue
		}
		if source.PinnedRevision != liteLLMPriceMapRevision {
			t.Fatalf("manifest LiteLLM revision = %q, const = %q", source.PinnedRevision, liteLLMPriceMapRevision)
		}
		return
	}
	t.Fatal("litellm-price-map source missing from manifest")
}

func TestFetchLiteLLMPrices_UpstreamFixtureContract(t *testing.T) {
	body := mustReadFixture(t, "testdata/fixtures/litellm-price-map.json")
	res, err := fetchLiteLLMPricesFromBytes(body, fixtureNow)
	records := res.Records
	if err != nil {
		t.Fatalf("fetchLiteLLMPricesFromBytes(valid fixture) = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}

	mutated := strings.Replace(string(body), `"max_input_tokens":8192`, `"max_input_tokens":"8192"`, 1)
	if _, err := fetchLiteLLMPricesFromBytes([]byte(mutated), fixtureNow); err == nil {
		t.Fatal("fetchLiteLLMPricesFromBytes(mutated retyped max_input_tokens) = nil error, want refusal")
	}
}

func TestFetchLiteLLMPrices_Positive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleLiteLLMPrices)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer server.Close()

	res, err := fetchLiteLLMPricesWithReport(context.Background(), server.URL, fixtureNow)
	if err != nil {
		t.Fatalf("fetchLiteLLMPricesWithReport() = %v", err)
	}
	if res.Malformed != 0 {
		t.Fatalf("res.Malformed = %d, want 0", res.Malformed)
	}
	records := res.Records
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1 (sample_spec skipped)", len(records))
	}
	rec := records[0]
	if rec.ModelID != "gpt-4" {
		t.Fatalf("ModelID = %q, want gpt-4", rec.ModelID)
	}
	if rec.PriceInPerM == nil || *rec.PriceInPerM != 30 {
		t.Fatalf("PriceInPerM = %v, want 30", rec.PriceInPerM)
	}
	if rec.ContextWindow == nil || *rec.ContextWindow != 8192 {
		t.Fatalf("ContextWindow = %v, want 8192", rec.ContextWindow)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestFetchLiteLLMPrices_Negative(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		if _, err := fetchLiteLLMPricesWithReport(context.Background(), "http://127.0.0.1:1", fixtureNow); err == nil {
			t.Fatal("fetchLiteLLMPricesWithReport() = nil error, want failure for an unreachable host")
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("[]")) //nolint:errcheck // an array, not the expected object; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		}))
		defer server.Close()
		if _, err := fetchLiteLLMPricesWithReport(context.Background(), server.URL, fixtureNow); err == nil {
			t.Fatal("fetchLiteLLMPricesWithReport() = nil error, want failure for a body that is not an object")
		}
	})
}

// TestFetchLiteLLMPrices_OneMalformedEntryRefused confirms a single model
// entry with a field the parser cannot decode fails the price map instead of
// silently dropping that model.
// TestFetchLiteLLMPrices_AllMalformedRefused: one malformed entry is counted
// and the rest kept (TestFetch_ReportsMalformedLiteLLMEntries), but a map in
// which every model entry is malformed is an upstream schema change and is
// refused rather than returned empty.
func TestFetchLiteLLMPrices_AllMalformedRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"vendor-a/model-1": {"max_input_tokens": "not a number"}, "vendor-a/model-2": {"input_cost_per_token": "0.1"}}`)) //nolint:errcheck // test httptest server response
	}))
	defer server.Close()

	if _, err := fetchLiteLLMPricesWithReport(context.Background(), server.URL, fixtureNow); err == nil {
		t.Fatal("fetchLiteLLMPricesWithReport() = nil error, want refusal when every model entry is malformed")
	}
}

// TestFetchLiteLLMPrices_Boundary covers max_input_tokens vs the legacy
// max_tokens fallback: when max_input_tokens is absent, max_tokens must be
// used instead, matching the price map's own documented fallback order.
func TestFetchLiteLLMPrices_Boundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"legacy-model": {"max_tokens": 4096, "litellm_provider": "openai"}}`)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer server.Close()

	res, err := fetchLiteLLMPricesWithReport(context.Background(), server.URL, fixtureNow)
	if err != nil {
		t.Fatalf("fetchLiteLLMPricesWithReport() = %v", err)
	}
	if res.Malformed != 0 {
		t.Fatalf("res.Malformed = %d, want 0", res.Malformed)
	}
	records := res.Records
	if len(records) != 1 || records[0].ContextWindow == nil || *records[0].ContextWindow != 4096 {
		t.Fatalf("records = %+v, want one record with ContextWindow=4096 from the max_tokens fallback", records)
	}
}

func TestFetchLiteLLMPrices_ResponseCap(t *testing.T) {
	serve := func(size int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			prefix := `{"m":{"litellm_provider":"openai"}}`
			body := []byte(prefix + strings.Repeat(" ", size-len(prefix)))
			if _, err := w.Write(body); err != nil {
				t.Errorf("write stand-in response: %v", err)
			}
		}))
	}

	t.Run("exactly at cap", func(t *testing.T) {
		server := serve(maxResponseBytes)
		defer server.Close()
		res, err := fetchLiteLLMPricesWithReport(context.Background(), server.URL, fixtureNow)
		if err != nil {
			t.Fatalf("fetchLiteLLMPricesWithReport() = %v, want success at cap", err)
		}
		if res.Malformed != 0 {
			t.Fatalf("res.Malformed = %d, want 0", res.Malformed)
		}
		records := res.Records
		if len(records) != 1 {
			t.Fatalf("records count = %d, want 1", len(records))
		}
	})

	t.Run("one byte over cap", func(t *testing.T) {
		server := serve(maxResponseBytes + 1)
		defer server.Close()
		_, err := fetchLiteLLMPricesWithReport(context.Background(), server.URL, fixtureNow)
		if !errors.Is(err, client.ErrBodyTooLarge) {
			t.Fatalf("fetchLiteLLMPricesWithReport() err = %v, want ErrBodyTooLarge", err)
		}
	})
}

func buildLiteLLMPriceMapJSON(count int) []byte {
	var b strings.Builder
	b.WriteString(`{`)
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"m%d":{"litellm_provider":"openai"}`, i)
	}
	b.WriteString(`}`)
	return []byte(b.String())
}

func TestFetchLiteLLMPrices_RecordCap(t *testing.T) {
	t.Run("4999 pass", func(t *testing.T) {
		res, err := fetchLiteLLMPricesFromBytes(buildLiteLLMPriceMapJSON(MaxLiteLLMPriceRecords), fixtureNow)
		if err != nil {
			t.Fatalf("fetchLiteLLMPricesFromBytes() = %v, want nil error", err)
		}
		if len(res.Records) != MaxLiteLLMPriceRecords {
			t.Fatalf("len(res.Records) = %d, want %d", len(res.Records), MaxLiteLLMPriceRecords)
		}
	})

	t.Run("5000 fail", func(t *testing.T) {
		_, err := fetchLiteLLMPricesFromBytes(buildLiteLLMPriceMapJSON(MaxLiteLLMPriceRecords+1), fixtureNow)
		if err == nil || !strings.Contains(err.Error(), "lists more than") {
			t.Fatalf("fetchLiteLLMPricesFromBytes() err = %v, want 'lists more than'", err)
		}
	})
}
