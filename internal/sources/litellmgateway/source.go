// Package litellmgateway lists models a LiteLLM gateway exposes to the
// caller's token via GET /v1/models.
//
// Verified live against a LiteLLM gateway on 2026-09-18: HTTP 200 for
// GET /v1/models; /model/info returned 403 for an agent token, so this
// source cannot see price/context metadata for gateway models in slice 1
// (see design doc, sources table) -- every record it produces says so
// via Record.Absent.
//
// The bearer token is read from a file and used only in the Authorization
// header; it is never logged, wrapped into an error, or otherwise surfaced.
package litellmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	gerrors "github.com/golusoris/golusoris/core/errors"
	"github.com/golusoris/golusoris/httpx/client"

	"github.com/tekmora/epitrop/catalog"
	"github.com/tekmora/epitrop/internal/sources/httpfetch"
)

// SourceName identifies this source in Snapshot.SourceRuns and CLI flags.
const SourceName = "litellm-gateway"

const (
	// MaxRecords is the maximum number of records this source can contribute
	// to one snapshot.
	MaxRecords = 5000
	// maxTokenFileBytes bounds the token file read (HISS-02).
	maxTokenFileBytes = 4096
	// maxResponseBytes bounds the /v1/models response read (HISS-02).
	maxResponseBytes = 8 << 20
	// maxModels bounds how many entries Fetch will turn into records.
	maxModels = MaxRecords
	// requestTimeout bounds the HTTP round trip (HISS-02).
	requestTimeout = 15 * time.Second
)

// Result is one Fetch attempt's outcome.
type Result struct {
	Records []catalog.Record
	Status  catalog.Status
	Count   int
	Detail  string
}

// Fetch reads the bearer token from tokenFile and lists models at
// baseURL+"/v1/models". Any failure -- unreadable token file, network
// error, non-200 response, malformed body -- is reported as StatusFail with
// a detail string that never contains the token.
func Fetch(ctx context.Context, baseURL, tokenFile string, now time.Time) Result {
	token, err := ReadToken(tokenFile)
	if err != nil {
		return Result{Status: catalog.StatusFail, Detail: err.Error()}
	}
	if token == "" {
		return Result{Status: catalog.StatusFail, Detail: fmt.Sprintf("token file %s is empty", tokenFile)}
	}

	models, err := listModels(ctx, baseURL, token)
	if err != nil {
		return Result{Status: catalog.StatusFail, Detail: err.Error()}
	}
	if len(models) == 0 {
		return Result{Status: catalog.StatusFail, Detail: fmt.Sprintf("empty response: %s/v1/models returned no models", baseURL)}
	}

	fetchedAt := now.UTC()
	records := make([]catalog.Record, 0, len(models))
	for _, m := range models {
		records = append(records, toRecord(m, fetchedAt))
	}
	return Result{Records: records, Status: catalog.StatusOK, Count: len(records)}
}

// ReadToken reads and trims tokenFile. Errors name the path, never contents.
func ReadToken(tokenFile string) (token string, err error) {
	// #nosec G304 -- tokenFile is a CLI flag the operator supplies directly, the
	// same trust level as any other path the operator passes.
	f, err := os.Open(tokenFile)
	if err != nil {
		return "", fmt.Errorf("litellm-gateway: open token file %s: %w", tokenFile, err)
	}
	defer gerrors.CloseJoin(f, &err, "litellm-gateway: close token file "+tokenFile)

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("litellm-gateway: stat token file %s: %w", tokenFile, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("litellm-gateway: token file %s is not a regular file", tokenFile)
	}
	if info.Size() > maxTokenFileBytes {
		return "", fmt.Errorf("litellm-gateway: token file %s exceeds %d bytes", tokenFile, maxTokenFileBytes)
	}

	data, err := client.ReadAllBounded(f, maxTokenFileBytes)
	if err != nil {
		return "", fmt.Errorf("litellm-gateway: read token file %s: %w", tokenFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

type modelEntry struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
}

type modelsResponse struct {
	Data []modelEntry `json:"data"`
}

// listModels performs the bounded, authenticated GET through httpfetch and
// decodes the response body.
func listModels(ctx context.Context, baseURL, token string) ([]modelEntry, error) {
	body, err := fetchModelsBody(ctx, baseURL, token)
	if err != nil {
		return nil, err
	}
	return decodeModelsBody(body)
}

// fetchModelsBody sends the token only in the Authorization header;
// httpfetch errors name the URL, never header values.
func fetchModelsBody(ctx context.Context, baseURL, token string) ([]byte, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	url := strings.TrimRight(baseURL, "/") + "/v1/models"
	body, err := httpfetch.GetWithHeader(ctx, url, header, requestTimeout, maxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("litellm-gateway: %w", err)
	}
	return body, nil
}

func decodeModelsBody(body []byte) ([]modelEntry, error) {
	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("litellm-gateway: decode response: %w", err)
	}
	if len(parsed.Data) > maxModels {
		return nil, fmt.Errorf("litellm-gateway: response lists more than %d models", maxModels)
	}
	// One entry without an id becomes a record sync rejects with a reason;
	// a list in which no entry carries an id is an upstream schema change.
	missing := 0
	for _, m := range parsed.Data {
		if m.ID == "" {
			missing++
		}
	}
	if missing > 0 && missing == len(parsed.Data) {
		return nil, fmt.Errorf("litellm-gateway: none of the %d data entries carries an id; the upstream schema changed", missing)
	}
	return parsed.Data, nil
}

func toRecord(m modelEntry, fetchedAt time.Time) catalog.Record {
	return catalog.Record{
		ModelID:    m.ID,
		Provider:   m.OwnedBy,
		AccessPath: catalog.AccessGateway,
		Provenance: catalog.Provenance{
			Source:    SourceName,
			FetchedAt: fetchedAt,
			Kind:      catalog.KindMeasured,
		},
		Absent: map[string]string{
			"context_window":  "gateway /v1/models does not expose it; /model/info returned 403 for the agent token",
			"price_in_per_m":  "gateway /v1/models does not expose it; /model/info returned 403 for the agent token",
			"price_out_per_m": "gateway /v1/models does not expose it; /model/info returned 403 for the agent token",
			"limits":          "gateway /v1/models does not expose rpm/tpm/daily caps",
		},
	}
}
