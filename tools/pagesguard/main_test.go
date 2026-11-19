// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type decideCase struct {
	name                string
	sha                 string
	ref                 string
	fk                  *fakeGitHub
	wantAllow           bool
	wantReason          []string
	wantErr             string
	wantDeploymentsCall int32
	wantStatusesCall    int32
	wantCompareCall     int32
	wantTipCall         int32
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
			wantStatusesCall:    2,
			wantCompareCall:     1,
		},
	}
	runDecideCases(t, cases)
}

func TestDecideSortsShuffledDeployments(t *testing.T) {
	cases := []decideCase{
		{
			name: "response order shuffled is sorted by created_at desc then id desc",
			fk: &fakeGitHub{
				deployments: []deployment{
					{ID: 4, SHA: testOlderSHA, CreatedAt: "2026-11-18T10:00:00Z"},
					{ID: 2, SHA: testOlderSHA, CreatedAt: "2026-11-18T12:00:00Z"},
					{ID: 3, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"},
				},
				statusesByID: map[int64][]deploymentStatus{
					3: {{State: "success"}},
					2: {{State: "success"}},
					4: {{State: "success"}},
				},
				compareByPair: map[string]string{testLiveSHA + "..." + testOurSHA: "ahead"},
			},
			wantAllow:           true,
			wantReason:          []string{"ahead", testLiveSHA},
			wantDeploymentsCall: 1,
			wantStatusesCall:    3,
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
			fk:                  &fakeGitHub{deployments: deps300, statusesByID: stat300, tipByRef: map[string]string{"heads/main": testLiveSHA}},
			wantAllow:           false,
			wantReason:          []string{"live deployment not among the newest 300"},
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
		},
		{
			name:                "boundary over 300 deployments makes at most 3 page requests and 300 statuses requests",
			fk:                  &fakeGitHub{deployments: deps305, statusesByID: stat305, tipByRef: map[string]string{"heads/main": testLiveSHA}},
			wantAllow:           false,
			wantReason:          []string{"live deployment not among the newest 300"},
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
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
			name:                "successful deployment with empty sha returns error",
			fk:                  &fakeGitHub{deployments: []deployment{{ID: 1, SHA: " ", CreatedAt: "2026-11-18T12:00:00Z"}}, statusesByID: liveSuccess},
			wantErr:             "deployment 1 has an empty sha",
			wantDeploymentsCall: 1,
			wantStatusesCall:    1,
		},
		{name: "missing GITHUB_SHA returns error", sha: " ", fk: &fakeGitHub{}, wantErr: "GITHUB_SHA is required"},
		{name: "missing GITHUB_REF returns error", ref: " ", fk: &fakeGitHub{}, wantErr: "GITHUB_REF is required"},
		{name: "non-branch GITHUB_REF returns error", ref: "refs/tags/v1", fk: &fakeGitHub{}, wantErr: `GITHUB_REF "refs/tags/v1" is not a branch`},
	}
	runDecideCases(t, cases)
}

func TestDecideDecodeErrors(t *testing.T) {
	liveDep := []deployment{{ID: 1, SHA: testLiveSHA, CreatedAt: "2026-11-18T12:00:00Z"}}
	liveSuccess := map[int64][]deploymentStatus{1: {{State: "success"}}}
	deps150, stat150 := makeFailedDeployments(150)
	cases := []decideCase{
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
	ref := "refs/heads/main"
	if tc.ref != "" {
		ref = tc.ref
	}
	cfg := config{
		APIBase:    srv.URL,
		Repository: testRepo,
		SHA:        sha,
		Ref:        ref,
		Access:     testAccessVal,
	}
	allow, reason, err := decide(context.Background(), cfg)
	checkDecideOutcome(t, tc, allow, reason, err)
	checkDecideCalls(t, tc)
}

func checkDecideOutcome(t *testing.T, tc decideCase, allow bool, reason string, err error) {
	t.Helper()
	if tc.wantErr != "" {
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) || allow {
			t.Fatalf("decide() allow = %v, err = %v, want false with substring %q", allow, err, tc.wantErr)
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
	if got := tc.fk.tipCalls.Load(); got != tc.wantTipCall {
		t.Fatalf("tip calls = %d, want %d", got, tc.wantTipCall)
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
		"GITHUB_REF":          "refs/heads/main",
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
		"GITHUB_REF":        "refs/heads/main",
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
