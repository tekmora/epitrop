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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testRepo           = "tekmora/epitrop"
	testOurSHA         = "2bbdef5db8deb4bb645d4bceab6b42af4751e25a"
	testLiveSHA        = "0c759db111111111111111111111111111111111"
	testOlderSHA       = "aaaaaaa000000000000000000000000000000000"
	testAccessVal      = "ghs_test_viewer_value"
	maxTestCases       = 64
	maxTestDeployments = 400
)

type fakeGitHub struct {
	deploymentsStatus      int
	deploymentsStatusPage  int
	deploymentsBadJSON     bool
	deploymentsBadJSONPage int
	deployments            []deployment
	statusesStatus         int
	statusesBadJSON        bool
	statusesByID           map[int64][]deploymentStatus
	compareStatus          int
	compareBadJSON         bool
	compareByPair          map[string]string
	deploymentsCalls       atomic.Int32
	statusesCalls          atomic.Int32
	compareCalls           atomic.Int32
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
	fk.deploymentsCalls.Add(1)
	page, ok := parseDeploymentsQuery(r)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if fk.deploymentsStatus != 0 && matchesPage(fk.deploymentsStatusPage, page) {
		w.WriteHeader(fk.deploymentsStatus)
		return
	}
	if fk.deploymentsBadJSON && matchesPage(fk.deploymentsBadJSONPage, page) {
		writeJSON(t, w, map[string]string{"invalid": "deployments"})
		return
	}
	serveDeploymentPage(t, w, fk.deployments, page)
}

func parseDeploymentsQuery(r *http.Request) (int, bool) {
	q := r.URL.Query()
	if q.Get("environment") != "github-pages" || q.Get("per_page") != "100" {
		return 0, false
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		return 0, false
	}
	return page, true
}

func matchesPage(targetPage, page int) bool {
	return targetPage == 0 || targetPage == page
}

func serveDeploymentPage(t *testing.T, w http.ResponseWriter, deps []deployment, page int) {
	t.Helper()
	start := min((page-1)*deploymentsPerPage, len(deps))
	end := min(start+deploymentsPerPage, len(deps))
	writeJSON(t, w, deps[start:end])
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
	if fk.statusesBadJSON {
		writeJSON(t, w, map[string]string{"invalid": "statuses"})
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
	if fk.compareBadJSON {
		writeJSON(t, w, []string{"invalid-compare"})
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
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func makeFailedDeployments(count int) ([]deployment, map[int64][]deploymentStatus) {
	deps := make([]deployment, 0, count)
	statuses := make(map[int64][]deploymentStatus, count)
	for i := 0; i < count && i < maxTestDeployments; i++ {
		id := int64(count - i + 1)
		deps = append(deps, deployment{
			ID:        id,
			SHA:       fmt.Sprintf("sha-%03d", id),
			CreatedAt: "2026-11-18T13:00:00Z",
		})
		statuses[id] = []deploymentStatus{{State: "failure"}}
	}
	return deps, statuses
}

type decideCase struct {
	name                string
	sha                 string
	fk                  *fakeGitHub
	wantAllow           bool
	wantReason          []string
	wantErr             string
	wantDeploymentsCall int32
	wantStatusesCall    int32
	wantCompareCall     int32
}

func TestDecidePositive(t *testing.T) {
	cases := []decideCase{
		{
			name:                "no deployments allows",
			fk:                  &fakeGitHub{deployments: []deployment{}},
			wantAllow:           true,
			wantReason:          []string{"no earlier successful deployment"},
			wantDeploymentsCall: 1,
		},
		{
			name: "live equals ours allows without compare",
			fk: &fakeGitHub{
				deployments:  []deployment{{ID: 1, SHA: testOurSHA, CreatedAt: "2026-11-18T12:00:00Z"}},
				statusesByID: map[int64][]deploymentStatus{1: {{State: "success"}}},
			},
			wantAllow:           true,
			wantReason:          []string{testOurSHA},
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
			wantCompareCall:     0,
		},
		{
			name: "compare ahead allows",
			fk: &fakeGitHub{
				deployments:   []deployment{{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"}},
				statusesByID:  map[int64][]deploymentStatus{1: {{State: "success"}}},
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "ahead"},
			},
			wantAllow:           true,
			wantReason:          []string{"ahead", testLiveSHA},
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
			wantCompareCall:     1,
		},
		{
			name: "compare identical allows",
			fk: &fakeGitHub{
				deployments:   []deployment{{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"}},
				statusesByID:  map[int64][]deploymentStatus{1: {{State: "success"}}},
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "identical"},
			},
			wantAllow:           true,
			wantReason:          []string{"identical", testLiveSHA},
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
			wantCompareCall:     1,
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
			wantAllow:           true,
			wantReason:          []string{"ahead", testLiveSHA},
			wantDeploymentsCall: 1,
			wantStatusesCall:    2,
			wantCompareCall:     1,
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
			wantAllow:           true,
			wantReason:          []string{"ahead", testLiveSHA},
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
			wantCompareCall:     1,
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
			wantAllow:           true,
			wantReason:          []string{"ahead", testLiveSHA},
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
			wantCompareCall:     1,
		},
	}
	runDecideCases(t, cases)
}

func TestDecideBoundary(t *testing.T) {
	depsRec, statRec := makeFailedDeployments(120)
	depsRec = append(depsRec, deployment{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"})
	statRec[1] = []deploymentStatus{{State: "success"}}
	deps150, stat150 := makeFailedDeployments(150)
	deps300, stat300 := makeFailedDeployments(300)
	deps305, stat305 := makeFailedDeployments(305)
	cases := []decideCase{
		{
			name: "recovery success on page 2 after 120 failed deployments compares with live",
			fk: &fakeGitHub{
				deployments:   depsRec,
				statusesByID:  statRec,
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "ahead"},
			},
			wantAllow:           true,
			wantReason:          []string{"ahead", testLiveSHA},
			wantDeploymentsCall: 2,
			wantStatusesCall:    121,
			wantCompareCall:     1,
		},
		{
			name:                "boundary 150 deployments none success exhausts history on page 2 and allows",
			fk:                  &fakeGitHub{deployments: deps150, statusesByID: stat150},
			wantAllow:           true,
			wantReason:          []string{"no earlier successful deployment"},
			wantDeploymentsCall: 2,
			wantStatusesCall:    150,
		},
		{
			name:                "boundary 300 deployments none success refuses",
			fk:                  &fakeGitHub{deployments: deps300, statusesByID: stat300},
			wantAllow:           false,
			wantReason:          []string{"live deployment not among the newest 300"},
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
		},
		{
			name:                "boundary over 300 deployments makes at most 3 page requests and 300 statuses requests",
			fk:                  &fakeGitHub{deployments: deps305, statusesByID: stat305},
			wantAllow:           false,
			wantReason:          []string{"live deployment not among the newest 300"},
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
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
			wantAllow:           false,
			wantReason:          []string{testOurSHA, testLiveSHA, "behind", "re-run the newest Pages run on main or dispatch the workflow on main"},
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
			wantCompareCall:     1,
		},
		{
			name: "compare diverged refuses and names status",
			fk: &fakeGitHub{
				deployments:   liveDep,
				statusesByID:  liveSuccess,
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "diverged"},
			},
			wantAllow:           false,
			wantReason:          []string{testOurSHA, testLiveSHA, "diverged"},
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
			wantCompareCall:     1,
		},
	}
	runDecideCases(t, cases)
}

func TestDecideNegativeErrors(t *testing.T) {
	liveDep := []deployment{{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"}}
	liveSuccess := map[int64][]deploymentStatus{1: {{State: "success"}}}
	deps150, stat150 := makeFailedDeployments(150)
	cases := []decideCase{
		{
			name:                "HTTP 500 on deployments returns error",
			fk:                  &fakeGitHub{deploymentsStatus: http.StatusInternalServerError},
			wantErr:             "fetch deployments page 1",
			wantDeploymentsCall: 1,
		},
		{
			name: "HTTP 500 on deployments page 2 returns error",
			fk: &fakeGitHub{
				deployments:           deps150,
				statusesByID:          stat150,
				deploymentsStatus:     http.StatusInternalServerError,
				deploymentsStatusPage: 2,
			},
			wantErr:             "fetch deployments page 2",
			wantDeploymentsCall: 2,
		},
		{
			name:                "HTTP 500 on statuses returns error",
			fk:                  &fakeGitHub{deployments: liveDep, statusesStatus: http.StatusInternalServerError},
			wantErr:             "fetch statuses for deployment 1",
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
		},
		{
			name:                "HTTP 500 on compare returns error",
			fk:                  &fakeGitHub{deployments: liveDep, statusesByID: liveSuccess, compareStatus: http.StatusInternalServerError},
			wantErr:             "compare " + testLiveSHA + "..." + testOurSHA,
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
			wantCompareCall:     1,
		},
		{
			name:                "malformed JSON on deployments returns error",
			fk:                  &fakeGitHub{deploymentsBadJSON: true},
			wantErr:             "decode deployments page 1",
			wantDeploymentsCall: 1,
		},
		{
			name: "malformed JSON on deployments page 2 returns error",
			fk: &fakeGitHub{
				deployments:            deps150,
				statusesByID:           stat150,
				deploymentsBadJSON:     true,
				deploymentsBadJSONPage: 2,
			},
			wantErr:             "decode deployments page 2",
			wantDeploymentsCall: 2,
		},
		{
			name:                "malformed JSON on statuses returns error",
			fk:                  &fakeGitHub{deployments: liveDep, statusesBadJSON: true},
			wantErr:             "decode statuses for deployment 1",
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
		},
		{
			name:                "malformed JSON on compare returns error",
			fk:                  &fakeGitHub{deployments: liveDep, statusesByID: liveSuccess, compareBadJSON: true},
			wantErr:             "decode compare",
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
			wantCompareCall:     1,
		},
		{
			name: "successful deployment with empty sha returns error",
			fk: &fakeGitHub{
				deployments:  []deployment{{ID: 1, SHA: " ", CreatedAt: "2026-11-18T12:00:00Z"}},
				statusesByID: liveSuccess,
			},
			wantErr:             "deployment 1 has an empty sha",
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
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
	if got := tc.fk.deploymentsCalls.Load(); got != tc.wantDeploymentsCall {
		t.Fatalf("deployments calls = %d, want %d", got, tc.wantDeploymentsCall)
	}
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
	dir := t.TempDir()
	summaryPath := filepath.Join(dir, "summary.md")
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
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s) = %v", dir, err)
	}
	summaryBytes, readErr := root.ReadFile("summary.md")
	closeErr := root.Close()
	if readErr != nil {
		t.Fatalf("root.ReadFile(summary.md) = %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("root.Close() = %v", closeErr)
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
