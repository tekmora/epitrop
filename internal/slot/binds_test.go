package slot

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
)

const boundLogin = `{"tokens":{"access_token":"a.b.c","refresh_token":"rt-secret-value"}}`

// boundJob is a stand-in whose sandbox binds a login into the job's home.
type boundJob struct {
	*standIn
	binds []Bind
}

func (j boundJob) Binds() []Bind { return j.binds }

// newBoundRun is a test run whose job has the workspace and a login bound into its home.
func newBoundRun(t *testing.T, w *runWork) (*testRun, string) {
	t.Helper()
	run := newTestRun(t, w)
	login := filepath.Join(t.TempDir(), "auth.json")
	plant(t, login, boundLogin)
	open := run.req.Open
	run.req.Open = func(ctx context.Context, ws Workspace, out string) (Job, UnitRecorder, error) {
		job, recorder, err := open(ctx, ws, out)
		if err != nil {
			return nil, nil, err
		}
		stand, ok := job.(*standIn)
		if !ok {
			t.Fatalf("Open() job = %T, want the stand-in", job)
		}
		home := filepath.Join(ws.Dir, filepath.FromSlash(config.SandboxHomeDir))
		binds := []Bind{
			{Source: ws.Dir, Target: ws.Dir, Mode: BindWrite},
			{Source: login, Target: filepath.Join(home, "auth.json"), Mode: BindRead},
		}
		return boundJob{standIn: stand, binds: binds}, recorder, nil
	}
	return run, login
}

// writeIn writes a file below the workspace, making its directories.
func writeIn(t *testing.T, ws Workspace, name string, body string) {
	t.Helper()
	path := filepath.Join(ws.Dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	plant(t, path, body)
}

// A run that commits its login, by force past the exclude, or under another name, or in a
// commit it reverts again, publishes nothing and is recorded as not taken.
func TestRunTakesNoBranchThatHandsBackTheLogin(t *testing.T) {
	cases := []struct {
		name string
		work runWork
		want string
	}{
		{"the home's login added by force", runWork{outcome: OutcomeDone, commit: true, stage: func(ws Workspace) {
			writeIn(t, ws, config.SandboxHomeDir+"/auth.json", boundLogin)
			gitIn(t, ws.Dir, "add", "-f", config.SandboxHomeDir+"/auth.json")
		}}, "a commit touches .slot/home/auth.json, the job's home"},
		{"another file of the home", runWork{outcome: OutcomeDone, commit: true, stage: func(ws Workspace) {
			writeIn(t, ws, config.SandboxHomeDir+"/sessions/log", "turn 1\n")
			gitIn(t, ws.Dir, "add", "-f", config.SandboxHomeDir+"/sessions/log")
		}}, "a commit touches .slot/home/sessions/log"},
		{"a file of the slot's inputs", runWork{outcome: OutcomeDone, commit: true, stage: func(ws Workspace) {
			writeIn(t, ws, InDir+"/brief.md", "the brief\n")
			gitIn(t, ws.Dir, "add", "-f", InDir+"/brief.md")
		}}, "a commit touches .slot/in/brief.md"},
		{"a file of the repository moved into the home", runWork{outcome: OutcomeDone, commit: true, stage: func(ws Workspace) {
			if err := os.MkdirAll(filepath.Join(ws.Dir, filepath.FromSlash(config.SandboxHomeDir)), 0o750); err != nil {
				t.Fatalf("MkdirAll() = %v, want nil", err)
			}
			gitIn(t, ws.Dir, "mv", "-f", "second.txt", config.SandboxHomeDir+"/second.txt")
		}}, "a commit touches .slot/home/second.txt"},
		{"the login under another name", runWork{outcome: OutcomeDone, commit: true, stage: func(ws Workspace) {
			writeIn(t, ws, "docs/notes.json", boundLogin)
			gitIn(t, ws.Dir, "add", "docs/notes.json")
		}}, "a commit adds docs/notes.json with the content of the bound file"},
		{"the login committed and removed again", runWork{outcome: OutcomeDone, commit: true, stage: func(ws Workspace) {
			writeIn(t, ws, "leak.json", boundLogin)
			gitIn(t, ws.Dir, "add", "leak.json")
			gitIn(t, ws.Dir, "commit", "--quiet", "--message", "add")
			gitIn(t, ws.Dir, "rm", "--quiet", "leak.json")
		}}, "a commit adds leak.json with the content of the bound file"},
		{"the login brought in by a merge commit alone", runWork{outcome: OutcomeFailed, after: func(ws Workspace) {
			gitIn(t, ws.Dir, "switch", "--quiet", "--create", "side")
			writeIn(t, ws, "side.txt", "side\n")
			gitIn(t, ws.Dir, "add", "side.txt")
			gitIn(t, ws.Dir, "commit", "--quiet", "--message", "side")
			gitIn(t, ws.Dir, "switch", "--quiet", ws.Branch)
			gitIn(t, ws.Dir, "merge", "--quiet", "--no-ff", "--no-commit", "side")
			writeIn(t, ws, "merged.json", boundLogin)
			gitIn(t, ws.Dir, "add", "merged.json")
			gitIn(t, ws.Dir, "commit", "--quiet", "--message", "merge")
		}}, "a commit adds merged.json with the content of the bound file"},
	}
	for i := 0; i < len(cases); i++ {
		run, login := newBoundRun(t, &cases[i].work)
		checkRunNotTaken(t, cases[i].name, run, cases[i].want)
		if report, body := readReport(t, run.req.Out); len(report.Binds) != 2 || strings.Contains(string(body), "rt-secret-value") {
			t.Errorf("%s: written report binds = %+v, want the binds listed and no login content", cases[i].name, report.Binds)
		} else if report.Binds[1].Source != login || report.Binds[1].Mode != BindRead {
			t.Errorf("%s: report binds = %+v, want the login bound read-only", cases[i].name, report.Binds)
		}
	}
}

// A run whose commits stay clear of the home and of the login is taken, and its report lists
// what the job's sandbox bound.
func TestRunTakesACleanBranchAndListsItsBinds(t *testing.T) {
	run, login := newBoundRun(t, &runWork{outcome: OutcomeDone, commit: true})
	report, err := Run(slotCtx(t), run.req)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if !report.Taken() || !report.Published {
		t.Fatalf("report = %+v, want the branch taken and published", report)
	}
	written, _ := readReport(t, run.req.Out)
	want := []Bind{{Source: report.Workspace, Target: report.Workspace, Mode: BindWrite}, {Source: login, Target: filepath.Join(report.Workspace, ".slot", "home", "auth.json"), Mode: BindRead}}
	if !slices.Equal(written.Binds, want) {
		t.Fatalf("written binds = %+v, want %+v", written.Binds, want)
	}
}

func TestReadOnlyFiles(t *testing.T) {
	dir := t.TempDir()
	login, empty, link := filepath.Join(dir, "auth.json"), filepath.Join(dir, "empty"), filepath.Join(dir, "link")
	settings := filepath.Join(dir, "config.toml")
	plant(t, login, boundLogin)
	plant(t, settings, "model = \"test\"\n")
	plant(t, empty, "")
	if err := os.Symlink(login, link); err != nil {
		t.Fatalf("Symlink() = %v, want nil", err)
	}
	binds := []Bind{
		{Source: dir, Mode: BindWrite},
		{Source: login, Mode: BindRead},
		{Source: login, Mode: BindRead},
		{Source: settings, Mode: BindCopy},
		{Source: empty, Mode: BindRead},
		{Source: link, Mode: BindRead},
		{Source: dir, Mode: BindRead},
		{Source: filepath.Join(dir, "missing"), Mode: BindRead},
	}
	if got := readOnlyFiles(binds); !slices.Equal(got, []string{login}) {
		t.Fatalf("readOnlyFiles() = %q, want the regular, non-empty read-only file once", got)
	}
}

func TestCheckRawChanges(t *testing.T) {
	blob := strings.Repeat("ab", 20)
	other := strings.Repeat("cd", 20)
	zero := strings.Repeat("00", 20)
	blobs := map[string]string{blob: "/src/auth.json"}
	record := func(old string, now string, status string) string {
		return ":100644 100644 " + old + " " + now + " " + status
	}
	cases := []struct {
		name   string
		fields []string
		want   string
	}{
		{"no change", []string{""}, ""},
		{"a plain change", []string{record(zero, other, "A"), "src/main.go", ""}, ""},
		{"a file named like the home", []string{record(zero, other, "A"), ".slotted/x", ""}, ""},
		{"a removal of the bound content", []string{record(blob, zero, "D"), "old.json", ""}, ""},
		{"the home", []string{record(zero, other, "A"), ".slot/home/auth.json", ""}, "touches .slot/home/auth.json"},
		{"the bound content", []string{record(zero, blob, "M"), "x.json", ""}, "adds x.json with the content of the bound file /src/auth.json"},
		{"a record without its path", []string{record(zero, other, "A")}, "unexpected change record"},
		{"a record of another shape", []string{":100644 " + other, "x", ""}, "unexpected change record"},
		{"a path where a record belongs", []string{"src/main.go", ""}, "unexpected change record"},
	}
	for _, tc := range cases {
		err := checkRawChanges(tc.fields, blobs)
		if tc.want == "" && err != nil {
			t.Errorf("%s: checkRawChanges() = %v, want nil", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: checkRawChanges() = %v, want an error holding %q", tc.name, err, tc.want)
		}
	}
	over := make([]string, 2*MaxBranchChanges+1)
	if err := checkRawChanges(over, blobs); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("checkRawChanges(%d fields) = %v, want the bound refused", len(over), err)
	}
	if err := checkRawChanges(make([]string, 2*MaxBranchChanges), blobs); err != nil {
		t.Errorf("checkRawChanges(%d empty fields) = %v, want nil", 2*MaxBranchChanges, err)
	}
}

func TestSupervisedBinds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the slot sandbox does not run on Windows")
	}
	sandbox := config.SandboxConfig{
		Mode: "enforce", Workspace: "/ws", Inputs: []string{"/in"},
		Home: &config.SandboxHome{
			Source: "/home-src", Env: "CODEX_HOME",
			Copy: []config.SandboxHomeCopy{{File: "config.toml", SHA256: strings.Repeat("a", 64)}},
			Bind: []config.SandboxHomeBind{{File: "auth.json"}, {File: ".client/login.json"}},
		},
	}
	job := &Supervised{sandbox: sandbox}
	want := []Bind{
		{Source: "/ws", Target: "/ws", Mode: BindWrite},
		{Source: "/in", Target: "/in", Mode: BindRead},
		{Source: "/home-src/config.toml", Target: "/ws/.slot/home/config.toml", Mode: BindCopy, SHA256: strings.Repeat("a", 64)},
		{Source: "/home-src/auth.json", Target: "/ws/.slot/home/auth.json", Mode: BindRead},
		{Source: "/home-src/.client/login.json", Target: "/ws/.slot/home/.client/login.json", Mode: BindRead},
	}
	if got := job.Binds(); !slices.Equal(got, want) {
		t.Fatalf("Binds() = %+v, want %+v", got, want)
	}
	sandbox.Home = nil
	if got := (&Supervised{sandbox: sandbox}).Binds(); len(got) != 2 {
		t.Errorf("Binds(no home) = %+v, want the workspace and the input", got)
	}
	if got := (&Supervised{sandbox: config.SandboxConfig{Mode: "off", Workspace: "/ws"}}).Binds(); got != nil {
		t.Errorf("Binds(mode off) = %+v, want none", got)
	}
}
