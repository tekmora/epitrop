package publiccatalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

var fixtureNow = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

func TestFetch_Positive(t *testing.T) {
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleOpenRouter)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer or.Close()
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleLiteLLMPrices)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer llm.Close()

	res := Fetch(context.Background(), or.URL, llm.URL, fixtureNow)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail = %q, want ok", res.Status, res.Detail)
	}
	if res.Count != 2 { // 1 openrouter + 1 litellm-prices
		t.Fatalf("Count = %d, want 2", res.Count)
	}
	if !strings.HasPrefix(res.Detail, "openrouter: ok (1);") {
		t.Fatalf("Detail = %q, want it to start with %q", res.Detail, "openrouter: ok (1);")
	}
}

// TestFetch_Negative confirms one sub-source's total failure does not hide
// the other sub-source's records, and both are named in Detail.
func TestFetch_Negative(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleLiteLLMPrices)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer llm.Close()

	res := Fetch(context.Background(), "http://127.0.0.1:1", llm.URL, fixtureNow)
	if res.Status != catalog.StatusDegraded {
		t.Fatalf("Status = %v, detail = %q, want degraded (litellm-prices alone succeeded)", res.Status, res.Detail)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d, want 1", res.Count)
	}
	if !strings.Contains(res.Detail, "openrouter: fail") {
		t.Fatalf("Detail = %q, want it to name the openrouter failure", res.Detail)
	}
}

// TestFetch_Boundary covers both sub-sources failing: Fetch must report
// StatusFail rather than a false StatusOK with zero records.
func TestFetch_Boundary(t *testing.T) {
	res := Fetch(context.Background(), "http://127.0.0.1:1", "http://127.0.0.1:1", fixtureNow)
	if res.Status != catalog.StatusFail {
		t.Fatalf("Status = %v, want fail when both sub-sources fail", res.Status)
	}
	if res.Count != 0 || len(res.Records) != 0 {
		t.Fatalf("got %d records, want 0", res.Count)
	}
}

func TestFetch_ReportsMalformedLiteLLMEntries(t *testing.T) {
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`)) //nolint:errcheck // test server response
	}))
	defer or.Close()
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"vendor-a/model-1":{"input_cost_per_token":0.00001},"vendor-b/model-2":{"max_input_tokens":"broken"}}`)) //nolint:errcheck // test server response
	}))
	defer llm.Close()

	res := Fetch(context.Background(), or.URL, llm.URL, fixtureNow)
	if res.Status != catalog.StatusDegraded {
		t.Fatalf("Status = %v, detail = %q, want degraded", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "malformed=1") {
		t.Fatalf("Detail = %q, want malformed=1", res.Detail)
	}
}

func TestFetch_EmptyPublicCatalogResponsesFail(t *testing.T) {
	t.Run("openrouter data list", func(t *testing.T) {
		or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":[]}`)) //nolint:errcheck // test server response
		}))
		defer or.Close()
		llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(sampleLiteLLMPrices)) //nolint:errcheck // test server response
		}))
		defer llm.Close()

		res := Fetch(context.Background(), or.URL, llm.URL, fixtureNow)
		if res.Status != catalog.StatusDegraded {
			t.Fatalf("Status = %v, detail = %q, want degraded", res.Status, res.Detail)
		}
		if !strings.Contains(res.Detail, "openrouter: empty response") {
			t.Fatalf("Detail = %q, want openrouter empty response", res.Detail)
		}
	})

	t.Run("litellm price map only sample_spec", func(t *testing.T) {
		or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(sampleOpenRouter)) //nolint:errcheck // test server response
		}))
		defer or.Close()
		llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"sample_spec":{"litellm_provider":"doc"}}`)) //nolint:errcheck // test server response
		}))
		defer llm.Close()

		res := Fetch(context.Background(), or.URL, llm.URL, fixtureNow)
		if res.Status != catalog.StatusDegraded {
			t.Fatalf("Status = %v, detail = %q, want degraded", res.Status, res.Detail)
		}
		if !strings.Contains(res.Detail, "litellm-prices: empty response") {
			t.Fatalf("Detail = %q, want litellm-prices empty response", res.Detail)
		}
	})
}

func TestLiteLLMDetailReportsMalformedEntries(t *testing.T) {
	got := liteLLMDetail(liteLLMPriceResult{
		Records:   make([]catalog.Record, 1),
		Malformed: 1,
	})
	if !strings.Contains(got, "malformed=1") {
		t.Fatalf("liteLLMDetail() = %q, want malformed=1", got)
	}
}

// TestDataSync_DocBounds pins timeouts and caps to docs/data-sync.md ("Sources" table:
// public-catalog: 20 s, 16 MiB, 4,999 records each).
func TestDataSync_DocBounds(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"requestTimeout", requestTimeout, 20 * time.Second},
		{"maxResponseBytes", maxResponseBytes, 16 << 20},
		{"MaxOpenRouterRecords", MaxOpenRouterRecords, 4999},
		{"MaxLiteLLMPriceRecords", MaxLiteLLMPriceRecords, 4999},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestFetch_ReportsRejectedOpenRouterRecords(t *testing.T) {
	payloadNoID := `{"data":[{"id":"openai/gpt-4","context_length":8192},{"id":""}]}`
	orNoID := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(payloadNoID)); err != nil {
			t.Errorf("write stand-in response: %v", err)
		}
	}))
	defer orNoID.Close()

	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(sampleLiteLLMPrices)); err != nil {
			t.Errorf("write stand-in response: %v", err)
		}
	}))
	defer llm.Close()

	res := Fetch(context.Background(), orNoID.URL, llm.URL, fixtureNow)
	if res.Status != catalog.StatusDegraded {
		t.Fatalf("Status = %v, want degraded", res.Status)
	}
	if res.Count != 2 {
		t.Fatalf("Count = %d, want 2", res.Count)
	}
	wantDetail := "openrouter: ok (1, rejected=1)"
	if !strings.Contains(res.Detail, wantDetail) {
		t.Fatalf("Detail = %q, want to contain %q", res.Detail, wantDetail)
	}

	mods := `"text","image","audio","video","file","a6","a7","a8","a9","a10","a11","a12","a13","a14","a15","a16","a17"`
	payloadWithErr := `{"data":[{"id":"openai/gpt-4","context_length":8192},{"id":"bad/model","architecture":{"input_modalities":[` + mods + `]}}]}`
	orWithErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(payloadWithErr)); err != nil {
			t.Errorf("write stand-in response: %v", err)
		}
	}))
	defer orWithErr.Close()

	res2 := Fetch(context.Background(), orWithErr.URL, llm.URL, fixtureNow)
	if res2.Status != catalog.StatusDegraded {
		t.Fatalf("Status = %v, want degraded", res2.Status)
	}
	if res2.Count != 2 {
		t.Fatalf("Count = %d, want 2", res2.Count)
	}
	wantDetail2 := "openrouter: ok (1, rejected=1, first: openrouter: bad/model input_modalities: lists more than 16 entries)"
	if !strings.Contains(res2.Detail, wantDetail2) {
		t.Fatalf("Detail = %q, want to contain %q", res2.Detail, wantDetail2)
	}
}

func TestFetch_StampsInjectedNow(t *testing.T) {
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleOpenRouter)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer or.Close()
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleLiteLLMPrices)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer llm.Close()

	fixed := time.Date(2026, 10, 18, 12, 34, 56, 0, time.UTC)
	res := Fetch(context.Background(), or.URL, llm.URL, fixed)
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
