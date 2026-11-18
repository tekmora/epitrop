// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
