package litellmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tekmora/epitrop/catalog"
	"github.com/tekmora/epitrop/internal/sources/httpfetch"
)

// The key budget source reads the calling key's own budget window from
// GET /key/info. Without a key query parameter the gateway answers for the
// key in the Authorization header, so the token never enters a URL or an
// access log. The fields read here are the ones the pinned upstream
// docstring names (testdata/upstream/litellm-key-info.md).

// KeyBudgetSourceName identifies the key budget source in Snapshot.SourceRuns
// and CLI flags.
const KeyBudgetSourceName = "litellm-key-budget"

const (
	// KeyBudgetMaxRecords is the maximum number of records the key budget
	// source contributes to one snapshot: the calling key's own.
	KeyBudgetMaxRecords = 1
	// maxKeyInfoBytes bounds the /key/info response read (HISS-02).
	maxKeyInfoBytes = 1 << 20
	// keyRecordPrefix starts every key budget record's model id.
	keyRecordPrefix = "litellm-key"
)

// budgetDuration is the part of LiteLLM's budget_duration grammar with a
// fixed length; "mo" windows end on calendar days and have none.
var budgetDuration = regexp.MustCompile(`^([1-9][0-9]{0,5})([smhdw])$`)

// budgetDurationAliases are the words LiteLLM normalises before parsing.
var budgetDurationAliases = map[string]string{"hourly": "1h", "daily": "24h", "weekly": "7d", "monthly": "30d"}

var budgetUnitSeconds = map[string]int{"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}

type keyInfoResponse struct {
	Info *keyInfo `json:"info"`
}

type keyInfo struct {
	Status         string   `json:"status"`
	Blocked        *bool    `json:"blocked"`
	KeyAlias       *string  `json:"key_alias"`
	Spend          *float64 `json:"spend"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration *string  `json:"budget_duration"`
	BudgetResetAt  *string  `json:"budget_reset_at"`
	TPMLimit       *int     `json:"tpm_limit"`
	RPMLimit       *int     `json:"rpm_limit"`
}

// FetchKeyBudget reads the bearer token from tokenFile and turns the key's
// budget window at baseURL+"/key/info" into one record. A key that is not
// active, a response without info or spend, or any transport failure is
// StatusFail with a detail that never contains the token.
func FetchKeyBudget(ctx context.Context, baseURL, tokenFile string, now time.Time) Result {
	token, err := ReadToken(tokenFile)
	if err != nil {
		return Result{Status: catalog.StatusFail, Detail: err.Error()}
	}
	if token == "" {
		return Result{Status: catalog.StatusFail, Detail: fmt.Sprintf("token file %s is empty", tokenFile)}
	}
	info, err := fetchKeyInfo(ctx, baseURL, token)
	if err != nil {
		return Result{Status: catalog.StatusFail, Detail: err.Error()}
	}
	rec := keyRecord(info, now.UTC())
	return Result{Records: []catalog.Record{rec}, Status: catalog.StatusOK, Count: 1}
}

func fetchKeyInfo(ctx context.Context, baseURL, token string) (keyInfo, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	url := strings.TrimRight(baseURL, "/") + "/key/info"
	body, err := httpfetch.GetWithHeader(ctx, url, header, requestTimeout, maxKeyInfoBytes)
	if err != nil {
		return keyInfo{}, fmt.Errorf("%s: %w", KeyBudgetSourceName, err)
	}
	return decodeKeyInfo(body)
}

func decodeKeyInfo(body []byte) (keyInfo, error) {
	var parsed keyInfoResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return keyInfo{}, fmt.Errorf("%s: decode response: %w", KeyBudgetSourceName, err)
	}
	switch info := parsed.Info; {
	case info == nil:
		return keyInfo{}, fmt.Errorf("%s: response carries no info object; the upstream schema changed", KeyBudgetSourceName)
	case info.Spend == nil:
		return keyInfo{}, fmt.Errorf("%s: info carries no spend; the upstream schema changed", KeyBudgetSourceName)
	case info.Status != "" && info.Status != "active":
		return keyInfo{}, fmt.Errorf("%s: key status is %q, not active", KeyBudgetSourceName, info.Status)
	case info.Blocked != nil && *info.Blocked:
		return keyInfo{}, fmt.Errorf("%s: key is blocked", KeyBudgetSourceName)
	default:
		return *info, nil
	}
}

func keyRecord(info keyInfo, fetchedAt time.Time) catalog.Record {
	id := keyRecordPrefix
	if info.KeyAlias != nil && *info.KeyAlias != "" {
		id += "/" + *info.KeyAlias
	}
	rec := catalog.Record{
		ModelID:    id,
		AccessPath: catalog.AccessGateway,
		Provenance: catalog.Provenance{Source: KeyBudgetSourceName, FetchedAt: fetchedAt, Kind: catalog.KindMeasured},
		Absent: map[string]string{
			"context_window":  "a key budget is no model",
			"price_in_per_m":  "a key budget is no model",
			"price_out_per_m": "a key budget is no model",
		},
	}
	rec.UsageWindow = keyUsageWindow(info, rec.Absent)
	rec.Limits = keyLimits(info, rec.Absent)
	return rec
}

// keyUsageWindow turns spend against max_budget into a used share and names
// every part it cannot fill in absent.
func keyUsageWindow(info keyInfo, absent map[string]string) *catalog.UsageWindow {
	uw := &catalog.UsageWindow{}
	absent["usage_window.burn_percent_per_hour"] = "key info reports the spend, not its rate"
	absent["usage_window.active_agents"] = "key info does not name the callers of the key"
	if info.MaxBudget != nil && *info.MaxBudget > 0 {
		used := *info.Spend / *info.MaxBudget * 100
		uw.UsedPercent = &used
	} else {
		absent["usage_window.used_percent"] = "the key has no positive max_budget"
	}
	if minutes, reason := budgetWindowMinutes(info.BudgetDuration); reason != "" {
		absent["usage_window.window_minutes"] = reason
	} else {
		uw.WindowMinutes = &minutes
	}
	if resets, reason := budgetResetAt(info.BudgetResetAt); reason != "" {
		absent["usage_window.resets_at"] = reason
	} else {
		uw.ResetsAt = &resets
	}
	return uw
}

func budgetWindowMinutes(duration *string) (int, string) {
	if duration == nil || *duration == "" {
		return 0, "the key has no budget_duration"
	}
	raw := strings.TrimSpace(strings.ToLower(*duration))
	if alias, ok := budgetDurationAliases[raw]; ok {
		raw = alias
	}
	m := budgetDuration.FindStringSubmatch(raw)
	if m == nil {
		return 0, fmt.Sprintf("budget_duration %q has no fixed length", *duration)
	}
	value, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Sprintf("budget_duration %q: %v", *duration, err)
	}
	seconds := value * budgetUnitSeconds[m[2]]
	if seconds < 60 {
		return 0, fmt.Sprintf("budget_duration %q is shorter than a minute", *duration)
	}
	return seconds / 60, ""
}

func budgetResetAt(raw *string) (time.Time, string) {
	if raw == nil || *raw == "" {
		return time.Time{}, "the key has no budget_reset_at"
	}
	t, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil {
		return time.Time{}, fmt.Sprintf("budget_reset_at %q carries no time zone or is malformed", *raw)
	}
	return t.UTC(), ""
}

func keyLimits(info keyInfo, absent map[string]string) *catalog.Limits {
	if info.RPMLimit == nil && info.TPMLimit == nil {
		absent["limits"] = "the key has no rpm_limit or tpm_limit"
		return nil
	}
	return &catalog.Limits{RPM: info.RPMLimit, TPM: info.TPMLimit}
}
