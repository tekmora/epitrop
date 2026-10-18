package litellmgateway

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/httpx/client"

	"github.com/tekmora/epitrop/catalog"
)

// serveKeyInfo answers GET /key/info with body and records the request.
func serveKeyInfo(t *testing.T, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	seen := &http.Request{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = *r.Clone(context.Background())
		if r.URL.Path != "/key/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body)) //nolint:errcheck // test server response
	}))
	t.Cleanup(server.Close)
	return server, seen
}

func TestKeyBudgetUpstreamContractPinned(t *testing.T) {
	body := mustReadFixture(t, "testdata/upstream/litellm-key-info.md")
	for _, marker := range []string{"status:", `"active"`, "key_alias:", "spend:", "max_budget:", "budget_duration:", "budget_reset_at:", "tpm_limit / rpm_limit:", "blocked:", "Defaults to the key in the Authorization header"} {
		if !bytes.Contains(body, []byte(marker)) {
			t.Fatalf("LiteLLM /key/info contract missing marker %q", marker)
		}
	}
}

func TestFetchKeyBudgetReadsTheFixture(t *testing.T) {
	server, seen := serveKeyInfo(t, string(mustReadFixture(t, "testdata/fixtures/key-info-response.json")))
	res := FetchKeyBudget(context.Background(), server.URL, writeTokenFile(t, "tok"))
	if res.Status != catalog.StatusOK || res.Count != 1 || len(res.Records) != 1 {
		t.Fatalf("FetchKeyBudget() = %+v, want one record", res)
	}
	if seen.Header.Get("Authorization") != "Bearer tok" || seen.URL.RawQuery != "" {
		t.Fatalf("request = %q ?%q, want the token in the header and no query", seen.Header.Get("Authorization"), seen.URL.RawQuery)
	}
	checkFixtureKeyRecord(t, res.Records[0])
}

// checkFixtureKeyRecord checks the record built from key-info-response.json.
func checkFixtureKeyRecord(t *testing.T, rec catalog.Record) {
	t.Helper()
	if rec.ModelID != "litellm-key/agent-lane" || rec.Provenance.Source != KeyBudgetSourceName || rec.Validate() != nil {
		t.Fatalf("record = %+v, want a valid litellm-key/agent-lane record", rec)
	}
	uw := rec.UsageWindow
	wantReset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if uw == nil || *uw.UsedPercent != 25 || *uw.WindowMinutes != 30*24*60 || !uw.ResetsAt.Equal(wantReset) {
		t.Fatalf("usage window = %+v, want 25%% of a 30-day window resetting %s", uw, wantReset)
	}
	checkFixtureKeyLimits(t, rec)
}

func checkFixtureKeyLimits(t *testing.T, rec catalog.Record) {
	t.Helper()
	if rec.Limits == nil || *rec.Limits.RPM != 60 || *rec.Limits.TPM != 200000 {
		t.Fatalf("limits = %+v, want rpm 60 and tpm 200000", rec.Limits)
	}
}

// A key without a budget, or with a zero budget, still yields a record,
// with every missing part named.
func TestFetchKeyBudgetNamesWhatTheKeyLacks(t *testing.T) {
	for _, budget := range []string{"null", "0"} {
		server, _ := serveKeyInfo(t, `{"info":{"spend":3,"max_budget":`+budget+`,"status":"active"}}`)
		res := FetchKeyBudget(context.Background(), server.URL, writeTokenFile(t, "tok"))
		if res.Status != catalog.StatusOK || len(res.Records) != 1 {
			t.Fatalf("max_budget %s: FetchKeyBudget() = %+v, want one record", budget, res)
		}
		rec := res.Records[0]
		if rec.ModelID != "litellm-key" || rec.UsageWindow.UsedPercent != nil || rec.Limits != nil {
			t.Fatalf("max_budget %s: record = %+v, want no used share and no limits", budget, rec)
		}
		for _, field := range []string{"usage_window.used_percent", "usage_window.window_minutes", "usage_window.resets_at", "usage_window.burn_percent_per_hour", "usage_window.active_agents", "limits"} {
			if rec.Absent[field] == "" {
				t.Errorf("max_budget %s: Absent[%s] is empty, want a reason", budget, field)
			}
		}
	}
}

func TestFetchKeyBudgetRefusesAKeyItCannotUse(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"expired", `{"info":{"spend":1,"status":"expired"}}`, `status is "expired"`},
		{"blocked", `{"info":{"spend":1,"blocked":true}}`, "blocked"},
		{"no info", `{"key":"x"}`, "no info object"},
		{"no spend", `{"info":{"status":"active"}}`, "no spend"},
		{"malformed", `{not json`, "decode response"},
	}
	for _, tc := range cases {
		server, _ := serveKeyInfo(t, tc.body)
		res := FetchKeyBudget(context.Background(), server.URL, writeTokenFile(t, "tok"))
		if res.Status != catalog.StatusFail || len(res.Records) != 0 || !strings.Contains(res.Detail, tc.want) {
			t.Errorf("%s: FetchKeyBudget() = %+v, want fail with %q", tc.name, res, tc.want)
		}
	}
}

func TestFetchKeyBudgetNeverLeaksTheToken(t *testing.T) {
	const secret = "do-not-print-me-67890"
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(forbidden.Close)
	for _, base := range []string{forbidden.URL, "http://127.0.0.1:1"} {
		res := FetchKeyBudget(context.Background(), base, writeTokenFile(t, secret))
		if res.Status != catalog.StatusFail || strings.Contains(res.Detail, secret) {
			t.Fatalf("FetchKeyBudget(%s) = %+v, want fail without the token", base, res)
		}
	}
	if res := FetchKeyBudget(context.Background(), forbidden.URL, writeTokenFile(t, " \n")); res.Status != catalog.StatusFail {
		t.Fatalf("FetchKeyBudget(empty token) = %+v, want fail", res)
	}
}

func TestBudgetWindowMinutes(t *testing.T) {
	text := func(s string) *string { return &s }
	for raw, want := range map[string]int{"1m": 1, "60s": 1, "1h": 60, "30d": 43200, "1w": 10080, "daily": 1440, " Monthly ": 43200} {
		if got, reason := budgetWindowMinutes(text(raw)); reason != "" || got != want {
			t.Errorf("budgetWindowMinutes(%q) = %d, %q, want %d", raw, got, reason, want)
		}
	}
	for raw, want := range map[string]string{"": "no budget_duration", "1mo": "no fixed length", "59s": "shorter than a minute", "0d": "no fixed length", "30dx": "no fixed length", "-1h": "no fixed length", "1234567d": "no fixed length"} {
		if got, reason := budgetWindowMinutes(text(raw)); !strings.Contains(reason, want) {
			t.Errorf("budgetWindowMinutes(%q) = %d, %q, want a reason with %q", raw, got, reason, want)
		}
	}
	if _, reason := budgetWindowMinutes(nil); reason == "" {
		t.Error("budgetWindowMinutes(nil) gave no reason, want one")
	}
}

func TestBudgetResetAt(t *testing.T) {
	text := func(s string) *string { return &s }
	got, reason := budgetResetAt(text("2026-11-01T02:00:00.5+02:00"))
	if reason != "" || !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 5e8, time.UTC)) || got.Location() != time.UTC {
		t.Fatalf("budgetResetAt() = %s, %q, want the instant in UTC", got, reason)
	}
	for _, raw := range []*string{nil, text(""), text("2026-11-01T00:00:00"), text("tomorrow")} {
		if _, reason = budgetResetAt(raw); reason == "" {
			t.Errorf("budgetResetAt(%v) gave no reason, want one", raw)
		}
	}
}

// The cap is 1 MiB, written out so a changed constant turns this red.
func TestFetchKeyInfoResponseCap(t *testing.T) {
	const capBytes = 1 << 20
	serve := func(size int) string {
		body := `{"info":{"spend":1}}`
		server, _ := serveKeyInfo(t, body+strings.Repeat(" ", size-len(body)))
		return server.URL
	}
	if _, err := fetchKeyInfo(context.Background(), serve(capBytes), "tok"); err != nil {
		t.Fatalf("fetchKeyInfo(at the cap) = %v, want nil", err)
	}
	if _, err := fetchKeyInfo(context.Background(), serve(capBytes+1), "tok"); !errors.Is(err, client.ErrBodyTooLarge) {
		t.Fatalf("fetchKeyInfo(one byte over) = %v, want client.ErrBodyTooLarge", err)
	}
}
