// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	skipLine       = "commit-messages: skipped: HEAD adds no commit to origin/main"
	makeRunTimeout = 2 * time.Minute
	gitSetupLimit  = 30 * time.Second
)

type makeResult struct {
	stdout   string
	stderr   string
	exitCode int
}

type makeTargetCase struct {
	name       string
	setup      func(t *testing.T, dir string)
	wantZero   bool
	wantSkip   bool
	wantStdout string
	wantStderr string
}

func TestMakeCommitMessages(t *testing.T) {
	requireTools(t)
	root := repoRoot(t)
	cases := makeTargetCases()
	for i := 0; i < len(cases); i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			got, err := runMakeCommitMessages(t.Context(), root, dir)
			if err != nil {
				t.Fatalf("runMakeCommitMessages = %v", err)
			}
			assertMakeResult(t, tc, got)
		})
	}
}

func TestMakeChildEnvIsolatesGitAndAppendsBuildFlags(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	in := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/hook/.git",
		"GIT_INDEX_FILE=/hook/.git/index",
		"git_work_tree=/hook",
		"GOFLAGS=-mod=readonly",
		"GOWORK=/work/go.work",
		"MAKEFLAGS=-w",
	}
	got := makeChildEnv(in, repo)
	want := []string{
		"PATH=/usr/bin",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test Author",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test Author",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_DIR=" + filepath.Join(repo, ".git"),
		"GIT_WORK_TREE=" + repo,
		"GOFLAGS=-mod=readonly -buildvcs=false",
		"GOWORK=off",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("makeChildEnv = %q, want %q", got, want)
	}
}

func makeTargetCases() []makeTargetCase {
	return []makeTargetCase{
		{name: "level", setup: setupLevelRepo, wantZero: true, wantSkip: true},
		{name: "behind", setup: setupBehindRepo, wantZero: true, wantSkip: true},
		{
			name: "one good commit", setup: setupGoodCommitRepo, wantZero: true, wantSkip: false,
			wantStdout: "1 texts checked, none names a model or a coding tool as an author",
		},
		{
			name: "one bad commit", setup: setupBadCommitRepo, wantZero: false, wantSkip: false,
			wantStdout: "trailer Co-authored-by names a model or a coding tool: Co-authored-by: Claude <noreply@anthropic.com>",
			wantStderr: "1 of 1 texts name a model or a coding tool as an author",
		},
		{name: "no origin/main ref", setup: setupWithoutOriginMainRepo, wantZero: false, wantSkip: false, wantStderr: "git rev-list"},
		{name: "not a git repository", setup: nil, wantZero: false, wantSkip: false, wantStderr: "git rev-list"},
	}
}

func setupLevelRepo(t *testing.T, dir string) {
	t.Helper()
	initGitRepo(t, dir)
	addCommit(t, dir, "chore: base")
	setOriginMain(t, dir, "HEAD")
}

func setupBehindRepo(t *testing.T, dir string) {
	t.Helper()
	initGitRepo(t, dir)
	addCommit(t, dir, "chore: base")
	addCommit(t, dir, "chore: ahead")
	setOriginMain(t, dir, "HEAD")
	if err := runGit(t.Context(), dir, "reset", "--quiet", "--hard", "HEAD~1"); err != nil {
		t.Fatalf("reset HEAD~1: %v", err)
	}
}

func setupGoodCommitRepo(t *testing.T, dir string) {
	t.Helper()
	initGitRepo(t, dir)
	addCommit(t, dir, "chore: base")
	setOriginMain(t, dir, "HEAD")
	addCommit(t, dir, "fix: plain change")
}

func setupBadCommitRepo(t *testing.T, dir string) {
	t.Helper()
	initGitRepo(t, dir)
	addCommit(t, dir, "chore: base")
	setOriginMain(t, dir, "HEAD")
	addCommit(t, dir, "fix: add change\n\nCo-authored-by: Claude <noreply@anthropic.com>")
}

func setupWithoutOriginMainRepo(t *testing.T, dir string) {
	t.Helper()
	initGitRepo(t, dir)
	addCommit(t, dir, "chore: base")
}

func requireTools(t *testing.T) {
	t.Helper()
	tools := [2]string{"make", "git"}
	for i := 0; i < len(tools); i++ {
		if _, err := exec.LookPath(tools[i]); err != nil {
			t.Skipf("%s not found in PATH: %v", tools[i], err)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	makefile := filepath.Join(root, "Makefile")
	info, err := os.Stat(makefile)
	if err != nil {
		t.Fatalf("stat %s: %v", makefile, err)
	}
	if info.IsDir() {
		t.Fatalf("%s is a directory, want Makefile", makefile)
	}
	return root
}

func filterBaseEnv(environ []string, dropMakeAndGo bool) ([]string, string) {
	env := make([]string, 0, len(environ)+10)
	var goflags string
	for i := 0; i < len(environ); i++ {
		name, val, _ := strings.Cut(environ[i], "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "GIT_") {
			continue
		}
		if dropMakeAndGo && isMakeOrGoOverride(upper) {
			if upper == "GOFLAGS" {
				goflags = strings.TrimSpace(val)
			}
			continue
		}
		env = append(env, environ[i])
	}
	return append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test Author",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test Author",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
	), goflags
}

func isMakeOrGoOverride(upper string) bool {
	switch upper {
	case "GOFLAGS", "GOWORK", "MAKEFLAGS", "MFLAGS", "MAKELEVEL":
		return true
	default:
		return false
	}
}

func gitChildEnv(environ []string) []string {
	env, _ := filterBaseEnv(environ, false)
	return env
}

func makeChildEnv(environ []string, repoDir string) []string {
	env, goflags := filterBaseEnv(environ, true)
	if goflags == "" {
		goflags = "-buildvcs=false"
	} else {
		goflags += " -buildvcs=false"
	}
	return append(env,
		"GIT_DIR="+filepath.Join(repoDir, ".git"),
		"GIT_WORK_TREE="+repoDir,
		"GOFLAGS="+goflags,
		"GOWORK=off",
	)
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := runGit(t.Context(), dir, "-c", "init.defaultBranch=main", "init", "--quiet"); err != nil {
		t.Fatalf("init temp repo: %v", err)
	}
}

func addCommit(t *testing.T, dir string, message string) {
	t.Helper()
	if err := runGit(t.Context(), dir, "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", message); err != nil {
		t.Fatalf("commit %q: %v", message, err)
	}
}

func setOriginMain(t *testing.T, dir string, rev string) {
	t.Helper()
	if err := runGit(t.Context(), dir, "update-ref", "refs/remotes/origin/main", rev); err != nil {
		t.Fatalf("set origin/main to %s: %v", rev, err)
	}
}

func runGit(parent context.Context, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(parent, gitSetupLimit)
	defer cancel()
	// #nosec G204 -- fixed git subcommands and test-owned arguments in a temp repository.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitChildEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runMakeCommitMessages(parent context.Context, root string, repoDir string) (makeResult, error) {
	ctx, cancel := context.WithTimeout(parent, makeRunTimeout)
	defer cancel()
	// #nosec G204 -- fixed make invocation against the repository root Makefile.
	cmd := exec.CommandContext(ctx, "make", "-s", "-C", root, "commit-messages")
	cmd.Dir = root
	cmd.Env = makeChildEnv(os.Environ(), repoDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := makeResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return res, fmt.Errorf("make commit-messages timed out: %w", ctx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.exitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, fmt.Errorf("exec make commit-messages: %w", err)
}

func assertMakeResult(t *testing.T, tc makeTargetCase, got makeResult) {
	t.Helper()
	assertExitCode(t, tc.wantZero, got)
	assertSkipLine(t, tc.wantSkip, got)
	if tc.wantStdout != "" && !strings.Contains(got.stdout, tc.wantStdout) {
		t.Fatalf("stdout = %q, want substring %q (stderr = %q)", got.stdout, tc.wantStdout, got.stderr)
	}
	if tc.wantStderr != "" && !strings.Contains(got.stderr, tc.wantStderr) {
		t.Fatalf("stderr = %q, want substring %q (stdout = %q)", got.stderr, tc.wantStderr, got.stdout)
	}
}

func assertExitCode(t *testing.T, wantZero bool, got makeResult) {
	t.Helper()
	if wantZero && got.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%sstderr:\n%s", got.exitCode, got.stdout, got.stderr)
	}
	if !wantZero && got.exitCode == 0 {
		t.Fatalf("exit = 0, want non-zero\nstdout:\n%sstderr:\n%s", got.stdout, got.stderr)
	}
}

func assertSkipLine(t *testing.T, wantSkip bool, got makeResult) {
	t.Helper()
	if wantSkip && strings.TrimSpace(got.stdout) != skipLine {
		t.Fatalf("stdout = %q, want %q\nstderr:\n%s", got.stdout, skipLine+"\n", got.stderr)
	}
	if !wantSkip && (strings.Contains(got.stdout, skipLine) || strings.Contains(got.stderr, skipLine)) {
		t.Fatalf("unexpected skip line in output\nstdout:\n%sstderr:\n%s", got.stdout, got.stderr)
	}
}
