// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testRepo      = "tekmora/epitrop"
	testOurSHA    = "2bbdef5db8deb4bb645d4bceab6b42af4751e25a"
	testLiveSHA   = "0c759db111111111111111111111111111111111"
	testOlderSHA  = "aaaaaaa000000000000000000000000000000000"
	testAccessVal = "ghs_test_viewer_value"
	maxTestCases  = 64
)

type fakeGitHub struct {
	deploymentsStatus int
	deploymentsRaw    string
	deployments       []deployment
	statusesStatus    int
	statusesRaw       string
	statusesByID      map[int64][]deploymentStatus
	compareStatus     int
	compareRaw        string
	compareByPair     map[string]string
	statusesCalls     atomic.Int32
	compareCalls      atomic.Int32
}

func newFakeGitHubServer(t *testing.T, fk *fakeGitHub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAccessVal {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fk.serve(t, w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (fk *fakeGitHub) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	const prefix = "/repos/" + testRepo
	switch {
	case r.URL.Path == prefix+"/deployments":
		fk.serveDeployments(t, w, r)
	case strings.HasPrefix(r.URL.Path, prefix+"/deployments/") && strings.HasSuffix(r.URL.Path, "/statuses"):
		fk.serveStatuses(t, w, r)
	case strings.HasPrefix(r.URL.Path, prefix+"/compare/"):
		fk.serveCompare(t, w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (fk *fakeGitHub) serveDeployments(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.URL.Query().Get("environment") != "github-pages" || r.URL.Query().Get("per_page") != "30" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if fk.deploymentsStatus != 0 {
		w.WriteHeader(fk.deploymentsStatus)
		return
	}
	if fk.deploymentsRaw != "" {
		writeBytes(t, w, []byte(fk.deploymentsRaw))
		return
	}
	writeJSON(t, w, fk.deployments)
}

func (fk *fakeGitHub) serveStatuses(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	fk.statusesCalls.Add(1)
	if r.URL.Query().Get("per_page") != "100" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if fk.statusesStatus != 0 {
		w.WriteHeader(fk.statusesStatus)
		return
	}
	if fk.statusesRaw != "" {
		writeBytes(t, w, []byte(fk.statusesRaw))
		return
	}
	var id int64
	if _, err := fmt.Sscanf(r.URL.Path, "/repos/"+testRepo+"/deployments/%d/statuses", &id); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeJSON(t, w, fk.statusesByID[id])
}

func (fk *fakeGitHub) serveCompare(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	fk.compareCalls.Add(1)
	if r.URL.Query().Get("per_page") != "1" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if fk.compareStatus != 0 {
		w.WriteHeader(fk.compareStatus)
		return
	}
	if fk.compareRaw != "" {
		writeBytes(t, w, []byte(fk.compareRaw))
		return
	}
	pair := strings.TrimPrefix(r.URL.Path, "/repos/"+testRepo+"/compare/")
	state, ok := fk.compareByPair[pair]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(t, w, compareResponse{Status: state})
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Errorf("marshal response: %v", err)
		return
	}
	writeBytes(t, w, data)
}

func writeBytes(t *testing.T, w http.ResponseWriter, data []byte) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(data); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func makeFailedDeployments(count int) ([]deployment, map[int64][]deploymentStatus) {
	deps := make([]deployment, 0, count)
	statuses := make(map[int64][]deploymentStatus, count)
	for i := 0; i < count && i < maxTestCases; i++ {
		id := int64(i + 1)
		deps = append(deps, deployment{
			ID:        id,
			SHA:       fmt.Sprintf("sha-%02d", id),
			CreatedAt: fmt.Sprintf("2026-11-18T10:%02d:00Z", i%60),
		})
		statuses[id] = []deploymentStatus{{State: "failure"}}
	}
	return deps, statuses
}

type decideCase struct {
	name             string
	sha              string
	fk               *fakeGitHub
	wantAllow        bool
	wantReason       []string
	wantErr          string
	wantStatusesCall int32
	wantCompareCall  int32
}

func TestDecidePositive(t *testing.T) {
	cases := []decideCase{
		{
			name:      "no deployments allows",
			fk:        &fakeGitHub{deployments: []deployment{}},
			wantAllow: true, wantReason: []string{"no earlier successful deployment"},
		},
		{
			name: "live equals ours allows without compare",
			fk: &fakeGitHub{
				deployments:  []deployment{{ID: 1, SHA: testOurSHA, CreatedAt: "2026-11-18T12:00:00Z"}},
				statusesByID: map[int64][]deploymentStatus{1: {{State: "success"}}},
			},
			wantAllow: true, wantReason: []string{testOurSHA}, wantStatusesCall: 1, wantCompareCall: 0,
		},
		{
			name: "compare ahead allows",
			fk: &fakeGitHub{
				deployments:   []deployment{{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"}},
				statusesByID:  map[int64][]deploymentStatus{1: {{State: "success"}}},
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "ahead"},
			},
			wantAllow: true, wantReason: []string{"ahead", testLiveSHA}, wantStatusesCall: 1, wantCompareCall: 1,
		},
		{
			name: "compare identical allows",
			fk: &fakeGitHub{
				deployments:   []deployment{{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"}},
				statusesByID:  map[int64][]deploymentStatus{1: {{State: "success"}}},
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "identical"},
			},
			wantAllow: true, wantReason: []string{"identical", testLiveSHA}, wantStatusesCall: 1, wantCompareCall: 1,
		},
	}
	runDecideCases(t, cases)
}

func TestDecideOrderingAndStatuses(t *testing.T) {
	cases := []decideCase{
		{
			name: "newest deployment failed and older success uses older as live",
			fk: &fakeGitHub{
				deployments: []deployment{
					{ID: 2, SHA: testOlderSHA, CreatedAt: "2026-11-18T13:00:00Z"},
					{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"},
				},
				statusesByID: map[int64][]deploymentStatus{
					2: {{State: "failure"}},
					1: {{State: "success"}},
				},
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "ahead"},
			},
			wantAllow: true, wantReason: []string{"ahead", testLiveSHA}, wantStatusesCall: 2, wantCompareCall: 1,
		},
		{
			name: "status list inactive and success counts as success and stops at first hit",
			fk: &fakeGitHub{
				deployments: []deployment{
					{ID: 2, SHA: testLiveSHA, CreatedAt: "2026-11-18T13:00:00Z"},
					{ID: 1, SHA: testOlderSHA, CreatedAt: "2026-11-18T12:00:00Z"},
				},
				statusesByID: map[int64][]deploymentStatus{
					2: {{State: "inactive"}, {State: "success"}},
					1: {{State: "success"}},
				},
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "ahead"},
			},
			wantAllow: true, wantReason: []string{"ahead", testLiveSHA}, wantStatusesCall: 1, wantCompareCall: 1,
		},
		{
			name: "response order shuffled is sorted by created_at desc then id desc",
			fk: &fakeGitHub{
				deployments: []deployment{
					{ID: 1, SHA: testOlderSHA, CreatedAt: "2026-11-18T10:00:00Z"},
					{ID: 2, SHA: testOlderSHA, CreatedAt: "2026-11-18T12:00:00Z"},
					{ID: 3, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"},
				},
				statusesByID: map[int64][]deploymentStatus{
					3: {{State: "success"}},
					2: {{State: "success"}},
					1: {{State: "success"}},
				},
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "ahead"},
			},
			wantAllow: true, wantReason: []string{"ahead", testLiveSHA}, wantStatusesCall: 1, wantCompareCall: 1,
		},
	}
	runDecideCases(t, cases)
}

func TestDecideBoundary(t *testing.T) {
	deps29, stat29 := makeFailedDeployments(29)
	deps30, stat30 := makeFailedDeployments(30)
	deps35, stat35 := makeFailedDeployments(35)
	cases := []decideCase{
		{
			name:      "boundary 29 deployments none success allows",
			fk:        &fakeGitHub{deployments: deps29, statusesByID: stat29},
			wantAllow: true, wantReason: []string{"no earlier successful deployment"}, wantStatusesCall: 29,
		},
		{
			name:      "boundary 30 deployments none success refuses",
			fk:        &fakeGitHub{deployments: deps30, statusesByID: stat30},
			wantAllow: false, wantReason: []string{"live deployment not among the newest 30"}, wantStatusesCall: 30,
		},
		{
			name:      "boundary over 30 deployments makes at most 30 statuses requests",
			fk:        &fakeGitHub{deployments: deps35, statusesByID: stat35},
			wantAllow: false, wantReason: []string{"live deployment not among the newest 30"}, wantStatusesCall: 30,
		},
	}
	runDecideCases(t, cases)
}

func TestDecideNegativeCompare(t *testing.T) {
	liveDep := []deployment{{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"}}
	liveSuccess := map[int64][]deploymentStatus{1: {{State: "success"}}}
	cases := []decideCase{
		{
			name: "compare behind refuses and names both shas",
			fk: &fakeGitHub{
				deployments:   liveDep,
				statusesByID:  liveSuccess,
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "behind"},
			},
			wantAllow:        false,
			wantReason:       []string{testOurSHA, testLiveSHA, "behind", "re-run the newest Pages run on main or dispatch the workflow on main"},
			wantStatusesCall: 1,
			wantCompareCall:  1,
		},
		{
			name: "compare diverged refuses and names status",
			fk: &fakeGitHub{
				deployments:   liveDep,
				statusesByID:  liveSuccess,
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "diverged"},
			},
			wantAllow:        false,
			wantReason:       []string{testOurSHA, testLiveSHA, "diverged"},
			wantStatusesCall: 1,
			wantCompareCall:  1,
		},
	}
	runDecideCases(t, cases)
}

func TestDecideNegativeErrors(t *testing.T) {
	liveDep := []deployment{{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"}}
	liveSuccess := map[int64][]deploymentStatus{1: {{State: "success"}}}
	cases := []decideCase{
		{
			name:    "HTTP 500 on deployments returns error",
			fk:      &fakeGitHub{deploymentsStatus: http.StatusInternalServerError},
			wantErr: "fetch deployments",
		},
		{
			name:             "HTTP 500 on statuses returns error",
			fk:               &fakeGitHub{deployments: liveDep, statusesStatus: http.StatusInternalServerError},
			wantErr:          "fetch statuses for deployment 1",
			wantStatusesCall: 1,
		},
		{
			name:             "HTTP 500 on compare returns error",
			fk:               &fakeGitHub{deployments: liveDep, statusesByID: liveSuccess, compareStatus: http.StatusInternalServerError},
			wantErr:          "compare " + testLiveSHA + "..." + testOurSHA,
			wantStatusesCall: 1,
			wantCompareCall:  1,
		},
		{
			name:    "malformed JSON on deployments returns error",
			fk:      &fakeGitHub{deploymentsRaw: "{not-json"},
			wantErr: "decode deployments",
		},
		{
			name:             "malformed JSON on statuses returns error",
			fk:               &fakeGitHub{deployments: liveDep, statusesRaw: "{not-json"},
			wantErr:          "decode statuses for deployment 1",
			wantStatusesCall: 1,
		},
		{
			name:             "malformed JSON on compare returns error",
			fk:               &fakeGitHub{deployments: liveDep, statusesByID: liveSuccess, compareRaw: "{not-json"},
			wantErr:          "decode compare",
			wantStatusesCall: 1,
			wantCompareCall:  1,
		},
		{
			name: "successful deployment with empty sha returns error",
			fk: &fakeGitHub{
				deployments:  []deployment{{ID: 1, SHA: " ", CreatedAt: "2026-11-18T12:00:00Z"}},
				statusesByID: liveSuccess,
			},
			wantErr:          "deployment 1 has an empty sha",
			wantStatusesCall: 1,
		},
		{
			name:    "missing GITHUB_SHA returns error",
			sha:     " ",
			fk:      &fakeGitHub{},
			wantErr: "GITHUB_SHA is required",
		},
	}
	runDecideCases(t, cases)
}

func runDecideCases(t *testing.T, cases []decideCase) {
	t.Helper()
	for i := 0; i < len(cases) && i < maxTestCases; i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			checkDecideCase(t, tc)
		})
	}
}

func checkDecideCase(t *testing.T, tc decideCase) {
	t.Helper()
	srv := newFakeGitHubServer(t, tc.fk)
	sha := testOurSHA
	if tc.sha != "" {
		sha = strings.TrimSpace(tc.sha)
	}
	cfg := config{
		APIBase:    srv.URL,
		Repository: testRepo,
		SHA:        sha,
		Access:     testAccessVal,
	}
	allow, reason, err := decide(context.Background(), cfg)
	checkDecideOutcome(t, tc, allow, reason, err)
	checkDecideCalls(t, tc)
}

func checkDecideOutcome(t *testing.T, tc decideCase, allow bool, reason string, err error) {
	t.Helper()
	if tc.wantErr != "" {
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("decide() err = %v, want substring %q", err, tc.wantErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("decide() unexpected err = %v", err)
	}
	if allow != tc.wantAllow {
		t.Fatalf("decide() allow = %v (reason %q), want %v", allow, reason, tc.wantAllow)
	}
	for j := 0; j < len(tc.wantReason) && j < maxTestCases; j++ {
		if !strings.Contains(reason, tc.wantReason[j]) {
			t.Fatalf("decide() reason = %q, missing %q", reason, tc.wantReason[j])
		}
	}
}

func checkDecideCalls(t *testing.T, tc decideCase) {
	t.Helper()
	if got := tc.fk.statusesCalls.Load(); got != tc.wantStatusesCall {
		t.Fatalf("statuses calls = %d, want %d", got, tc.wantStatusesCall)
	}
	if got := tc.fk.compareCalls.Load(); got != tc.wantCompareCall {
		t.Fatalf("compare calls = %d, want %d", got, tc.wantCompareCall)
	}
}

func TestRunWritesStepSummaryAndEnforcesConfig(t *testing.T) {
	fk := &fakeGitHub{
		deployments:   []deployment{{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"}},
		statusesByID:  map[int64][]deploymentStatus{1: {{State: "success"}}},
		compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "ahead"},
	}
	srv := newFakeGitHubServer(t, fk)
	summaryPath := filepath.Join(t.TempDir(), "summary.md")
	env := map[string]string{
		"GITHUB_API_URL":      srv.URL,
		"GITHUB_REPOSITORY":   testRepo,
		"GITHUB_SHA":          testOurSHA,
		"GH_TOKEN":            testAccessVal,
		"GITHUB_STEP_SUMMARY": summaryPath,
	}
	var out bytes.Buffer
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	summaryBytes, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v", summaryPath, err)
	}
	if out.String() == "" || string(summaryBytes) != out.String() {
		t.Fatalf("summary = %q, stdout = %q, want identical non-empty outcome line", summaryBytes, out.String())
	}
	if err := run(context.Background(), func(string) string { return "" }, &out); err == nil {
		t.Fatal("run(empty env) = nil, want missing config error")
	}
}

// TestRunRefusalFailsTheStep guards the exit path: a refusal from decide must
// reach main as an error (exit 1), or the deploy step after the guard runs.
func TestRunRefusalFailsTheStep(t *testing.T) {
	fk := &fakeGitHub{
		deployments:   []deployment{{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"}},
		statusesByID:  map[int64][]deploymentStatus{1: {{State: "success"}}},
		compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "behind"},
	}
	srv := newFakeGitHubServer(t, fk)
	env := map[string]string{
		"GITHUB_API_URL":    srv.URL,
		"GITHUB_REPOSITORY": testRepo,
		"GITHUB_SHA":        testOurSHA,
		"GH_TOKEN":          testAccessVal,
	}
	var out bytes.Buffer
	err := run(context.Background(), func(k string) string { return env[k] }, &out)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("run() = %v, want refused error", err)
	}
	if !strings.HasPrefix(out.String(), "pagesguard: refuse: ") {
		t.Fatalf("stdout = %q, want refuse outcome line", out.String())
	}
}
