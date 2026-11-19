// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"net/http"
	"strings"
	"testing"
)

func makePageFourSuccessDeployments() ([]deployment, map[int64][]deploymentStatus) {
	deps, statuses := makeFailedDeployments(maxDeployments)
	deps = append(deps, deployment{
		ID:        1,
		SHA:       testLiveSHA,
		CreatedAt: "2026-11-18T12:00:00Z",
	})
	statuses[1] = []deploymentStatus{{State: "success"}}
	return deps, statuses
}

func TestDecideBeyondWindowRecovery(t *testing.T) {
	depsPage4, statPage4 := makePageFourSuccessDeployments()
	cases := []decideCase{
		{
			name: "positive page four success recovers when commit is branch tip without fetching page four",
			fk: &fakeGitHub{
				deployments:  depsPage4,
				statusesByID: statPage4,
				tipByRef:     map[string]string{"heads/main": testOurSHA},
			},
			wantAllow: true,
			wantReason: []string{
				"live deployment not among the newest 300; commit " + testOurSHA + " is the tip of refs/heads/main",
			},
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
			wantCompareCall:     0,
		},
		{
			name: "positive page four success with slashed branch ref escapes each segment",
			ref:  "refs/heads/release/v1",
			fk: &fakeGitHub{
				deployments:  depsPage4,
				statusesByID: statPage4,
				tipByRef:     map[string]string{"heads/release/v1": testOurSHA},
			},
			wantAllow: true,
			wantReason: []string{
				"live deployment not among the newest 300; commit " + testOurSHA + " is the tip of refs/heads/release/v1",
			},
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
			wantCompareCall:     0,
		},
		{
			name:    "branch with a reserved character fails closed",
			ref:     "refs/heads/feat/a#b",
			fk:      &fakeGitHub{deployments: depsPage4, statusesByID: statPage4},
			wantErr: "invalid branch segment",
		},
	}
	runDecideCases(t, cases)
}

func TestDecideBeyondWindowRecoveryRefusals(t *testing.T) {
	depsPage4, statPage4 := makePageFourSuccessDeployments()
	cases := []decideCase{
		{
			name: "negative slashed branch names its own branch in the hint",
			ref:  "refs/heads/release/v1",
			fk: &fakeGitHub{
				deployments:  depsPage4,
				statusesByID: statPage4,
				tipByRef:     map[string]string{"heads/release/v1": testLiveSHA},
			},
			wantAllow:           false,
			wantReason:          []string{"re-run the newest Pages run on release/v1 or dispatch the workflow on release/v1"},
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
		},
		{
			name: "negative page four success refuses when commit is not branch tip and names both shas",
			fk: &fakeGitHub{
				deployments:  depsPage4,
				statusesByID: statPage4,
				tipByRef:     map[string]string{"heads/main": testLiveSHA},
			},
			wantAllow: false,
			wantReason: []string{
				"live deployment not among the newest 300 and commit " + testOurSHA + " is not the tip of refs/heads/main (" + testLiveSHA + ")",
				"re-run the newest Pages run on main or dispatch the workflow on main",
			},
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
			wantCompareCall:     0,
		},
	}
	runDecideCases(t, cases)
}

func TestDecideBeyondWindowBoundary(t *testing.T) {
	deps300, stat300 := makeFailedDeployments(300)
	deps299, stat299 := makeFailedDeployments(299)
	cases := []decideCase{
		{
			name: "boundary exactly 300 failed deployments allows when commit is branch tip",
			fk: &fakeGitHub{
				deployments:  deps300,
				statusesByID: stat300,
				tipByRef:     map[string]string{"heads/main": testOurSHA},
			},
			wantAllow: true,
			wantReason: []string{
				"live deployment not among the newest 300; commit " + testOurSHA + " is the tip of refs/heads/main",
			},
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
			wantCompareCall:     0,
		},
		{
			name: "boundary 299 failed deployments short last page allows without tip call",
			fk: &fakeGitHub{
				deployments:  deps299,
				statusesByID: stat299,
			},
			wantAllow:           true,
			wantReason:          []string{"no earlier successful deployment"},
			wantDeploymentsCall: 3,
			wantStatusesCall:    299,
			wantTipCall:         0,
			wantCompareCall:     0,
		},
	}
	runDecideCases(t, cases)
}

func TestDecideBeyondWindowFailClosed(t *testing.T) {
	depsPage4, statPage4 := makePageFourSuccessDeployments()
	cases := []decideCase{
		{
			name: "tip endpoint HTTP 500 fails closed",
			fk: &fakeGitHub{
				deployments:  depsPage4,
				statusesByID: statPage4,
				tipStatus:    http.StatusInternalServerError,
			},
			wantErr:             "fetch tip of refs/heads/main",
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
		},
		{
			name: "tip endpoint HTTP 404 unknown ref fails closed",
			fk: &fakeGitHub{
				deployments:  depsPage4,
				statusesByID: statPage4,
			},
			wantErr:             "fetch tip of refs/heads/main",
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
		},
	}
	runDecideCases(t, cases)
}

func TestDecideBeyondWindowInvalidTip(t *testing.T) {
	depsPage4, statPage4 := makePageFourSuccessDeployments()
	cases := []decideCase{
		{
			name: "tip endpoint malformed JSON fails closed",
			fk: &fakeGitHub{
				deployments:  depsPage4,
				statusesByID: statPage4,
				tipBadJSON:   true,
			},
			wantErr:             "decode tip of refs/heads/main",
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
		},
		{
			name:                "tip endpoint wrong JSON type fails closed",
			fk:                  &fakeGitHub{deployments: depsPage4, statusesByID: statPage4, tipWrongType: true},
			wantErr:             "decode tip of refs/heads/main",
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
		},
		{
			name: "tip endpoint empty sha fails closed",
			fk: &fakeGitHub{
				deployments:  depsPage4,
				statusesByID: statPage4,
				tipByRef:     map[string]string{"heads/main": " "},
			},
			wantErr:             "empty",
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
		},
		{
			name: "ref at the segment bound reaches the tip endpoint",
			ref:  "refs/heads/" + strings.Repeat("a/", maxRefSegments-1) + "a",
			fk: &fakeGitHub{
				deployments:  depsPage4,
				statusesByID: statPage4,
				tipByRef:     map[string]string{"heads/" + strings.Repeat("a/", maxRefSegments-1) + "a": testOurSHA},
			},
			wantAllow:           true,
			wantReason:          []string{"is the tip of refs/heads/" + strings.Repeat("a/", maxRefSegments-1) + "a"},
			wantDeploymentsCall: 3,
			wantStatusesCall:    300,
			wantTipCall:         1,
		},
	}
	runDecideCases(t, cases)
}
