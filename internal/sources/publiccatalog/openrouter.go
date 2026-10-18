// Package publiccatalog reads two published, unauthenticated model catalogs:
// OpenRouter's public models API and LiteLLM's public model price map.
// Neither call carries a credential; both are plain GETs.
//
// Both URLs and shapes below were verified live on 2026-09-18 (not taken
// from memory), per the design doc's requirement to confirm this source
// before coding:
//
//   - OpenRouter: GET https://openrouter.ai/api/v1/models -> HTTP 200,
//     {"data":[{"id":"...","name":"...","context_length":262144,
//     "architecture":{"input_modalities":["text","image"],
//     "output_modalities":["text"]},
//     "pricing":{"prompt":"0.0000025","completion":"0.0000075"}, ...}]}.
//     "pricing" values are USD per token as decimal strings.
//
//   - LiteLLM price map: GET
//     https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
//     -> HTTP 200, a JSON object keyed by model id, e.g.
//     {"gpt-4":{"input_cost_per_token":0.00003,"output_cost_per_token":
//     0.00006,"max_input_tokens":8192,"max_output_tokens":4096,
//     "litellm_provider":"openai","mode":"chat"}, ...}. The single key
//     "sample_spec" documents the field names and is not a model; it is
//     skipped explicitly rather than treated as a broken record.
//
// OpenRouter and LiteLLM name models under incompatible id schemes (e.g.
// "openai/gpt-4" vs "gpt-4"), and slice 1 does not attempt to reconcile
// them -- that is catalog-merge/routing work the design doc marks out of
// scope. Each sub-fetch contributes its own records, tagged with its own
// provenance source ("public-catalog:openrouter" or
// "public-catalog:litellm-prices") so origin stays traceable even though
// the CLI reports both under the single "public-catalog" source name.
package publiccatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/tekmora/epitrop/catalog"
	"github.com/tekmora/epitrop/internal/sources/httpfetch"
)

// DefaultOpenRouterURL is the OpenRouter public models endpoint verified
// live on 2026-09-18.
const DefaultOpenRouterURL = "https://openrouter.ai/api/v1/models"

const (
	// maxResponseBytes bounds each catalog response read (HISS-02).
	maxResponseBytes = 16 << 20
	// MaxOpenRouterRecords bounds how many entries this sub-fetch turns into
	// records (HISS-02). Public catalog sub-sources intentionally stay below
	// 5000 each so all source caps sum below catalog.MaxSnapshotRecords.
	MaxOpenRouterRecords = 4999
	// maxModalities bounds each modality list copied into capabilities
	// (HISS-02).
	maxModalities = 16
	// requestTimeout bounds each HTTP round trip (HISS-02).
	requestTimeout = 20 * time.Second
)

type openRouterArchitecture struct {
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
}

type openRouterPricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

type openRouterEntry struct {
	ID            string                 `json:"id"`
	ContextLength int64                  `json:"context_length"`
	Architecture  openRouterArchitecture `json:"architecture"`
	Pricing       openRouterPricing      `json:"pricing"`
}

type openRouterResponse struct {
	Data []openRouterEntry `json:"data"`
}

type openRouterResult struct {
	Records  []catalog.Record
	Rejected int
	// FirstReject is the reason the first entry with an ID was rejected, so
	// the source detail names a cause, not only a count.
	FirstReject string
}

func fetchOpenRouterWithReport(ctx context.Context, url string, now time.Time) (openRouterResult, error) {
	body, err := httpfetch.Get(ctx, url, requestTimeout, maxResponseBytes)
	if err != nil {
		return openRouterResult{}, fmt.Errorf("openrouter: %w", err)
	}
	return fetchOpenRouterFromBytes(body, now)
}

// fetchOpenRouterFromBytes parses an OpenRouter models response. One
// malformed entry rejects only itself; a response in which every entry is
// rejected is refused, since that is an upstream schema change.
func fetchOpenRouterFromBytes(body []byte, now time.Time) (openRouterResult, error) {
	var parsed openRouterResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return openRouterResult{}, fmt.Errorf("openrouter: decode response: %w", err)
	}
	if len(parsed.Data) > MaxOpenRouterRecords {
		return openRouterResult{}, fmt.Errorf("openrouter: response lists more than %d models", MaxOpenRouterRecords)
	}
	if len(parsed.Data) == 0 {
		return openRouterResult{}, fmt.Errorf("openrouter: empty response data list")
	}

	fetchedAt := now.UTC()
	res := openRouterResult{Records: make([]catalog.Record, 0, len(parsed.Data))}
	for _, e := range parsed.Data {
		if e.ID == "" {
			res.Rejected++
			continue
		}
		rec, recErr := openRouterRecord(e, fetchedAt)
		if recErr != nil {
			// One malformed entry rejects only itself; it is counted and its
			// reason reported, and the rest of the catalog is kept.
			res.Rejected++
			if res.FirstReject == "" {
				res.FirstReject = recErr.Error()
			}
			continue
		}
		res.Records = append(res.Records, rec)
	}
	if len(res.Records) == 0 && res.Rejected > 0 {
		return openRouterResult{}, fmt.Errorf("openrouter: all %d entries were rejected; the upstream schema changed", res.Rejected)
	}
	return res, nil
}

func openRouterRecord(e openRouterEntry, fetchedAt time.Time) (catalog.Record, error) {
	rec := catalog.Record{
		ModelID:    e.ID,
		AccessPath: catalog.AccessAPI,
		Provenance: catalog.Provenance{
			Source:    "public-catalog:openrouter",
			FetchedAt: fetchedAt,
			Kind:      catalog.KindDeclared,
		},
		Absent: map[string]string{
			"limits": "OpenRouter's public models list does not report per-key rpm/tpm/daily caps",
		},
	}
	if e.ContextLength > 0 {
		cw := e.ContextLength
		rec.ContextWindow = &cw
	} else {
		rec.Absent["context_window"] = "OpenRouter public models list does not report a positive context_length"
	}
	if p, reason := perTokenToPerM(e.Pricing.Prompt); reason == "" {
		rec.PriceInPerM = &p
	} else {
		rec.Absent["price_in_per_m"] = reason
	}
	if p, reason := perTokenToPerM(e.Pricing.Completion); reason == "" {
		rec.PriceOutPerM = &p
	} else {
		rec.Absent["price_out_per_m"] = reason
	}
	var err error
	rec.Capabilities, err = appendModalities(rec.Capabilities, "in:", e.Architecture.InputModalities)
	if err != nil {
		return catalog.Record{}, fmt.Errorf("openrouter: %s input_modalities: %w", e.ID, err)
	}
	rec.Capabilities, err = appendModalities(rec.Capabilities, "out:", e.Architecture.OutputModalities)
	if err != nil {
		return catalog.Record{}, fmt.Errorf("openrouter: %s output_modalities: %w", e.ID, err)
	}
	return rec, nil
}

// perTokenToPerM converts a USD-per-token decimal string to USD-per-million-
// tokens. An empty or unparsable value is reported absent, never guessed.
func perTokenToPerM(raw string) (float64, string) {
	if raw == "" {
		return 0, "OpenRouter public models list does not report this price"
	}
	perToken, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, "OpenRouter public models list reports an unparsable price"
	}
	if raw == "-1" {
		return 0, "variable price"
	}
	if perToken < 0 {
		return 0, "OpenRouter public models list reports a negative price"
	}
	return perToken * 1_000_000, ""
}

func appendModalities(dst []string, prefix string, values []string) ([]string, error) {
	if len(values) > maxModalities {
		return nil, fmt.Errorf("lists more than %d entries", maxModalities)
	}
	for i := 0; i < len(values) && i < maxModalities; i++ {
		dst = append(dst, prefix+values[i])
	}
	return dst, nil
}
