// Package ollamalocal lists models installed on a local Ollama daemon via
// GET /api/tags, and marks which of them are currently loaded via
// GET /api/ps.
//
// Verified live against http://localhost:11434 on this workstation,
// 2026-09-18: both endpoints returned HTTP 200. /api/tags carries
// {"models":[{"name":...,"model":...,"modified_at":...,"size":...,
// "details":{"parameter_size":...,"quantization_level":...,
// "context_length":...},"capabilities":[...]}]}. /api/ps carries the same
// per-model shape for loaded models plus "expires_at" and "size_vram".
//
// This intentionally does not import the internal/router package of
// cordanaLLM/praetor, which talks to the same two endpoints for routing
// decisions. That package is planned to read the Epitrop catalog instead
// (one model catalog, #29); until then the two GETs exist in both places.
package ollamalocal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tekmora/epitrop/catalog"
	"github.com/tekmora/epitrop/internal/sources/httpfetch"
)

// SourceName identifies this source in Snapshot.SourceRuns and CLI flags.
const SourceName = "ollama-local"

const (
	// MaxRecords is the maximum number of records this source can contribute
	// to one snapshot.
	MaxRecords = 5000
	// maxResponseBytes bounds each endpoint's response read (HISS-02).
	maxResponseBytes = 8 << 20
	// maxModels bounds how many entries Fetch will turn into records.
	maxModels = MaxRecords
	// requestTimeout bounds each HTTP round trip (HISS-02).
	requestTimeout = 5 * time.Second
)

// Result is one Fetch attempt's outcome.
type Result struct {
	Records []catalog.Record
	Status  catalog.Status
	Count   int
	Detail  string
}

type modelDetails struct {
	ParameterSize     string `json:"parameter_size"`
	QuantizationLevel string `json:"quantization_level"`
	ContextLength     int64  `json:"context_length"`
}

// tagEntry is hand-written from Ollama docs/api.md at
// c2b7368d4156656ddb9a23b43f721a841b9c23e0, checked 2026-10-09; no published
// JSON schema was found for /api/tags.
type tagEntry struct {
	Name         string       `json:"name"`
	Details      modelDetails `json:"details"`
	Capabilities []string     `json:"capabilities"`
}

type tagsResponse struct {
	Models []tagEntry `json:"models"`
}

// psEntry is hand-written from Ollama docs/api.md at
// c2b7368d4156656ddb9a23b43f721a841b9c23e0, checked 2026-10-09; no published
// JSON schema was found for /api/ps.
type psEntry struct {
	Name string `json:"name"`
}

type psResponse struct {
	Models []psEntry `json:"models"`
}

// Fetch lists installed models at endpoint+"/api/tags" and annotates which
// are currently loaded via endpoint+"/api/ps". A /api/ps failure does not
// fail the whole source: installed-model data still stands on its own, so
// Fetch downgrades that one failure into a note rather than discarding
// everything /api/tags already measured.
func Fetch(ctx context.Context, endpoint string, now time.Time) Result {
	tags, err := getTags(ctx, endpoint)
	if err != nil {
		return Result{Status: catalog.StatusFail, Detail: err.Error()}
	}
	if len(tags.Models) == 0 {
		return Result{Status: catalog.StatusSkip, Detail: fmt.Sprintf("%s/api/tags lists no installed models", endpoint)}
	}

	loaded, psErr := loadedModelNames(ctx, endpoint)
	loadedKnown := psErr == nil

	fetchedAt := now.UTC()
	records := make([]catalog.Record, 0, len(tags.Models))
	for _, m := range tags.Models {
		records = append(records, toRecord(m, loaded[m.Name], loadedKnown, fetchedAt))
	}

	detail := ""
	if psErr != nil {
		detail = fmt.Sprintf("api/ps unavailable, loaded-state omitted: %v", psErr)
		return Result{Records: records, Status: catalog.StatusDegraded, Count: len(records), Detail: detail}
	}
	return Result{Records: records, Status: catalog.StatusOK, Count: len(records), Detail: detail}
}

func getTags(ctx context.Context, endpoint string) (tagsResponse, error) {
	var parsed tagsResponse
	body, err := httpfetch.Get(ctx, endpoint+"/api/tags", requestTimeout, maxResponseBytes)
	if err != nil {
		return parsed, fmt.Errorf("ollama-local: %w", err)
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return parsed, fmt.Errorf("ollama-local: decode /api/tags: %w", err)
	}
	if len(parsed.Models) > maxModels {
		return parsed, fmt.Errorf("ollama-local: /api/tags lists more than %d models", maxModels)
	}
	// One model without a name becomes a record sync rejects with a reason;
	// a list in which no model carries a name is an upstream schema change.
	missing := 0
	for _, m := range parsed.Models {
		if m.Name == "" {
			missing++
		}
	}
	if missing > 0 && missing == len(parsed.Models) {
		return parsed, fmt.Errorf("ollama-local: none of the %d /api/tags models carries a name; the upstream schema changed", missing)
	}
	return parsed, nil
}

// loadedModelNames returns the set of model names /api/ps reports as
// currently loaded. A failure here is returned to the caller rather than
// wrapped into a Result, so Fetch can decide it is non-fatal.
func loadedModelNames(ctx context.Context, endpoint string) (map[string]bool, error) {
	body, err := httpfetch.Get(ctx, endpoint+"/api/ps", requestTimeout, maxResponseBytes)
	if err != nil {
		return nil, err
	}
	var parsed psResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode /api/ps: %w", err)
	}
	if len(parsed.Models) > maxModels {
		return nil, fmt.Errorf("/api/ps lists more than %d models", maxModels)
	}
	loaded := make(map[string]bool, len(parsed.Models))
	for i, m := range parsed.Models {
		if m.Name == "" {
			return nil, fmt.Errorf("/api/ps models[%d].name is required", i)
		}
		loaded[m.Name] = true
	}
	return loaded, nil
}

func toRecord(m tagEntry, loaded, loadedKnown bool, fetchedAt time.Time) catalog.Record {
	rec := catalog.Record{
		ModelID:      m.Name,
		Provider:     "ollama",
		AccessPath:   catalog.AccessLocal,
		Capabilities: append([]string{}, m.Capabilities...),
		Provenance: catalog.Provenance{
			Source:    SourceName,
			FetchedAt: fetchedAt,
			Kind:      catalog.KindMeasured,
		},
		Absent: map[string]string{
			"price_in_per_m":  "local inference, no metered price",
			"price_out_per_m": "local inference, no metered price",
			"limits":          "local daemon reports no rpm/tpm/daily caps",
		},
	}
	if m.Details.ContextLength > 0 {
		cw := m.Details.ContextLength
		rec.ContextWindow = &cw
	} else {
		rec.Absent["context_window"] = "local daemon did not report a positive context_length"
	}
	if m.Details.ParameterSize != "" {
		rec.Capabilities = append(rec.Capabilities, "params:"+m.Details.ParameterSize)
	}
	if m.Details.QuantizationLevel != "" {
		rec.Capabilities = append(rec.Capabilities, "quant:"+strings.ToLower(m.Details.QuantizationLevel))
	}
	if loaded {
		rec.Capabilities = append(rec.Capabilities, "loaded")
	}
	if !loadedKnown {
		rec.Absent["capabilities.loaded"] = "/api/ps unavailable, loaded state unknown"
	}
	return rec
}
