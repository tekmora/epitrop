// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"strings"
	"testing"
)

func TestValidateBranchRef(t *testing.T) {
	cases := []struct {
		ref   string
		valid bool
	}{
		{"refs/heads/main", true}, {"refs/heads/release/v1", true},
		{"refs/heads/a_b-c.d", true}, {"refs/heads/" + strings.Repeat("a/", maxRefSegments-1) + "a", true},
		{"refs/heads/", false}, {"refs/heads/topic/../main", false}, {"refs/heads/./main", false},
		{"refs/heads/a//b", false}, {"refs/heads/a/", false}, {"refs/heads/.hidden", false},
		{"refs/heads/x.lock", false}, {"refs/heads/a%2Fb", false}, {"refs/heads/a b", false},
		{"refs/heads/" + strings.Repeat("a/", maxRefSegments) + "a", false},
		{"refs/pull/1/merge", false}, {"refs/tags/v1", false},
	}
	for i := 0; i < len(cases) && i < maxTestCases; i++ {
		tc := cases[i]
		t.Run(tc.ref, func(t *testing.T) {
			cfg := config{APIBase: "http://example.test", Repository: testRepo, SHA: testOurSHA, Ref: tc.ref, Access: testAccessVal}
			if err := validateConfig(cfg); (err == nil) != tc.valid {
				t.Errorf("validateConfig(%q) = %v, valid = %v", tc.ref, err, tc.valid)
			}
			if strings.HasPrefix(tc.ref, "refs/heads/") {
				if err := validateBranchRef(tc.ref); (err == nil) != tc.valid {
					t.Errorf("validateBranchRef(%q) = %v, valid = %v", tc.ref, err, tc.valid)
				}
			}
		})
	}
}

func TestDecideInvalidRefBeforeRequests(t *testing.T) {
	deps, statuses := makeFailedDeployments(1)
	cases := []decideCase{
		{name: "dot segment", ref: "refs/heads/topic/../main", fk: &fakeGitHub{deployments: deps, statusesByID: statuses}, wantErr: "invalid branch segment"},
		{name: "over segment bound", ref: "refs/heads/" + strings.Repeat("a/", maxRefSegments) + "a", fk: &fakeGitHub{deployments: deps, statusesByID: statuses}, wantErr: "more than 32 segments"},
	}
	runDecideCases(t, cases)
}
