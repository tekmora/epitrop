package publiccatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tekmora/epitrop/catalog"
	"github.com/tekmora/epitrop/internal/sources/httpfetch"
)

// renovate: datasource=git-refs depName=https://github.com/BerriAI/litellm branch=main
const liteLLMPriceMapRevision = "ad83e3d3c8376e5561b07a329a063bb5ac03dc9c"

// DefaultLiteLLMPriceMapURL is LiteLLM's pinned, unauthenticated model
// price/context map, checked 2026-10-09.
const DefaultLiteLLMPriceMapURL = "https://raw.githubusercontent.com/BerriAI/litellm/" + liteLLMPriceMapRevision + "/model_prices_and_context_window.json"

// litellmSampleSpecKey is the one key in the price map that documents the
// schema rather than naming a model; it is skipped, not parsed as a record.
const litellmSampleSpecKey = "sample_spec"

// MaxLiteLLMPriceRecords is the maximum number of records the LiteLLM price
// map sub-source can contribute to one snapshot.
const MaxLiteLLMPriceRecords = 4999

type litellmPriceEntry struct {
	InputCostPerToken  *float64 `json:"input_cost_per_token"`
	OutputCostPerToken *float64 `json:"output_cost_per_token"`
	MaxInputTokens     *int64   `json:"max_input_tokens"`
	MaxTokens          *int64   `json:"max_tokens"`
	LiteLLMProvider    string   `json:"litellm_provider"`
	Mode               string   `json:"mode"`
}

// fetchLiteLLMPrices fetches and parses LiteLLM's public price map.
//
// The map is decoded key-by-key rather than in one map[string]litellmPriceEntry
// shot: verified live on 2026-09-18, the map's own "sample_spec" key documents
// each field with a human-readable *string* ("max input tokens, if the
// provider specifies it...") in a field the model entries type as an int64.
// A single json.Unmarshal into a typed map aborts on that first mismatch and
// discards every real model with it. Decoding entry-by-entry keeps that one
// documentation key (and any one malformed model entry) from taking down
// every other model in the same response.
type liteLLMPriceResult struct {
	Records   []catalog.Record
	Malformed int
}

func fetchLiteLLMPricesWithReport(ctx context.Context, url string) (liteLLMPriceResult, error) {
	body, err := httpfetch.Get(ctx, url, requestTimeout, maxResponseBytes)
	if err != nil {
		return liteLLMPriceResult{}, fmt.Errorf("litellm-prices: %w", err)
	}
	return fetchLiteLLMPricesFromBytes(body)
}

// fetchLiteLLMPricesFromBytes parses a price map. One malformed entry is
// counted and skipped; a map in which every model entry is malformed is
// refused, since that is an upstream schema change, not one bad entry.
func fetchLiteLLMPricesFromBytes(body []byte) (liteLLMPriceResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return liteLLMPriceResult{}, fmt.Errorf("litellm-prices: decode response: %w", err)
	}
	if len(raw) > MaxLiteLLMPriceRecords {
		return liteLLMPriceResult{}, fmt.Errorf("litellm-prices: response lists more than %d models", MaxLiteLLMPriceRecords)
	}
	if len(raw) == 0 {
		return liteLLMPriceResult{}, fmt.Errorf("litellm-prices: empty response price map")
	}

	fetchedAt := time.Now().UTC()
	records := make([]catalog.Record, 0, len(raw))
	malformed := 0
	seenModels := 0
	for id, msg := range raw {
		record, seen, ok := parseLiteLLMPriceRecord(id, msg, fetchedAt)
		seenModels += seen
		if !ok {
			malformed += seen
			continue
		}
		records = append(records, record)
	}
	if len(records) == 0 && malformed > 0 {
		return liteLLMPriceResult{}, fmt.Errorf("litellm-prices: all %d model entries are malformed; the upstream schema changed", malformed)
	}
	if seenModels == 0 {
		return liteLLMPriceResult{}, fmt.Errorf("litellm-prices: empty response price map contains only sample_spec")
	}
	return liteLLMPriceResult{Records: records, Malformed: malformed}, nil
}

func parseLiteLLMPriceRecord(id string, msg json.RawMessage, fetchedAt time.Time) (catalog.Record, int, bool) {
	if id == litellmSampleSpecKey {
		return catalog.Record{}, 0, false
	}
	if id == "" {
		return catalog.Record{}, 1, false
	}
	var e litellmPriceEntry
	if err := json.Unmarshal(msg, &e); err != nil {
		return catalog.Record{}, 1, false
	}
	return litellmPriceRecord(id, e, fetchedAt), 1, true
}

func litellmPriceRecord(id string, e litellmPriceEntry, fetchedAt time.Time) catalog.Record {
	rec := catalog.Record{
		ModelID:    id,
		Provider:   e.LiteLLMProvider,
		AccessPath: catalog.AccessAPI,
		Provenance: catalog.Provenance{
			Source:    "public-catalog:litellm-prices",
			FetchedAt: fetchedAt,
			Kind:      catalog.KindDeclared,
		},
		Absent: map[string]string{
			"limits": "LiteLLM's public price map does not report per-key rpm/tpm/daily caps",
		},
	}
	if e.InputCostPerToken != nil {
		p := *e.InputCostPerToken * 1_000_000
		rec.PriceInPerM = &p
	} else {
		rec.Absent["price_in_per_m"] = "LiteLLM price map entry does not report input_cost_per_token"
	}
	if e.OutputCostPerToken != nil {
		p := *e.OutputCostPerToken * 1_000_000
		rec.PriceOutPerM = &p
	} else {
		rec.Absent["price_out_per_m"] = "LiteLLM price map entry does not report output_cost_per_token"
	}
	cw := contextWindowFrom(e)
	if cw != nil {
		rec.ContextWindow = cw
	} else {
		rec.Absent["context_window"] = "LiteLLM price map entry does not report max_input_tokens or max_tokens"
	}
	if e.Mode != "" {
		rec.Capabilities = []string{"mode:" + e.Mode}
	}
	return rec
}

// contextWindowFrom picks max_input_tokens when present, falling back to
// the legacy max_tokens field, matching LiteLLM's own documented fallback
// order (see the price map's "sample_spec" entry, verified live 2026-09-18).
func contextWindowFrom(e litellmPriceEntry) *int64 {
	if e.MaxInputTokens != nil {
		return e.MaxInputTokens
	}
	return e.MaxTokens
}
