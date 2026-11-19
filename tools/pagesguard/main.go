// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

// Command pagesguard refuses to deploy a GitHub Pages artifact from a commit
// older than the current live github-pages deployment.
//
// Deploy jobs queue FIFO without replacement (queue: max), but the queue
// orders by arrival, not commit order, and a workflow or deploy-only re-run
// reuses its run's GITHUB_SHA; an out-of-order or re-run deploy could
// otherwise overwrite the live site with an older artifact.
//
// Refusing fails the deploy job before actions/deploy-pages runs so GitHub
// marks the job's github-pages environment deployment failed; a step-level
// skip would leave the environment deployment marked success without having
// deployed. When no successful deployment sits within the bounded 300-record
// lookup window, pagesguard allows the deploy only if the run's commit matches
// the current tip of the run's branch.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/tekmora/epitrop/internal/sources/httpfetch"
)

const (
	defaultAPIBase     = "https://api.github.com"
	deploymentsPerPage = 100
	// List deployments caps per_page at 100 (default 30, page default 1).
	// With maxDeploymentPages = 3 (bound 300 records), worst case is 3 list
	// calls + 300 status calls + 1 ref or compare call = 304 requests, still
	// below the 1,000 requests per hour per repository GITHUB_TOKEN rate limit
	// for one run.
	maxDeploymentPages = 3
	maxDeployments     = deploymentsPerPage * maxDeploymentPages
	maxStatuses        = 100
	maxRefSegments     = 32
	maxResponseBytes   = 4 << 20
	requestTimeout     = 20 * time.Second
	// The compare response lists up to 300 changed files with their patches
	// even with per_page=1 (GitHub REST compare, "Working with large
	// comparisons"), so a large gap between live and ours needs more room
	// than a deployments page; a cap hit fails closed.
	maxCompareBytes = 32 << 20
)

type config struct {
	APIBase     string
	Repository  string
	SHA         string
	Ref         string
	Access      string
	StepSummary string
}

type deployment struct {
	ID        int64  `json:"id"`
	SHA       string `json:"sha"`
	CreatedAt string `json:"created_at"`
}

type deploymentStatus struct {
	State string `json:"state"`
}

type compareResponse struct {
	Status string `json:"status"`
}

func main() {
	if err := run(context.Background(), os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "pagesguard:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, out io.Writer) error {
	cfg, err := configFromEnv(getenv)
	if err != nil {
		return err
	}
	allow, reason, err := decide(ctx, cfg)
	if err != nil {
		return err
	}
	status := "allow"
	if !allow {
		status = "refuse"
	}
	line := fmt.Sprintf("pagesguard: %s: %s\n", status, reason)
	if _, err := fmt.Fprint(out, line); err != nil {
		return fmt.Errorf("write outcome: %w", err)
	}
	if cfg.StepSummary != "" {
		if err := appendSummary(cfg.StepSummary, line); err != nil {
			return err
		}
	}
	if !allow {
		return fmt.Errorf("refused: %s", reason)
	}
	return nil
}

func appendSummary(path string, line string) error {
	// #nosec G304 -- GITHUB_STEP_SUMMARY is supplied by the GitHub Actions runner environment.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open step summary %s: %w", path, err)
	}
	_, err = file.WriteString(line)
	if closeErr := file.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write step summary %s: %w", path, err)
	}
	return nil
}

func configFromEnv(getenv func(string) string) (config, error) {
	apiBase := strings.TrimSpace(getenv("GITHUB_API_URL"))
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	cfg := config{
		APIBase:     strings.TrimRight(apiBase, "/"),
		Repository:  strings.TrimSpace(getenv("GITHUB_REPOSITORY")),
		SHA:         strings.TrimSpace(getenv("GITHUB_SHA")),
		Ref:         strings.TrimSpace(getenv("GITHUB_REF")),
		Access:      strings.TrimSpace(getenv("GH_TOKEN")),
		StepSummary: strings.TrimSpace(getenv("GITHUB_STEP_SUMMARY")),
	}
	if err := validateConfig(cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg config) error {
	if strings.TrimSpace(cfg.Repository) == "" {
		return errors.New("GITHUB_REPOSITORY is required")
	}
	if strings.TrimSpace(cfg.SHA) == "" {
		return errors.New("GITHUB_SHA is required")
	}
	ref := strings.TrimSpace(cfg.Ref)
	if ref == "" {
		return errors.New("GITHUB_REF is required")
	}
	if !strings.HasPrefix(ref, "refs/heads/") {
		return fmt.Errorf("GITHUB_REF %q is not a branch", ref)
	}
	if strings.TrimSpace(cfg.Access) == "" {
		return errors.New("GH_TOKEN is required")
	}
	if strings.TrimSpace(cfg.APIBase) == "" {
		return errors.New("GITHUB_API_URL is required")
	}
	return nil
}

func decide(ctx context.Context, cfg config) (allow bool, reason string, err error) {
	if err := validateConfig(cfg); err != nil {
		return false, "", err
	}
	apiBase := strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/")
	header := githubHeader(cfg.Access)
	deployments, err := fetchDeployments(ctx, apiBase, cfg.Repository, header)
	if err != nil {
		return false, "", err
	}
	liveSHA, found, err := findLiveDeploymentSHA(ctx, apiBase, cfg.Repository, header, deployments)
	if err != nil {
		return false, "", err
	}
	if !found {
		if len(deployments) >= maxDeployments {
			return decideBeyondWindow(ctx, apiBase, header, cfg)
		}
		return true, "no earlier successful deployment", nil
	}
	if liveSHA == cfg.SHA {
		return true, fmt.Sprintf("live deployment is already at %s", cfg.SHA), nil
	}
	return compareWithLive(ctx, apiBase, cfg.Repository, header, liveSHA, cfg.SHA)
}

// decideBeyondWindow decides when the bounded window is full and holds no
// successful deployment: only the tip of the run's branch may deploy.
func decideBeyondWindow(ctx context.Context, apiBase string, header http.Header, cfg config) (bool, string, error) {
	ref := strings.TrimSpace(cfg.Ref)
	tip, err := fetchBranchTip(ctx, apiBase, cfg.Repository, header, ref)
	if err != nil {
		return false, "", err
	}
	if tip == cfg.SHA {
		return true, fmt.Sprintf("live deployment not among the newest 300; commit %s is the tip of %s", cfg.SHA, ref), nil
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	return false, fmt.Sprintf(
		"live deployment not among the newest 300 and commit %s is not the tip of %s (%s): re-run the newest Pages run on %s or dispatch the workflow on %s",
		cfg.SHA, ref, tip, branch, branch,
	), nil
}

func fetchBranchTip(ctx context.Context, apiBase, repo string, header http.Header, ref string) (string, error) {
	branch := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	segments := strings.Split(branch, "/")
	if len(segments) > maxRefSegments {
		return "", fmt.Errorf("ref %s has more than %d segments", ref, maxRefSegments)
	}
	for i := 0; i < len(segments) && i < maxRefSegments; i++ {
		segments[i] = url.PathEscape(segments[i])
	}
	endpoint := fmt.Sprintf("%s/repos/%s/git/ref/heads/%s", apiBase, repo, strings.Join(segments, "/"))
	body, err := httpfetch.GetWithHeader(ctx, endpoint, header, requestTimeout, maxResponseBytes)
	if err != nil {
		return "", fmt.Errorf("fetch tip of %s: %w", ref, err)
	}
	var payload struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode tip of %s: %w", ref, err)
	}
	sha := strings.TrimSpace(payload.Object.SHA)
	if sha == "" {
		return "", fmt.Errorf("ref %s has an empty tip sha", ref)
	}
	return sha, nil
}

func githubHeader(access string) http.Header {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+access)
	header.Set("Accept", "application/vnd.github+json")
	header.Set("X-GitHub-Api-Version", "2022-11-28")
	return header
}

func fetchDeployments(ctx context.Context, apiBase, repo string, header http.Header) ([]deployment, error) {
	var deployments []deployment
	for page := 1; page <= maxDeploymentPages; page++ {
		endpoint := fmt.Sprintf(
			"%s/repos/%s/deployments?environment=github-pages&per_page=%d&page=%d",
			apiBase,
			repo,
			deploymentsPerPage,
			page,
		)
		body, err := httpfetch.GetWithHeader(ctx, endpoint, header, requestTimeout, maxResponseBytes)
		if err != nil {
			return nil, fmt.Errorf("fetch deployments page %d: %w", page, err)
		}
		var pageDeployments []deployment
		if err := json.Unmarshal(body, &pageDeployments); err != nil {
			return nil, fmt.Errorf("decode deployments page %d: %w", page, err)
		}
		deployments = append(deployments, pageDeployments...)
		if len(pageDeployments) < deploymentsPerPage {
			break
		}
	}
	slices.SortFunc(deployments, compareDeployments)
	return deployments, nil
}

func compareDeployments(a, b deployment) int {
	ta, errA := time.Parse(time.RFC3339Nano, a.CreatedAt)
	tb, errB := time.Parse(time.RFC3339Nano, b.CreatedAt)
	if errA == nil && errB == nil {
		if ta.After(tb) {
			return -1
		}
		if tb.After(ta) {
			return 1
		}
	} else if a.CreatedAt != b.CreatedAt {
		if a.CreatedAt > b.CreatedAt {
			return -1
		}
		return 1
	}
	return cmp.Compare(b.ID, a.ID)
}

func findLiveDeploymentSHA(ctx context.Context, apiBase, repo string, header http.Header, deployments []deployment) (string, bool, error) {
	for i := 0; i < len(deployments) && i < maxDeployments; i++ {
		dep := deployments[i]
		ok, err := deploymentSucceeded(ctx, apiBase, repo, header, dep.ID)
		if err != nil {
			return "", false, err
		}
		if !ok {
			continue
		}
		sha := strings.TrimSpace(dep.SHA)
		if sha == "" {
			return "", false, fmt.Errorf("deployment %d has an empty sha", dep.ID)
		}
		return sha, true, nil
	}
	return "", false, nil
}

func deploymentSucceeded(ctx context.Context, apiBase, repo string, header http.Header, deploymentID int64) (bool, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/deployments/%d/statuses?per_page=%d", apiBase, repo, deploymentID, maxStatuses)
	body, err := httpfetch.GetWithHeader(ctx, endpoint, header, requestTimeout, maxResponseBytes)
	if err != nil {
		return false, fmt.Errorf("fetch statuses for deployment %d: %w", deploymentID, err)
	}
	var statuses []deploymentStatus
	if err := json.Unmarshal(body, &statuses); err != nil {
		return false, fmt.Errorf("decode statuses for deployment %d: %w", deploymentID, err)
	}
	for i := 0; i < len(statuses) && i < maxStatuses; i++ {
		if statuses[i].State == "success" {
			return true, nil
		}
	}
	return false, nil
}

func compareWithLive(ctx context.Context, apiBase, repo string, header http.Header, liveSHA, ourSHA string) (bool, string, error) {
	// per_page=1: only .status is read; unpaged, the response carries up to
	// 250 commits on top of the changed files.
	endpoint := fmt.Sprintf("%s/repos/%s/compare/%s...%s?per_page=1", apiBase, repo, url.PathEscape(liveSHA), url.PathEscape(ourSHA))
	body, err := httpfetch.GetWithHeader(ctx, endpoint, header, requestTimeout, maxCompareBytes)
	if err != nil {
		return false, "", fmt.Errorf("compare %s...%s: %w", liveSHA, ourSHA, err)
	}
	var comp compareResponse
	if err := json.Unmarshal(body, &comp); err != nil {
		return false, "", fmt.Errorf("decode compare %s...%s: %w", liveSHA, ourSHA, err)
	}
	switch comp.Status {
	case "ahead":
		return true, fmt.Sprintf("commit %s is ahead of live deployment %s", ourSHA, liveSHA), nil
	case "identical":
		return true, fmt.Sprintf("commit %s is identical to live deployment %s", ourSHA, liveSHA), nil
	case "behind":
		return false, fmt.Sprintf(
			"commit %s is behind live deployment %s: re-run the newest Pages run on main or dispatch the workflow on main",
			ourSHA,
			liveSHA,
		), nil
	default:
		return false, fmt.Sprintf("commit %s compare status against live deployment %s is %q", ourSHA, liveSHA, comp.Status), nil
	}
}
