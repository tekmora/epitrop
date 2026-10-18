package publiccatalog

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

// SourceName identifies this source in Snapshot.SourceRuns and CLI flags.
// It covers two independent sub-fetches (OpenRouter, LiteLLM prices); see
// package doc for why they are not reported as separate CLI sources.
const SourceName = "public-catalog"

// MaxRecords is the maximum number of records this source can contribute to
// one snapshot.
const MaxRecords = MaxOpenRouterRecords + MaxLiteLLMPriceRecords

// Result is one Fetch attempt's outcome.
type Result struct {
	Records []catalog.Record
	Status  catalog.Status
	Count   int
	Detail  string
}

// Fetch runs both public-catalog sub-fetches and merges them. Each fails
// independently: one succeeding while the other fails still returns
// StatusDegraded with that sub-fetch's records and a Detail line naming the
// other's failure, because "OpenRouter is down" should never hide LiteLLM's
// price map (or vice versa).
func Fetch(ctx context.Context, openRouterURL, liteLLMPriceMapURL string, now time.Time) Result {
	var records []catalog.Record
	var notes []string
	degraded := false

	orResult, orErr := fetchOpenRouterWithReport(ctx, openRouterURL, now)
	if orErr != nil {
		notes = append(notes, "openrouter: fail: "+orErr.Error())
		degraded = true
	} else {
		notes = append(notes, openRouterDetail(orResult))
		if orResult.Rejected > 0 {
			degraded = true
		}
		records = append(records, orResult.Records...)
	}

	llmResult, llmErr := fetchLiteLLMPricesWithReport(ctx, liteLLMPriceMapURL, now)
	if llmErr != nil {
		notes = append(notes, "litellm-prices: fail: "+llmErr.Error())
		degraded = true
	} else {
		notes = append(notes, liteLLMDetail(llmResult))
		if llmResult.Malformed > 0 {
			degraded = true
		}
		records = append(records, llmResult.Records...)
	}

	detail := strings.Join(notes, "; ")
	if orErr != nil && llmErr != nil {
		return Result{Status: catalog.StatusFail, Detail: detail}
	}
	if len(records) == 0 {
		return Result{Status: catalog.StatusSkip, Detail: detail}
	}
	if degraded {
		return Result{Records: records, Status: catalog.StatusDegraded, Count: len(records), Detail: detail}
	}
	return Result{Records: records, Status: catalog.StatusOK, Count: len(records), Detail: detail}
}

func openRouterDetail(res openRouterResult) string {
	if res.Rejected == 0 {
		return fmt.Sprintf("openrouter: ok (%d)", len(res.Records))
	}
	if res.FirstReject == "" {
		return fmt.Sprintf("openrouter: ok (%d, rejected=%d)", len(res.Records), res.Rejected)
	}
	return fmt.Sprintf("openrouter: ok (%d, rejected=%d, first: %s)", len(res.Records), res.Rejected, res.FirstReject)
}

func liteLLMDetail(res liteLLMPriceResult) string {
	if res.Malformed == 0 {
		return fmt.Sprintf("litellm-prices: ok (%d)", len(res.Records))
	}
	return fmt.Sprintf("litellm-prices: ok (%d, malformed=%d)", len(res.Records), res.Malformed)
}
