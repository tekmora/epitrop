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
		{"refs/heads/main ", false}, {" refs/heads/main", false},
		{"refs/heads/main\t", false}, {"refs/heads/main\n", false},
		{"refs/heads/" + strings.Repeat("a", maxRefBytes-len("refs/heads/")), true},
		{"refs/heads/" + strings.Repeat("a", maxRefBytes+1-len("refs/heads/")), false},
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
		{name: "over byte bound", ref: "refs/heads/" + strings.Repeat("a", maxRefBytes), fk: &fakeGitHub{deployments: deps, statusesByID: statuses}, wantErr: "more than 255 bytes"},
		{name: "surrounding whitespace", ref: "refs/heads/main ", fk: &fakeGitHub{deployments: deps, statusesByID: statuses}, wantErr: "surrounding whitespace"},
	}
	runDecideCases(t, cases)
}

func TestValidateConfigRefWhitespaceErrors(t *testing.T) {
	cases := []struct{ ref, want string }{
		{"", "GITHUB_REF is required"}, {" \t\n", "GITHUB_REF is required"},
		{"refs/heads/main ", `GITHUB_REF "refs/heads/main " has surrounding whitespace`},
	}
	for i := 0; i < len(cases) && i < maxTestCases; i++ {
		tc := cases[i]
		cfg := config{Repository: testRepo, SHA: testOurSHA, Ref: tc.ref}
		if err := validateConfig(cfg); err == nil || err.Error() != tc.want {
			t.Errorf("validateConfig(%q) = %v, want %q", tc.ref, err, tc.want)
		}
	}
}

func TestFetchBranchTipBoundAndEscaping(t *testing.T) {
	cases := []struct {
		name, branch, wantErr string
		wantCalls             int32
	}{
		{"too many segments", strings.Repeat("a/", maxRefSegments) + "a", "more than 32 segments", 0},
		{"segment limit", strings.Repeat("a/", maxRefSegments-1) + "a", "", 1},
		{"escaped question mark", "topic/a?b", "", 1},
	}
	for i := 0; i < len(cases) && i < maxTestCases; i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			fk := &fakeGitHub{tipByRef: map[string]string{"heads/" + tc.branch: testOurSHA}}
			srv := newFakeGitHubServer(t, fk)
			tip, err := fetchBranchTip(t.Context(), srv.URL, testRepo, githubHeader(testAccessVal), "refs/heads/"+tc.branch)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("fetchBranchTip() err = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil || tip != testOurSHA {
				t.Fatalf("fetchBranchTip() = %q, %v; want %q", tip, err, testOurSHA)
			}
			if got := fk.tipCalls.Load(); got != tc.wantCalls {
				t.Errorf("tip calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}
