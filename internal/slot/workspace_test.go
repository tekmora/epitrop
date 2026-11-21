package slot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/gitexec"
)

func slotCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// gitIn runs git in dir as a test author would, without the machine's own settings.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=slot test", "-c", "user.email=slot@example.invalid", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.CommandContext(slotCtx(t), "git", full...) // #nosec G204 -- fixture callers supply git argv in their temporary repository
	cmd.Dir = dir
	cmd.Env = gitexec.Env(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v = %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testRepo makes a repository with two commits and returns it with the two commit ids.
func testRepo(t *testing.T) (string, string, string) {
	t.Helper()
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet")
	commit := func(name string, body string) string {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) = %v, want nil", name, err)
		}
		gitIn(t, repo, "add", name)
		gitIn(t, repo, "commit", "--quiet", "--message", "add "+name)
		return gitIn(t, repo, "rev-parse", "HEAD")
	}
	first := commit("first.txt", "first\n")
	second := commit("second.txt", "second\n")
	return repo, first, second
}

func request(t *testing.T, repo string, base string) Request {
	t.Helper()
	return Request{
		Repo: repo, Base: base, Unit: testUnit, Dir: filepath.Join(t.TempDir(), "ws"),
		In: Inputs{
			Brief:     []byte("# Unit\nAdd the retry.\n"),
			Resume:    []byte("main is green.\n"),
			Decisions: []Decision{{ID: "epitrop-port", Answer: "a) 8080"}, {ID: "epitrop-retries", Answer: "3"}},
		},
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) // #nosec G304 -- callers read files in temporary slot workspaces
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", path, err)
	}
	return string(body)
}

func TestPrepareMakesTheUnitsBranchAtTheBaseWithTheInputs(t *testing.T) {
	repo, first, _ := testRepo(t)
	req := request(t, repo, first)
	ws, err := Prepare(slotCtx(t), req)
	if err != nil {
		t.Fatalf("Prepare() = %v, want nil", err)
	}
	if ws.Dir != req.Dir || ws.Branch != "unit/epitrop=unit-1" || ws.Base != first {
		t.Fatalf("workspace = %+v, want %s on unit/epitrop=unit-1 at %s", ws, req.Dir, first)
	}
	assertWorkspaceBranchAndFiles(t, ws, first)
	assertWorkspaceInputsAndEvidence(t, ws.Dir)
}

func assertWorkspaceBranchAndFiles(t *testing.T, ws Workspace, first string) {
	t.Helper()
	if head, branch := gitIn(t, ws.Dir, "rev-parse", "HEAD"), gitIn(t, ws.Dir, "branch", "--show-current"); head != first || branch != ws.Branch {
		t.Fatalf("clone is at %s on %q, want %s on %s", head, branch, first, ws.Branch)
	}
	if got := readFile(t, filepath.Join(ws.Dir, "first.txt")); got != "first\n" {
		t.Fatalf("first.txt = %q, want the base's file", got)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "second.txt")); !os.IsNotExist(err) {
		t.Fatalf("second.txt: %v, want it absent: it came after the base", err)
	}
}

func assertWorkspaceInputsAndEvidence(t *testing.T, dir string) {
	t.Helper()
	in := func(name string) string { return readFile(t, filepath.Join(dir, filepath.FromSlash(name))) }
	if in(BriefFile) != "# Unit\nAdd the retry.\n" || in(ResumeFile) != "main is green.\n" || in(DecisionsFile) != "epitrop-port\ta) 8080\nepitrop-retries\t3\n" {
		t.Fatalf("inputs = %q / %q / %q, want what the request held", in(BriefFile), in(ResumeFile), in(DecisionsFile))
	}
	entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(EvidenceDir)))
	if err != nil || len(entries) != 0 {
		t.Fatalf("evidence directory = %d entries, %v; want it there and empty", len(entries), err)
	}
}

// A run can write its whole workspace. The clone therefore shares no file with the
// repository it came from, has no remote, and keeps the contract's files out of its commits.
func TestPrepareMakesACloneThatSharesNothingWithItsRepository(t *testing.T) {
	repo, _, second := testRepo(t)
	ws, err := Prepare(slotCtx(t), request(t, repo, second))
	if err != nil {
		t.Fatalf("Prepare() = %v, want nil", err)
	}
	if remotes := gitIn(t, ws.Dir, "remote"); remotes != "" {
		t.Fatalf("remotes = %q, want none", remotes)
	}
	assertCloneObjectsNotShared(t, ws.Dir, repo)
	if status := gitIn(t, ws.Dir, "status", "--porcelain"); status != "" {
		t.Fatalf("status of the fresh workspace = %q, want it clean: the contract's files are excluded", status)
	}
	assertCloneCommitUntouchedRepo(t, ws.Dir, repo, second)
}

func checkCloneObjectFile(t *testing.T, path string, d os.DirEntry, walkErr error, wsDir, repo string, objects *int) error {
	t.Helper()
	if walkErr != nil || d.IsDir() {
		return walkErr
	}
	rel, relErr := filepath.Rel(wsDir, path)
	if relErr != nil {
		return relErr
	}
	mine, statErr := os.Stat(path)
	if statErr != nil {
		return statErr
	}
	*objects++
	if theirs, sourceErr := os.Stat(filepath.Join(repo, rel)); sourceErr == nil && os.SameFile(mine, theirs) {
		t.Errorf("%s is the same file as the repository's: a hard link, want a copy", rel)
	}
	return nil
}

func assertCloneObjectsNotShared(t *testing.T, wsDir, repo string) {
	t.Helper()
	objects := 0
	err := filepath.WalkDir(filepath.Join(wsDir, ".git", "objects"), func(path string, d os.DirEntry, walkErr error) error {
		return checkCloneObjectFile(t, path, d, walkErr, wsDir, repo, &objects)
	})
	if err != nil || objects == 0 {
		t.Fatalf("walk of the clone's objects = %d files, %v; want its own copies", objects, err)
	}
}

func assertCloneCommitUntouchedRepo(t *testing.T, wsDir, repo, second string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wsDir, "third.txt"), []byte("third\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	gitIn(t, wsDir, "add", "--all")
	gitIn(t, wsDir, "commit", "--quiet", "--message", "the unit's work")
	if files := gitIn(t, wsDir, "show", "--name-only", "--format=", "HEAD"); files != "third.txt" {
		t.Fatalf("the run's commit holds %q, want third.txt alone", files)
	}
	if head := gitIn(t, repo, "rev-parse", "HEAD"); head != second {
		t.Fatalf("the repository moved to %s, want it untouched at %s", head, second)
	}
}

// A repository that is on the unit's own branch gives its clone a branch of that name, at
// the repository's commit. The workspace still starts at the base that was asked for.
func TestPrepareStartsAtTheBaseWhenTheRepositoryIsOnTheUnitsBranch(t *testing.T) {
	repo, first, second := testRepo(t)
	gitIn(t, repo, "switch", "--quiet", "--create", Branch(testUnit), first)
	ws, err := Prepare(slotCtx(t), request(t, repo, second))
	if err != nil {
		t.Fatalf("Prepare(repository on the unit's branch) = %v, want nil", err)
	}
	if head := gitIn(t, ws.Dir, "rev-parse", "HEAD"); head != second {
		t.Fatalf("the workspace is at %s, want the base %s", head, second)
	}
	if branch := gitIn(t, ws.Dir, "branch", "--show-current"); branch != ws.Branch {
		t.Fatalf("the workspace is on %q, want %q", branch, ws.Branch)
	}
	if head := gitIn(t, repo, "rev-parse", "refs/heads/"+Branch(testUnit)); head != first {
		t.Fatalf("the repository's branch moved to %s, want it at %s", head, first)
	}
}

// Git exports GIT_DIR, GIT_WORK_TREE and GIT_INDEX_FILE to a hook. Prepare started from one
// still clones the repository it was given and touches no other.
func TestPrepareIgnoresTheCallersGitEnvironment(t *testing.T) {
	repo, _, second := testRepo(t)
	other := t.TempDir()
	gitIn(t, other, "init", "--quiet")
	gitIn(t, other, "commit", "--quiet", "--allow-empty", "--message", "the hook's repository")
	otherHead, otherRefs := gitIn(t, other, "rev-parse", "HEAD"), gitIn(t, other, "for-each-ref")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))

	ws, err := Prepare(slotCtx(t), request(t, repo, second))
	if err != nil {
		t.Fatalf("Prepare(with a hook's environment) = %v, want nil", err)
	}
	if ws.Base != second {
		t.Errorf("base = %s, want %s of the repository that was named", ws.Base, second)
	}
	if head := gitIn(t, ws.Dir, "rev-parse", "HEAD"); head != second {
		t.Errorf("the workspace is at %s, want %s", head, second)
	}
	if branch := gitIn(t, ws.Dir, "branch", "--show-current"); branch != ws.Branch {
		t.Errorf("the workspace is on %q, want %q", branch, ws.Branch)
	}
	if head, refs := gitIn(t, other, "rev-parse", "HEAD"), gitIn(t, other, "for-each-ref"); head != otherHead || refs != otherRefs {
		t.Errorf("the hook's repository changed: head %s, refs %q; want %s and %q", head, refs, otherHead, otherRefs)
	}
}

func TestPrepareRefusesAndLeavesNothing(t *testing.T) {
	repo, first, _ := testRepo(t)
	t.Chdir(t.TempDir()) // a relative path that is wrongly taken lands here, not in the source tree
	cases := []struct {
		name string
		edit func(*Request)
		want string
	}{
		{"an invalid unit id", func(r *Request) { r.Unit = "unit-1" }, `invalid unit id "unit-1"`},
		{"a unit id that gives no branch name", func(r *Request) { r.Unit = "a..b#1" }, "gives no valid branch name"},
		{"a relative repository", func(r *Request) { r.Repo = "repo" }, "absolute paths"},
		{"a relative workspace", func(r *Request) { r.Dir = "ws" }, "absolute paths"},
		{"a base that is no commit", func(r *Request) { r.Base = "no-such-branch" }, `base "no-such-branch" is no commit`},
		{"a base that is an option", func(r *Request) { r.Base = "--upload-pack=touch" }, "is no commit name"},
		{"no base", func(r *Request) { r.Base = "" }, "is no commit name"},
		{"an empty brief", func(r *Request) { r.In.Brief = nil }, "the brief is empty"},
		{"a brief above the bound", func(r *Request) { r.In.Brief = []byte(strings.Repeat("b", MaxInputBytes+1)) }, "at most 65536 bytes"},
		{"a resume note above the bound", func(r *Request) { r.In.Resume = []byte(strings.Repeat("r", MaxInputBytes+1)) }, "at most 65536 bytes"},
		{"a decision with a tab in its answer", func(r *Request) { r.In.Decisions = []Decision{{ID: "x", Answer: "a\tb"}} }, "tab or line break"},
		{"a decision without an id", func(r *Request) { r.In.Decisions = []Decision{{Answer: "a"}} }, "has no id"},
		{"one decision too many", func(r *Request) { r.In.Decisions = make([]Decision, MaxDecisions+1) }, "at most 256 decisions"},
	}
	for i := 0; i < len(cases); i++ {
		req := request(t, repo, first)
		cases[i].edit(&req)
		_, err := Prepare(slotCtx(t), req)
		if err == nil || !strings.Contains(err.Error(), cases[i].want) {
			t.Errorf("%s: Prepare() = %v, want an error holding %q", cases[i].name, err, cases[i].want)
		}
		if _, statErr := os.Lstat(req.Dir); !os.IsNotExist(statErr) {
			t.Errorf("%s: %s is there after the refusal (%v), want nothing left", cases[i].name, req.Dir, statErr)
		}
	}
	req := request(t, repo, first)
	if _, err := Prepare(context.Background(), req); err == nil || !strings.Contains(err.Error(), "requires a context deadline") {
		t.Errorf("Prepare(no deadline) = %v, want it refused", err)
	}
	if err := os.MkdirAll(req.Dir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	if _, err := Prepare(slotCtx(t), req); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("Prepare(existing workspace) = %v, want it refused", err)
	}
}

func TestPrepareTakesInputsAtTheirBounds(t *testing.T) {
	repo, first, _ := testRepo(t)
	req := request(t, repo, first)
	req.In.Brief = []byte(strings.Repeat("b", MaxInputBytes))
	req.In.Resume = nil
	req.In.Decisions = make([]Decision, MaxDecisions)
	for i := 0; i < MaxDecisions; i++ {
		req.In.Decisions[i] = Decision{ID: "d", Answer: "a"}
	}
	ws, err := Prepare(slotCtx(t), req)
	if err != nil {
		t.Fatalf("Prepare(inputs at their bounds) = %v, want nil", err)
	}
	if got := strings.Count(readFile(t, filepath.Join(ws.Dir, filepath.FromSlash(DecisionsFile))), "\n"); got != MaxDecisions {
		t.Fatalf("decisions file has %d lines, want %d", got, MaxDecisions)
	}
}

// A workspace that could not be finished is removed: a later run must not take a half-made
// clone for a workspace.
func TestPrepareRemovesAWorkspaceItCouldNotFinish(t *testing.T) {
	repo, first, _ := testRepo(t)
	real := git
	git = func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "switch" {
			return "", errors.New("planted: the branch cannot be made")
		}
		return real(ctx, dir, args...)
	}
	t.Cleanup(func() { git = real })
	req := request(t, repo, first)
	_, err := Prepare(slotCtx(t), req)
	if err == nil || !strings.Contains(err.Error(), "planted: the branch cannot be made") {
		t.Fatalf("Prepare() = %v, want the planted failure", err)
	}
	if _, statErr := os.Lstat(req.Dir); !os.IsNotExist(statErr) {
		t.Fatalf("%s is there after the failure (%v), want it removed", req.Dir, statErr)
	}
}

func TestBranchGivesEachUnitItsOwnName(t *testing.T) {
	cases := map[string]string{
		"tekmora/epitrop#7":  "unit/tekmora/epitrop#7",
		"epitrop:slot-5":     "unit/epitrop=slot-5",
		"epitrop:7":          "unit/epitrop=7",
		"epitrop#7":          "unit/epitrop#7",
		"a-b:c":              "unit/a-b=c",
		"a:b-c":              "unit/a=b-c",
		"tekmora/epitrop:x1": "unit/tekmora/epitrop=x1",
	}
	seen := map[string]string{}
	for unit, want := range cases {
		got := Branch(unit)
		if got != want {
			t.Errorf("Branch(%q) = %q, want %q", unit, got, want)
		}
		if other, taken := seen[got]; taken {
			t.Errorf("Branch(%q) and Branch(%q) are both %q, want a name of its own for each unit", unit, other, got)
		}
		seen[got] = unit
	}
}

// The decisions file a caller hands over reads back as what the workspace gets.
func TestParseDecisionsReadsTheFormTheWorkspaceGets(t *testing.T) {
	got, err := ParseDecisions([]byte("epitrop-port\ta) 8080\n\nepitrop-retries\t3\r\nepitrop-empty\t\n"))
	want := []Decision{{ID: "epitrop-port", Answer: "a) 8080"}, {ID: "epitrop-retries", Answer: "3"}, {ID: "epitrop-empty", Answer: ""}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ParseDecisions() = %+v, %v; want %+v", got, err, want)
	}
	if got, err = ParseDecisions(nil); err != nil || len(got) != 0 {
		t.Fatalf("ParseDecisions(empty) = %+v, %v; want none", got, err)
	}
	checkParseDecisionsRefusalsAndLimits(t)
	assertParseDecisionsRoundTrip(t, want)
}

func checkParseDecisionsRefusalsAndLimits(t *testing.T) {
	t.Helper()
	bodies := []string{"no tab here\n", "\tanswer without an id\n", "ok\tyes\nbroken\n"}
	for i := 0; i < len(bodies); i++ {
		body := bodies[i]
		if _, err := ParseDecisions([]byte(body)); err == nil || !strings.Contains(err.Error(), "is not an id, a tab and an answer") {
			t.Errorf("ParseDecisions(%q) = %v, want it refused", body, err)
		}
	}
	if _, err := ParseDecisions([]byte(strings.Repeat("id\tanswer\n", MaxDecisions))); err != nil {
		t.Fatalf("ParseDecisions(%d lines) = %v, want nil", MaxDecisions, err)
	}
	if _, err := ParseDecisions([]byte(strings.Repeat("id\tanswer\n", MaxDecisions+1))); err == nil || !strings.Contains(err.Error(), "at most 256 decisions") {
		t.Fatalf("ParseDecisions(%d lines) = %v, want it refused", MaxDecisions+1, err)
	}
}

func assertParseDecisionsRoundTrip(t *testing.T, want []Decision) {
	t.Helper()
	// What is parsed is what Prepare writes: the file reads back as itself.
	repo, first, _ := testRepo(t)
	req := request(t, repo, first)
	req.In.Decisions = want
	ws, err := Prepare(slotCtx(t), req)
	if err != nil {
		t.Fatalf("Prepare() = %v, want nil", err)
	}
	written, err := os.ReadFile(filepath.Join(ws.Dir, filepath.FromSlash(DecisionsFile)))
	if err != nil {
		t.Fatalf("ReadFile() = %v, want nil", err)
	}
	if again, parseErr := ParseDecisions(written); parseErr != nil || !slices.Equal(again, want) {
		t.Fatalf("ParseDecisions(written file) = %+v, %v; want %+v", again, parseErr, want)
	}
}
