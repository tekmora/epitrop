// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const (
	seqSHAA  = "1111111111111111111111111111111111111111"
	seqSHAB  = "2222222222222222222222222222222222222222"
	seqSHAC  = "3333333333333333333333333333333333333333"
	seqSHAD  = "4444444444444444444444444444444444444444"
	maxSteps = 16
)

type workflowSpec struct {
	Concurrency workflowConcurrency        `yaml:"concurrency"`
	Jobs        map[string]workflowJobSpec `yaml:"jobs"`
}

type workflowConcurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress string `yaml:"cancel-in-progress"`
	Queue            string `yaml:"queue"`
}

type workflowJobSpec struct {
	Concurrency workflowConcurrency `yaml:"concurrency"`
}

func validatePagesWorkflow(raw []byte) error {
	var wf workflowSpec
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return fmt.Errorf("decode workflow yaml: %w", err)
	}
	if err := validateWorkflowConcurrency(wf.Concurrency); err != nil {
		return fmt.Errorf("workflow concurrency: %w", err)
	}
	deploy, ok := wf.Jobs["deploy"]
	if !ok {
		return errors.New("missing deploy job")
	}
	if err := validateDeployConcurrency(deploy.Concurrency); err != nil {
		return fmt.Errorf("deploy concurrency: %w", err)
	}
	return nil
}

func validateWorkflowConcurrency(c workflowConcurrency) error {
	if !strings.Contains(c.Group, "github.run_id") {
		return fmt.Errorf("group %q does not contain github.run_id", c.Group)
	}
	if strings.Contains(c.Group, "github.ref") {
		return fmt.Errorf("group %q must not contain github.ref", c.Group)
	}
	if c.Queue != "" {
		return fmt.Errorf("queue %q must be absent at workflow level", c.Queue)
	}
	return nil
}

func validateDeployConcurrency(c workflowConcurrency) error {
	if c.Group != "github-pages-deploy" {
		return fmt.Errorf("group = %q, want github-pages-deploy", c.Group)
	}
	if c.Queue != "max" {
		return fmt.Errorf("queue = %q, want max", c.Queue)
	}
	if c.CancelInProgress != "" && c.CancelInProgress != "false" {
		return fmt.Errorf("cancel-in-progress = %q, want absent or false", c.CancelInProgress)
	}
	return nil
}

type workflowCheckCase struct {
	name    string
	yaml    string
	wantErr string
}

func TestWorkflowQueuesEveryDeploy(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/pages.yml")
	if err != nil {
		t.Fatalf("read pages.yml: %v", err)
	}
	if err := validatePagesWorkflow(raw); err != nil {
		t.Fatalf("validatePagesWorkflow(pages.yml) = %v, want nil", err)
	}
	cases := workflowNegativeCases()
	for i := 0; i < len(cases) && i < maxTestCases; i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			checkWorkflowCase(t, tc)
		})
	}
}

func checkWorkflowCase(t *testing.T, tc workflowCheckCase) {
	t.Helper()
	err := validatePagesWorkflow([]byte(tc.yaml))
	if tc.wantErr == "" {
		if err != nil {
			t.Fatalf("validatePagesWorkflow() = %v, want nil", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
		t.Fatalf("validatePagesWorkflow() = %v, want substring %q", err, tc.wantErr)
	}
}

func workflowNegativeCases() []workflowCheckCase {
	return []workflowCheckCase{
		{
			name: "valid inline workflow passes",
			yaml: "concurrency:\n  group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.run_id }}\n" +
				"  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n" +
				"jobs:\n  deploy:\n    concurrency:\n      group: github-pages-deploy\n      cancel-in-progress: false\n      queue: max\n",
			wantErr: "",
		},
		{
			name: "deploy queue single fails",
			yaml: "concurrency:\n  group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.run_id }}\n" +
				"jobs:\n  deploy:\n    concurrency:\n      group: github-pages-deploy\n      cancel-in-progress: false\n      queue: single\n",
			wantErr: "queue = \"single\", want max",
		},
		{
			name: "deploy queue absent fails",
			yaml: "concurrency:\n  group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.run_id }}\n" +
				"jobs:\n  deploy:\n    concurrency:\n      group: github-pages-deploy\n      cancel-in-progress: false\n",
			wantErr: "queue = \"\", want max",
		},
		{
			name: "deploy cancel-in-progress true fails",
			yaml: "concurrency:\n  group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.run_id }}\n" +
				"jobs:\n  deploy:\n    concurrency:\n      group: github-pages-deploy\n      cancel-in-progress: true\n      queue: max\n",
			wantErr: "cancel-in-progress = \"true\", want absent or false",
		},
		{
			name: "workflow group with github.ref fails",
			yaml: "concurrency:\n  group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.ref }}\n" +
				"jobs:\n  deploy:\n    concurrency:\n      group: github-pages-deploy\n      queue: max\n",
			wantErr: "github.run_id",
		},
		{
			name: "workflow level queue max fails",
			yaml: "concurrency:\n  group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.run_id }}\n  queue: max\n" +
				"jobs:\n  deploy:\n    concurrency:\n      group: github-pages-deploy\n      queue: max\n",
			wantErr: "must be absent at workflow level",
		},
	}
}

type queueMode int

const (
	queueMax queueMode = iota
	queueSingle
)

type statefulGitHub struct {
	order        map[string]int
	nextID       int64
	deployments  []deployment
	statusesByID map[int64][]deploymentStatus
	liveSHA      string
}

func newStatefulGitHub() *statefulGitHub {
	return &statefulGitHub{
		order: map[string]int{
			seqSHAA: 0,
			seqSHAB: 1,
			seqSHAC: 2,
			seqSHAD: 3,
		},
		statusesByID: make(map[int64][]deploymentStatus, maxSteps),
	}
}

func (s *statefulGitHub) recordDeployment(sha, state string) {
	s.nextID++
	id := s.nextID
	dep := deployment{
		ID:        id,
		SHA:       sha,
		CreatedAt: fmt.Sprintf("2026-11-18T12:%02d:00Z", id%60),
	}
	s.deployments = append([]deployment{dep}, s.deployments...)
	s.statusesByID[id] = []deploymentStatus{{State: state}}
	if state == "success" {
		s.liveSHA = sha
	}
}

func newStatefulGitHubServer(t *testing.T, s *statefulGitHub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validGitHubRequestHeaders(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.serve(t, w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func validGitHubRequestHeaders(r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+testAccessVal {
		return false
	}
	return r.Header.Get("Accept") == "application/vnd.github+json" &&
		r.Header.Get("X-GitHub-Api-Version") == "2022-11-28"
}

func (s *statefulGitHub) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	const prefix = "/repos/" + testRepo
	switch {
	case r.URL.Path == prefix+"/deployments":
		page, ok := parseDeploymentsQuery(r)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		serveDeploymentPage(t, w, s.deployments, page)
	case strings.HasPrefix(r.URL.Path, prefix+"/deployments/") && strings.HasSuffix(r.URL.Path, "/statuses"):
		s.serveStatuses(t, w, r)
	case strings.HasPrefix(r.URL.Path, prefix+"/compare/"):
		s.serveCompare(t, w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *statefulGitHub) serveStatuses(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var id int64
	if _, err := fmt.Sscanf(r.URL.Path, "/repos/"+testRepo+"/deployments/%d/statuses", &id); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeJSON(t, w, s.statusesByID[id])
}

func (s *statefulGitHub) serveCompare(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	pair := strings.TrimPrefix(r.URL.Path, "/repos/"+testRepo+"/compare/")
	base, head, ok := strings.Cut(pair, "...")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	baseIdx, okBase := s.order[base]
	headIdx, okHead := s.order[head]
	if !okBase || !okHead {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(t, w, compareResponse{Status: compareOrderStatus(baseIdx, headIdx)})
}

func compareOrderStatus(baseIdx, headIdx int) string {
	if headIdx > baseIdx {
		return "ahead"
	}
	if headIdx == baseIdx {
		return "identical"
	}
	return "behind"
}

type sequenceCase struct {
	name        string
	mode        queueMode
	running     string
	arrivals    []string
	wantLive    string
	wantRefused []string
}

func TestQueueSequenceDeploysNewest(t *testing.T) {
	cases := []sequenceCase{
		{
			name:        "finding sequence under queue max deploys C and refuses B",
			mode:        queueMax,
			running:     seqSHAA,
			arrivals:    []string{seqSHAC, seqSHAB},
			wantLive:    seqSHAC,
			wantRefused: []string{seqSHAB},
		},
		{
			name:        "finding sequence under queue single replaces C with B leaving live at B",
			mode:        queueSingle,
			running:     seqSHAA,
			arrivals:    []string{seqSHAC, seqSHAB},
			wantLive:    seqSHAB,
			wantRefused: nil,
		},
		{
			name:        "deploy-only re-run of A while D pending under queue max refuses A and deploys D",
			mode:        queueMax,
			running:     seqSHAC,
			arrivals:    []string{seqSHAD, seqSHAA},
			wantLive:    seqSHAD,
			wantRefused: []string{seqSHAA},
		},
		{
			name:        "deploy-only re-run of A while D pending under queue single replaces D and leaves live at C",
			mode:        queueSingle,
			running:     seqSHAC,
			arrivals:    []string{seqSHAD, seqSHAA},
			wantLive:    seqSHAC,
			wantRefused: []string{seqSHAA},
		},
		{
			name:        "boundary newest commit arrives first older arrives last under queue max keeps C",
			mode:        queueMax,
			running:     seqSHAC,
			arrivals:    []string{seqSHAA},
			wantLive:    seqSHAC,
			wantRefused: []string{seqSHAA},
		},
	}
	for i := 0; i < len(cases) && i < maxSteps; i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			runSequenceCase(t, tc)
		})
	}
}

func runSequenceCase(t *testing.T, tc sequenceCase) {
	t.Helper()
	gh := newStatefulGitHub()
	srv := newStatefulGitHubServer(t, gh)
	schedule := buildQueueSchedule(tc.mode, tc.running, tc.arrivals)
	var refused []string
	for i := 0; i < len(schedule) && i < maxSteps; i++ {
		sha := schedule[i]
		allow := runDeployStep(t, srv.URL, gh, sha)
		if !allow {
			refused = append(refused, sha)
		}
	}
	if gh.liveSHA != tc.wantLive {
		t.Fatalf("final live SHA = %s, want %s", gh.liveSHA, tc.wantLive)
	}
	if !slices.Equal(refused, tc.wantRefused) {
		t.Fatalf("refused = %v, want %v", refused, tc.wantRefused)
	}
}

func buildQueueSchedule(mode queueMode, running string, arrivals []string) []string {
	var pending []string
	for i := 0; i < len(arrivals) && i < maxSteps; i++ {
		if mode == queueSingle {
			pending = []string{arrivals[i]}
		} else {
			pending = append(pending, arrivals[i])
		}
	}
	schedule := make([]string, 0, 1+len(pending))
	if running != "" {
		schedule = append(schedule, running)
	}
	for i := 0; i < len(pending) && i < maxSteps; i++ {
		schedule = append(schedule, pending[i])
	}
	return schedule
}

func runDeployStep(t *testing.T, apiBase string, gh *statefulGitHub, sha string) bool {
	t.Helper()
	cfg := config{
		APIBase:    apiBase,
		Repository: testRepo,
		SHA:        sha,
		Ref:        "refs/heads/main",
		Access:     testAccessVal,
	}
	allow, _, err := decide(context.Background(), cfg)
	if err != nil {
		t.Fatalf("decide(%s) unexpected error: %v", sha, err)
	}
	if allow {
		gh.recordDeployment(sha, "success")
		return true
	}
	gh.recordDeployment(sha, "failure")
	return false
}
