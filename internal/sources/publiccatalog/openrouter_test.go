package publiccatalog

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
)

const sampleOpenRouter = `{"data":[{"id":"openai/gpt-4","name":"GPT-4","context_length":8192,"architecture":{"input_modalities":["text"],"output_modalities":["text"]},"pricing":{"prompt":"0.00003","completion":"0.00006"}}]}`

func TestOpenRouter_UpstreamContractDocumentPinned(t *testing.T) {
	body := mustReadFixture(t, "testdata/upstream/openrouter-models-openapi.yaml")
	for _, marker := range [][]byte{
		[]byte("openapi: 3.1.0"),
		[]byte("operationId: getModels"),
		[]byte("- pricing"),
		[]byte("- context_length"),
		[]byte("- architecture"),
	} {
		if !bytes.Contains(body, marker) {
			t.Fatalf("OpenRouter upstream contract missing marker %q", marker)
		}
	}
}

func TestFetchOpenRouter_UpstreamFixtureContract(t *testing.T) {
	body := mustReadFixture(t, "testdata/fixtures/openrouter-models-response.json")
	res, err := fetchOpenRouterFromBytes(body, fixtureNow)
	records := res.Records
	if err != nil {
		t.Fatalf("fetchOpenRouterFromBytes(valid fixture) = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}

	mutated := strings.Replace(string(body), `"id":"vendor-a/model-1",`, "", 1)
	if _, err := fetchOpenRouterFromBytes([]byte(mutated), fixtureNow); err == nil {
		t.Fatal("fetchOpenRouterFromBytes(mutated missing id) = nil error, want refusal")
	}
}

func TestFetchOpenRouter_Positive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleOpenRouter)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer server.Close()

	res, err := fetchOpenRouterWithReport(context.Background(), server.URL, fixtureNow)
	if err != nil {
		t.Fatalf("fetchOpenRouterWithReport() = %v", err)
	}
	if res.Rejected != 0 {
		t.Fatalf("res.Rejected = %d, want 0", res.Rejected)
	}
	records := res.Records
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.ModelID != "openai/gpt-4" {
		t.Fatalf("ModelID = %q", rec.ModelID)
	}
	if rec.PriceInPerM == nil || *rec.PriceInPerM != 30 {
		t.Fatalf("PriceInPerM = %v, want 30 (0.00003 * 1e6)", rec.PriceInPerM)
	}
	if rec.PriceOutPerM == nil || *rec.PriceOutPerM != 60 {
		t.Fatalf("PriceOutPerM = %v, want 60", rec.PriceOutPerM)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func mustReadFixture(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return body
}

func TestFetchOpenRouter_Negative(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		if _, err := fetchOpenRouterWithReport(context.Background(), "http://127.0.0.1:1", fixtureNow); err == nil {
			t.Fatal("fetchOpenRouterWithReport() = nil error, want failure for an unreachable host")
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{not json")) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
		}))
		defer server.Close()
		if _, err := fetchOpenRouterWithReport(context.Background(), server.URL, fixtureNow); err == nil {
			t.Fatal("fetchOpenRouterWithReport() = nil error, want failure for a malformed body")
		}
	})
}

// TestFetchOpenRouter_Boundary covers a pricing value the parser cannot
// read: it must be dropped as absent, not crash or fabricate a price.
func TestFetchOpenRouter_Boundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"vendor-a/model-1","pricing":{"prompt":"not-a-number","completion":""}}]}`)) //nolint:errcheck // test httptest server response; a write failure here would fail the test's own HTTP round trip, not silently corrupt anything
	}))
	defer server.Close()

	res, err := fetchOpenRouterWithReport(context.Background(), server.URL, fixtureNow)
	if err != nil {
		t.Fatalf("fetchOpenRouterWithReport() = %v", err)
	}
	records := res.Records
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].PriceInPerM != nil || records[0].PriceOutPerM != nil {
		t.Fatalf("PriceInPerM/PriceOutPerM = %v/%v, want both nil for unparsable pricing", records[0].PriceInPerM, records[0].PriceOutPerM)
	}
	if records[0].Absent["price_in_per_m"] == "" || records[0].Absent["price_out_per_m"] == "" {
		t.Fatalf("Absent = %+v, want reasons for both missing prices", records[0].Absent)
	}
	if records[0].Absent["context_window"] == "" {
		t.Fatalf("Absent = %+v, want a reason for missing context_window", records[0].Absent)
	}
}

func TestFetchOpenRouter_VariablePriceIsAbsent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"vendor-a/model-1","context_length":1024,"pricing":{"prompt":"-1","completion":"-1"}}]}`)) //nolint:errcheck // test server response
	}))
	defer server.Close()

	res, err := fetchOpenRouterWithReport(context.Background(), server.URL, fixtureNow)
	if err != nil {
		t.Fatalf("fetchOpenRouterWithReport() = %v", err)
	}
	records := res.Records
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].PriceInPerM != nil || records[0].PriceOutPerM != nil {
		t.Fatalf("prices = %v/%v, want nil for variable price", records[0].PriceInPerM, records[0].PriceOutPerM)
	}
	if records[0].Absent["price_in_per_m"] != "variable price" {
		t.Fatalf("Absent[price_in_per_m] = %q, want variable price", records[0].Absent["price_in_per_m"])
	}
	if records[0].Absent["price_out_per_m"] != "variable price" {
		t.Fatalf("Absent[price_out_per_m] = %q, want variable price", records[0].Absent["price_out_per_m"])
	}
}

func TestOpenRouterRecord_AbsentReasons(t *testing.T) {
	rec, err := openRouterRecord(openRouterEntry{
		ID: "vendor-a/model-1",
		Pricing: openRouterPricing{
			Prompt:     "not-a-number",
			Completion: "",
		},
	}, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("openRouterRecord() = %v", err)
	}
	if rec.Absent["context_window"] == "" {
		t.Fatalf("Absent = %+v, want context_window reason", rec.Absent)
	}
	if rec.Absent["price_in_per_m"] == "" || rec.Absent["price_out_per_m"] == "" {
		t.Fatalf("Absent = %+v, want price reasons", rec.Absent)
	}
}

func TestOpenRouterRecord_VariablePriceIsAbsent(t *testing.T) {
	rec, err := openRouterRecord(openRouterEntry{
		ID:            "vendor-a/model-1",
		ContextLength: 1024,
		Pricing: openRouterPricing{
			Prompt:     "-1",
			Completion: "-1",
		},
	}, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("openRouterRecord() = %v", err)
	}
	if rec.PriceInPerM != nil || rec.PriceOutPerM != nil {
		t.Fatalf("prices = %v/%v, want nil", rec.PriceInPerM, rec.PriceOutPerM)
	}
	if rec.Absent["price_in_per_m"] != "variable price" || rec.Absent["price_out_per_m"] != "variable price" {
		t.Fatalf("Absent = %+v, want variable price reasons", rec.Absent)
	}
}

func TestFetchOpenRouter_OversizedModalitiesRejectOnlyThatEntry(t *testing.T) {
	mods := `"text","image","audio","video","file","a6","a7","a8","a9","a10","a11","a12","a13","a14","a15","a16","a17"`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[` + //nolint:errcheck // test server response
			`{"id":"vendor-a/model-1","architecture":{"input_modalities":[` + mods + `]}},` +
			`{"id":"vendor-a/model-2","context_length":1024,"architecture":{"input_modalities":["text"]}}]}`))
	}))
	defer server.Close()

	res, err := fetchOpenRouterWithReport(context.Background(), server.URL, fixtureNow)
	if err != nil {
		t.Fatalf("fetchOpenRouterWithReport() = %v, want the oversized entry rejected and the rest kept", err)
	}
	if len(res.Records) != 1 || res.Records[0].ModelID != "vendor-a/model-2" {
		t.Fatalf("records = %+v, want only vendor-a/model-2", res.Records)
	}
	if res.Rejected != 1 {
		t.Fatalf("Rejected = %d, want 1", res.Rejected)
	}
}

func TestFetchOpenRouter_ResponseCap(t *testing.T) {
	serve := func(size int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			prefix := `{"data":[{"id":"m"}]}`
			body := []byte(prefix + strings.Repeat(" ", size-len(prefix)))
			if _, err := w.Write(body); err != nil {
				t.Errorf("write stand-in response: %v", err)
			}
		}))
	}

	t.Run("exactly at cap", func(t *testing.T) {
		server := serve(maxResponseBytes)
		defer server.Close()
		res, err := fetchOpenRouterWithReport(context.Background(), server.URL, fixtureNow)
		if err != nil {
			t.Fatalf("fetchOpenRouterWithReport() = %v, want success at cap", err)
		}
		records := res.Records
		if len(records) != 1 {
			t.Fatalf("records count = %d, want 1", len(records))
		}
	})

	t.Run("one byte over cap", func(t *testing.T) {
		server := serve(maxResponseBytes + 1)
		defer server.Close()
		_, err := fetchOpenRouterWithReport(context.Background(), server.URL, fixtureNow)
		if !errors.Is(err, client.ErrBodyTooLarge) {
			t.Fatalf("fetchOpenRouterWithReport() err = %v, want ErrBodyTooLarge", err)
		}
	})
}

func buildOpenRouterDataJSON(count int) []byte {
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"m"}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func TestFetchOpenRouter_RecordCap(t *testing.T) {
	t.Run("4999 pass", func(t *testing.T) {
		res, err := fetchOpenRouterFromBytes(buildOpenRouterDataJSON(MaxOpenRouterRecords), fixtureNow)
		if err != nil {
			t.Fatalf("fetchOpenRouterFromBytes() = %v, want nil error", err)
		}
		if len(res.Records) != MaxOpenRouterRecords {
			t.Fatalf("len(res.Records) = %d, want %d", len(res.Records), MaxOpenRouterRecords)
		}
	})

	t.Run("5000 fail", func(t *testing.T) {
		_, err := fetchOpenRouterFromBytes(buildOpenRouterDataJSON(MaxOpenRouterRecords+1), fixtureNow)
		if err == nil || !strings.Contains(err.Error(), "lists more than") {
			t.Fatalf("fetchOpenRouterFromBytes() err = %v, want 'lists more than'", err)
		}
	})
}
