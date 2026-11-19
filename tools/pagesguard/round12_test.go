// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunRejectsWhitespaceRef(t *testing.T) {
	fk := &fakeGitHub{}
	srv := newFakeGitHubServer(t, fk)
	env := map[string]string{"GITHUB_API_URL": srv.URL, "GITHUB_REPOSITORY": testRepo, "GITHUB_SHA": testOurSHA, "GH_TOKEN": testAccessVal}
	for _, ref := range []string{"refs/heads/main ", " refs/heads/main", "refs/heads/main\t", "refs/heads/main\n"} {
		env["GITHUB_REF"] = ref
		err := run(context.Background(), func(k string) string { return env[k] }, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "surrounding whitespace") {
			t.Errorf("ref %q: %v", ref, err)
		}
	}
	if fk.deploymentsCalls.Load()+fk.statusesCalls.Load()+fk.compareCalls.Load()+fk.tipCalls.Load() != 0 {
		t.Fatal("unexpected API requests")
	}
	env["GITHUB_REF"] = "refs/heads/main"
	if _, err := configFromEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
}

func TestStatusHistoryBoundary(t *testing.T) {
	for _, n := range []int{99, 100} {
		statuses := make([]deploymentStatus, n)
		fk := &fakeGitHub{deployments: []deployment{{ID: 2, SHA: testOlderSHA, CreatedAt: "2026-11-18T13:00:00Z"}, {ID: 1, SHA: testOurSHA, CreatedAt: "2026-11-18T12:00:00Z"}}, statusesByID: map[int64][]deploymentStatus{2: statuses, 1: {{State: "success"}}}}
		srv := newFakeGitHubServer(t, fk)
		allow, _, err := decide(context.Background(), config{APIBase: srv.URL, Repository: testRepo, SHA: testOurSHA, Ref: "refs/heads/main", Access: testAccessVal})
		if n == 100 && (err == nil || !strings.Contains(err.Error(), "deployment 2") || allow) {
			t.Errorf("100 statuses: allow %v err %v", allow, err)
		}
		if n == 99 && (err != nil || !allow) {
			t.Errorf("99 statuses: allow %v err %v", allow, err)
		}
	}
}

func TestBatchOrdering(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + testRepo + "/deployments/2/statuses":
			select {
			case <-released:
			case <-r.Context().Done():
				return
			}
			writeJSON(t, w, []deploymentStatus{{State: "success"}})
		case "/repos/" + testRepo + "/deployments/6/statuses":
			writeJSON(t, w, []deploymentStatus{{State: "success"}})
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush: %v", err)
			}
			close(released)
		default:
			writeJSON(t, w, []deploymentStatus{})
		}
	}))
	defer srv.Close()
	deps := make([]deployment, 10)
	for i := 0; i < len(deps); i++ {
		deps[i] = deployment{ID: int64(i), SHA: fmt.Sprintf("sha%d", i)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sha, found, err := findLiveDeploymentSHA(ctx, srv.URL, testRepo, http.Header{}, deps)
	if err != nil || !found || sha != "sha2" {
		t.Fatalf("sha %q found %v err %v", sha, found, err)
	}
}

func TestBatchEarlierError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/1/statuses") {
			http.Error(w, "failure", http.StatusInternalServerError)
			return
		}
		if strings.Contains(r.URL.Path, "/3/statuses") {
			writeJSON(t, w, []deploymentStatus{{State: "success"}})
			return
		}
		writeJSON(t, w, []deploymentStatus{})
	}))
	defer srv.Close()
	deps := make([]deployment, 8)
	for i := 0; i < len(deps); i++ {
		deps[i] = deployment{ID: int64(i), SHA: fmt.Sprintf("sha%d", i)}
	}
	_, found, err := findLiveDeploymentSHA(context.Background(), srv.URL, testRepo, http.Header{}, deps)
	if err == nil || found || !strings.Contains(err.Error(), "deployment 1") {
		t.Fatalf("found %v err %v", found, err)
	}
}

func TestStatusRequestDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/deployments") {
			writeJSON(t, w, []deployment{{ID: 1, SHA: testOurSHA, CreatedAt: "2026-11-18T13:00:00Z"}, {ID: 2, SHA: "a", CreatedAt: "2026-11-18T12:00:00Z"}})
			return
		}
		if strings.Contains(r.URL.Path, "/1/statuses") {
			writeJSON(t, w, []deploymentStatus{{State: "success"}})
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	allow, _, err := decide(ctx, config{APIBase: srv.URL, Repository: testRepo, SHA: testOurSHA, Ref: "refs/heads/main", Access: testAccessVal})
	if allow || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("allow %v err %v", allow, err)
	}
}
