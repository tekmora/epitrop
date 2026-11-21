package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/slot"
	"github.com/tekmora/epitrop/internal/supervisor"
)

const slotTestUnit = "epitrop:unit-1"

// slotGit runs git for a test, without the caller's own git variables and configuration.
func slotGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=slot test", "-c", "user.email=slot@example.invalid", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.CommandContext(testContext(t), "git", full...) // #nosec G204 -- fixture callers supply git argv in their temporary repository
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v = %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// slotRun is one prepared `slot run`: a repository, an event log, a template and the
// arguments of the command.
type slotRun struct {
	state     string
	repo      string
	base      string
	template  string
	workspace string
	out       string
	outside   string
	args      []string
}

// newSlotRun prepares a run. job gives the template's job once the run's directories are
// known, so that a job's command can name them.
func newSlotRun(t *testing.T, job func(slotRun) string) slotRun {
	t.Helper()
	base := jobsTempDir(t)
	keyPath := filepath.Join(base, "seed.hex")
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(bytes.Repeat([]byte{6}, 32))), 0o600); err != nil {
		t.Fatalf("WriteFile(key) = %v, want nil", err)
	}
	state := filepath.Join(base, "state")
	run := slotRun{state: state, repo: filepath.Join(base, "repo"), template: filepath.Join(base, "slot.json"), workspace: filepath.Join(base, "ws"), out: filepath.Join(base, "out"), outside: filepath.Join(base, "outside")}
	for _, dir := range []string{filepath.Join(state, ".git"), run.repo, run.outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) = %v, want nil", dir, err)
		}
	}
	slotGit(t, run.repo, "init", "--quiet")
	slotGit(t, run.repo, "commit", "--quiet", "--allow-empty", "--message", "first")
	run.base = slotGit(t, run.repo, "rev-parse", "HEAD")
	brief := filepath.Join(base, "brief.md")
	if err := os.WriteFile(brief, []byte("# Unit\nAdd the retry.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(brief) = %v, want nil", err)
	}
	template := `{"sandbox_input_roots": [` + jsonQuote(base) + `], "event_log": {"dir": ` + jsonQuote(state) + `, "signing_key_path": ` + jsonQuote(keyPath) + `}, "jobs": [` + job(run) + `]}`
	if err := os.WriteFile(run.template, []byte(template+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(template) = %v, want nil", err)
	}
	run.args = []string{"run", "--config", run.template, "--repo", run.repo, "--base", run.base, "--brief", brief, "--workspace", run.workspace, "--out", run.out, slotTestUnit}
	return run
}

// withTestShim makes the supervisor of a command start this test binary as the job's shim.
func withTestShim(t *testing.T) {
	t.Helper()
	old := newJobsSupervisor
	newJobsSupervisor = func(cfg config.Config, opts supervisor.Options) (*supervisor.Supervisor, error) {
		opts.ShimCommand = []string{os.Args[0], "-test.run=TestHelperProcessShim", "--"}
		return supervisor.New(cfg, opts)
	}
	t.Cleanup(func() { newJobsSupervisor = old })
	t.Setenv("EPITROP_TEST_SHIM", "1")
}

// slotFile writes a file of a run's input next to its template and returns its path.
func slotFile(t *testing.T, run slotRun, name string, body string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(run.template), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) = %v, want nil", name, err)
	}
	return path
}

// captureSlot runs `slot` with args and returns what it printed.
func captureSlot(t *testing.T, args []string) (string, error) {
	t.Helper()
	old := slotOut
	var out bytes.Buffer
	slotOut = &out
	defer func() { slotOut = old }()
	err := runSlot(args)
	return out.String(), err
}

func TestRunSlotRefusesWhatIsNoRun(t *testing.T) {
	run := newSlotRun(t, func(slotRun) string {
		return `{"name": "slot-1", "command": ["/usr/bin/true"], "max_runtime_seconds": 60}`
	})
	without := func(flag string) []string {
		return slotArgsWithoutFlag(run.args, flag)
	}
	with := func(flag string, value string) []string {
		return append(without(flag), flag, value)
	}
	malformed := slotFile(t, run, "decisions.tsv", "no tab in this line\n")
	large := slotFile(t, run, "large.md", strings.Repeat("b", slot.MaxInputBytes+1))
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "slot command required"},
		{"an unknown command", []string{"walk"}, "unknown slot command: walk"},
		{"no unit", run.args[:len(run.args)-1], "exactly one unit id"},
		{"two units", append(append([]string{}, run.args...), "epitrop:unit-2"), "exactly one unit id"},
		{"no template", without("--config"), "--config is required"},
		{"no base", without("--base"), "--base is required"},
		{"no brief", without("--brief"), "--brief is required"},
		{"no workspace", without("--workspace"), "--workspace is required"},
		{"no output directory", without("--out"), "--out is required"},
		{"a brief that is not there", with("--brief", filepath.Join(run.outside, "missing.md")), "no such file"},
		{"a resume note that is not there", with("--resume", filepath.Join(run.outside, "missing.md")), "no such file"},
		{"a brief above the bound", with("--brief", large), "at most 65536 bytes"},
		{"decisions that are no table", with("--decisions", malformed), "is not an id, a tab and an answer"},
		{"an unknown flag", append([]string{"run", "--harness=x"}, run.args[1:]...), "harness"},
		{"an invalid unit id", append(append([]string{}, run.args[:len(run.args)-1]...), "unit-1"), "invalid unit id"},
	}
	for i := 0; i < len(cases); i++ {
		checkSlotRefusalCase(t, cases[i].name, cases[i].args, cases[i].want, run)
	}
}

func slotArgsWithoutFlag(args []string, flag string) []string {
	res := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		res = append(res, args[i])
	}
	return res
}

func checkSlotRefusalCase(t *testing.T, name string, args []string, want string, run slotRun) {
	t.Helper()
	printed, err := captureSlot(t, args)
	matched := err != nil && (strings.Contains(err.Error(), want) || (want == "no such file" && errors.Is(err, fs.ErrNotExist)))
	if !matched {
		t.Errorf("%s: slot = %v, want an error holding %q", name, err, want)
	}
	if printed != "" {
		t.Errorf("%s: printed %q, want nothing", name, printed)
	}
	for _, path := range []string{run.workspace, run.out} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Errorf("%s: %s is there (%v), want nothing made", name, path, statErr)
		}
	}
}

// A template whose job is not sandboxed starts nothing and leaves nothing: neither the
// workspace nor the output directory stays, and the unit is not on record.
func TestRunSlotRefusesATemplateWithoutASandbox(t *testing.T) {
	run := newSlotRun(t, func(slotRun) string {
		return `{"name": "slot-1", "command": ["/usr/bin/true"], "max_runtime_seconds": 60, "sandbox": {"mode": "off", "reason": "trusted"}}`
	})
	printed, err := captureSlot(t, run.args)
	if err == nil || !strings.Contains(err.Error(), "runs sandboxed") || printed != "" {
		t.Fatalf("slot run = %q, %v; want the template refused and nothing printed", printed, err)
	}
	for _, path := range []string{run.workspace, run.out} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Errorf("%s is there (%v), want nothing left", path, statErr)
		}
	}
	if refs := slotGit(t, run.repo, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "unit/") || strings.Contains(refs, "refs/epitrop/") {
		t.Fatalf("refs = %q, want no branch of the unit", refs)
	}
	if _, statErr := os.Lstat(filepath.Join(run.state, "events")); !os.IsNotExist(statErr) {
		t.Fatalf("the event log holds records (%v), want none: nothing was recorded", statErr)
	}
}

// The runner takes absolute paths only. The command makes the repository, the workspace and
// the output directory absolute from where it was called, and leaves a path that is one.
func TestSlotRunRequestMakesItsPathsAbsolute(t *testing.T) {
	run := newSlotRun(t, func(slotRun) string {
		return `{"name": "slot-1", "command": ["/usr/bin/true"], "max_runtime_seconds": 60}`
	})
	base := filepath.Dir(run.template)
	t.Chdir(base)
	here, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() = %v, want nil", err)
	}
	empty, repo, workspace := "", "repo", filepath.Join("deeper", "ws")
	brief := filepath.Join(base, "brief.md")
	flags := slotRunFlags{config: &run.template, repo: &repo, base: &run.base, brief: &brief, resume: &empty, decisions: &empty, workspace: &workspace, out: &run.out}
	req, err := slotRunRequest(flags, []string{slotTestUnit})
	if err != nil {
		t.Fatalf("slotRunRequest() = %v, want nil", err)
	}
	if req.Repo != filepath.Join(here, "repo") || req.Dir != filepath.Join(here, "deeper", "ws") || req.Out != run.out {
		t.Fatalf("request = repository %s, workspace %s, output %s; want them absolute below %s, and %s as it was", req.Repo, req.Dir, req.Out, here, run.out)
	}
	if req.Unit != slotTestUnit || req.Base != run.base || string(req.In.Brief) != "# Unit\nAdd the retry.\n" {
		t.Fatalf("request = unit %s on %s with the brief %q, want what the command line names", req.Unit, req.Base, req.In.Brief)
	}
}

// What `slot run` prints is read by whoever started it: one line each for the unit, the
// branch and the report, then the reason and every question of the run.
func TestPrintSlotReportSaysWhatBecameOfTheRun(t *testing.T) {
	printed := func(report slot.Report) string {
		old := slotOut
		var out bytes.Buffer
		slotOut = &out
		defer func() { slotOut = old }()
		printSlotReport(report, "/run/out")
		return out.String()
	}
	file := filepath.Join("/run/out", slot.ReportFile)
	asked := slot.Report{Unit: slotTestUnit, Status: "blocked", Outcome: slot.OutcomeBlocked, Branch: "unit/epitrop=unit-1", Head: "0a0a", Questions: []string{"Which bound?", "Which log?"}}
	want := "unit\t" + slotTestUnit + "\tblocked\tblocked\nbranch\tunit/epitrop=unit-1\t0a0a\tnot published\nreport\t" + file + "\nquestion\tWhich bound?\nquestion\tWhich log?\n"
	if got := printed(asked); got != want {
		t.Errorf("a blocked run printed %q, want %q", got, want)
	}
	done := slot.Report{Unit: slotTestUnit, Status: "review", Outcome: slot.OutcomeDone, Branch: "unit/epitrop=unit-1", Head: "0b0b", Published: true, Reason: "said once"}
	want = "unit\t" + slotTestUnit + "\treview\tdone\nbranch\tunit/epitrop=unit-1\t0b0b\tpublished\nreport\t" + file + "\nreason\tsaid once\n"
	if got := printed(done); got != want {
		t.Errorf("a published run printed %q, want %q", got, want)
	}
}

// The built command knows `slot`: its own refusal comes back, not the one for a command
// that does not exist.
func TestCLISlotReachesItsCommandSubprocess(t *testing.T) {
	bin := buildCLI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "slot").CombinedOutput() // #nosec G204 -- bin is the CLI built by the test
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("epitropctl slot = %v, want exit 1\n%s", err, out)
	}
	if text := string(out); !strings.Contains(text, "Error: slot command required") || strings.Contains(text, "unknown command") {
		t.Fatalf("epitropctl slot said %q, want the slot command's own refusal", text)
	}
}
