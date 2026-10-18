package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testHomeSum = "2b2b6ccccf382af02ad9641bc858d389999f79ced50cbd5213b79298f79091b0"

func testHome() *SandboxHome {
	source := "/srv/slot-home"
	if !filepath.IsAbs(source) {
		source = filepath.Join(os.TempDir(), "slot-home")
	}
	return &SandboxHome{
		Source: source, Env: "CODEX_HOME",
		Copy: []SandboxHomeCopy{{File: "config.toml", SHA256: testHomeSum}},
		Bind: []SandboxHomeBind{{File: "auth.json", JWT: "tokens.access_token"}},
	}
}

func homeFiles(prefix string, count int) []SandboxHomeCopy {
	files := make([]SandboxHomeCopy, 0, count)
	for i := 0; i < count; i++ {
		files = append(files, SandboxHomeCopy{File: fmt.Sprintf("%s%d", prefix, i), SHA256: testHomeSum})
	}
	return files
}

func TestValidateSandboxHomeRules(t *testing.T) {
	cases := []struct {
		name string
		edit func(*SandboxConfig)
		want string
	}{
		{"no home", func(s *SandboxConfig) { s.Home = nil }, ""},
		{"copy and bind", func(*SandboxConfig) {}, ""},
		{"bind only", func(s *SandboxConfig) { s.Home.Copy = nil }, ""},
		{"copy only", func(s *SandboxConfig) { s.Home.Bind = nil }, ""},
		{"no file", func(s *SandboxConfig) { s.Home.Copy, s.Home.Bind = nil, nil }, "names no file"},
		{"mode off", func(s *SandboxConfig) { s.Mode, s.Reason = "off", "legacy" }, "home: forbidden unless mode=enforce"},
		{"relative source", func(s *SandboxConfig) { s.Home.Source = "slot-home" }, "home.source: \"slot-home\" is not absolute"},
		{"empty source", func(s *SandboxConfig) { s.Home.Source = "" }, "home.source"},
		{"env lower case", func(s *SandboxConfig) { s.Home.Env = "codex_home" }, "home.env: invalid name"},
		{"env empty", func(s *SandboxConfig) { s.Home.Env = "" }, "home.env: invalid name"},
		{"env a proxy variable", func(s *SandboxConfig) { s.Home.Env = "HTTPS_PROXY" }, "is set by the sandbox"},
		{"env also allowed", func(s *SandboxConfig) { s.EnvAllow = []string{"CODEX_HOME"} }, "also in env_allow"},
		{"copies at limit", func(s *SandboxConfig) { s.Home.Copy = homeFiles("c", MaxSandboxHomeFiles) }, ""},
		{"copies over limit", func(s *SandboxConfig) { s.Home.Copy = homeFiles("c", MaxSandboxHomeFiles+1) }, "at most 8 files"},
		{"binds over limit", func(s *SandboxConfig) {
			s.Home.Bind = make([]SandboxHomeBind, MaxSandboxHomeFiles+1)
		}, "at most 8 files"},
		{"sum upper case", func(s *SandboxConfig) { s.Home.Copy[0].SHA256 = strings.ToUpper(testHomeSum) }, "home.copy/0.sha256"},
		{"sum short", func(s *SandboxConfig) { s.Home.Copy[0].SHA256 = testHomeSum[1:] }, "home.copy/0.sha256"},
		{"token path of eight names", func(s *SandboxConfig) { s.Home.Bind[0].JWT = "a.b.c.d.e.f.g.h" }, ""},
		{"token path of nine names", func(s *SandboxConfig) { s.Home.Bind[0].JWT = "a.b.c.d.e.f.g.h.i" }, "home.bind/0.jwt"},
		{"token path with an empty name", func(s *SandboxConfig) { s.Home.Bind[0].JWT = "tokens..access" }, "home.bind/0.jwt"},
		{"file ..", func(s *SandboxConfig) { s.Home.Bind[0].File = ".." }, "not one plain path element"},
		{"file .", func(s *SandboxConfig) { s.Home.Copy[0].File = "." }, "not one plain path element"},
		{"file with a slash", func(s *SandboxConfig) { s.Home.Bind[0].File = "sub/auth.json" }, "nor a known login file"},
		{"file empty", func(s *SandboxConfig) { s.Home.Bind[0].File = "" }, "not one plain path element"},
		{"file name of 128", func(s *SandboxConfig) { s.Home.Bind[0].File = strings.Repeat("a", 128) }, ""},
		{"file name of 129", func(s *SandboxConfig) { s.Home.Bind[0].File = strings.Repeat("a", 129) }, "not one plain path element"},
		{"file named in copy and bind", func(s *SandboxConfig) { s.Home.Bind[0].File = "config.toml" }, "named twice"},
	}
	for _, tc := range cases {
		checkSandboxHomeRule(t, tc.name, tc.edit, tc.want)
	}
}

// A login file keeps its client's name, a bind names its expiry once, and a home may take
// HOME and no other variable the sandbox sets.
func TestValidateSandboxHomeLoginRules(t *testing.T) {
	cases := []struct {
		name string
		edit func(*SandboxConfig)
		want string
	}{
		{"bind with no expiry", func(s *SandboxConfig) { s.Home.Bind[0].JWT = "" }, "home.bind/0: names neither jwt nor expires_ms"},
		{"bind with both expiries", func(s *SandboxConfig) { s.Home.Bind[0].ExpiresMS = "expiresAt" }, "home.bind/0: names both jwt and expires_ms"},
		{"bind with an expiry in milliseconds", func(s *SandboxConfig) {
			s.Home.Bind[0] = SandboxHomeBind{File: ".credentials.json", ExpiresMS: "claudeAiOauth.expiresAt"}
		}, ""},
		{"expiry path of eight names", func(s *SandboxConfig) { s.Home.Bind[0].JWT, s.Home.Bind[0].ExpiresMS = "", "a.b.c.d.e.f.g.h" }, ""},
		{"expiry path of nine names", func(s *SandboxConfig) {
			s.Home.Bind[0].JWT, s.Home.Bind[0].ExpiresMS = "", "a.b.c.d.e.f.g.h.i"
		}, "home.bind/0.expires_ms"},
		{"expiry path with a slash", func(s *SandboxConfig) { s.Home.Bind[0].JWT, s.Home.Bind[0].ExpiresMS = "", "a/b" }, "home.bind/0.expires_ms"},
		{"env HOME", func(s *SandboxConfig) { s.Home.Env = "HOME" }, ""},
		{"env PATH", func(s *SandboxConfig) { s.Home.Env = "PATH" }, "home.env: PATH is set by the sandbox"},
		{"file a dotfile", func(s *SandboxConfig) { s.Home.Bind[0].File = ".auth.json" }, "nor a known login file"},
		{"login file", func(s *SandboxConfig) { s.Home.Bind[0].File = ".credentials.json" }, ""},
		{"login file as a copy", func(s *SandboxConfig) { s.Home.Copy[0].File = ".credentials.json" }, "is not one plain path element"},
		{"login file below a folder", func(s *SandboxConfig) { s.Home.Bind[0].File = "sub/.credentials.json" }, "nor a known login file"},
	}
	for _, tc := range cases {
		checkSandboxHomeRule(t, tc.name, tc.edit, tc.want)
	}
}

func checkSandboxHomeRule(t *testing.T, name string, edit func(*SandboxConfig), want string) {
	t.Helper()
	sandbox := SandboxConfig{Mode: "enforce", Network: "none", MemoryMax: "2G", CPUWeight: 100, TasksMax: 512, Home: testHome()}
	edit(&sandbox)
	err := ValidateSandbox(sandbox)
	if want == "" && err != nil {
		t.Errorf("%s: ValidateSandbox() = %v, want nil", name, err)
	}
	if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
		t.Errorf("%s: ValidateSandbox() = %v, want an error holding %q", name, err, want)
	}
}

// A login file the list names below a folder is taken as written, and no other path.
func TestValidateSandboxHomeTakesListedLoginPaths(t *testing.T) {
	saved := homeCredentialFiles
	t.Cleanup(func() { homeCredentialFiles = saved })
	homeCredentialFiles = []string{".client/login/token.json"}
	sandbox := SandboxConfig{Mode: "enforce", Network: "none", MemoryMax: "2G", CPUWeight: 100, TasksMax: 512, Home: testHome()}
	sandbox.Home.Bind[0].File = ".client/login/token.json"
	if err := ValidateSandbox(sandbox); err != nil {
		t.Fatalf("ValidateSandbox(listed login path) = %v, want nil", err)
	}
	for _, file := range []string{".client/login", ".client/login/token.json/", "./.client/login/token.json", ".credentials.json"} {
		sandbox.Home.Bind[0].File = file
		if err := ValidateSandbox(sandbox); err == nil || !strings.Contains(err.Error(), "nor a known login file") {
			t.Errorf("ValidateSandbox(%q) = %v, want it refused", file, err)
		}
	}
}

// loadHome loads a configuration whose one job has a home at source and the deadline runtime.
func (w sandboxPathWorld) loadHome(t *testing.T, roots []string, source string, runtime int) (Config, error) {
	t.Helper()
	home := testHome()
	home.Source = source
	sandbox, err := json.Marshal(map[string]any{"workspace": filepath.Join(w.root, "ws"), "home": home})
	if err != nil {
		t.Fatalf("marshal sandbox: %v", err)
	}
	rootList, err := json.Marshal(roots)
	if err != nil {
		t.Fatalf("marshal roots: %v", err)
	}
	body := fmt.Sprintf(`{"sandbox_input_roots": %s, "event_log": {"dir": %q, "signing_key_path": %q}, "jobs": [{"name": "daemon", "command": ["/bin/true"], "log_path": "/tmp/daemon.log", "max_runtime_seconds": %d, "sandbox": %s}]}`, rootList, w.events, w.key, runtime, sandbox)
	path := filepath.Join(w.confDir, "config.json")
	if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(config) = %v, want nil", err)
	}
	return Load(testContext(t), path)
}

// A home's source is checked as an input is: in a root, clear of login locations and of the
// protected paths. It must not lie in the workspace either, which the job writes.
func TestLoadSandboxHomeSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sandbox does not run on Windows")
	}
	w := newSandboxPathWorld(t)
	w.plantLogins(t)
	ws := filepath.Join(w.root, "ws")
	slotHome := filepath.Join(w.home, "jobs", "codex-slot")
	inWorkspace := filepath.Join(ws, "home")
	for _, dir := range []string{slotHome, inWorkspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) = %v, want nil", dir, err)
		}
	}
	link := filepath.Join(w.home, "jobs", "link")
	if err := os.Symlink(slotHome, link); err != nil {
		t.Fatalf("Symlink() = %v, want nil", err)
	}
	roots := []string{ws, filepath.Join(w.home, "jobs")}
	cases := []struct {
		name   string
		roots  []string
		source string
		want   string
	}{
		{"in a root", roots, slotHome, ""},
		{"outside every root", []string{ws}, slotHome, "home.source: " + slotHome + " lies in no sandbox_input_roots entry"},
		{"the client's own login folder, named as a root", append(roots, filepath.Join(w.home, ".codex")), filepath.Join(w.home, ".codex"), "overlaps the login location"},
		{"in the workspace", roots, inWorkspace, "overlaps the workspace"},
		{"the workspace", roots, ws, "overlaps the workspace"},
		{"missing", roots, filepath.Join(w.home, "jobs", "missing"), "home.source:"},
		{"protected key folder", append(roots, w.keys), w.keys, "home.source:"},
	}
	for _, tc := range cases {
		runLoadSandboxHomeSourceCase(t, w, tc.name, tc.roots, tc.source, tc.want)
	}
	assertLoadSandboxHomeSourceEdgeCases(t, w, roots, slotHome, link)
}

func runLoadSandboxHomeSourceCase(t *testing.T, w sandboxPathWorld, name string, roots []string, source, want string) {
	t.Helper()
	_, err := w.loadHome(t, roots, source, 3600)
	if want == "" && err != nil {
		t.Errorf("%s: Load() = %v, want nil", name, err)
	}
	if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
		t.Errorf("%s: Load() = %v, want an error holding %q", name, err, want)
	}
}

func assertLoadSandboxHomeSourceEdgeCases(t *testing.T, w sandboxPathWorld, roots []string, slotHome, link string) {
	t.Helper()
	if _, err := w.loadHome(t, roots, slotHome, 0); err == nil || !strings.Contains(err.Error(), "home: needs the job's max_runtime_seconds") {
		t.Errorf("Load(home without a deadline) = %v, want the refusal", err)
	}
	cfg, err := w.loadHome(t, roots, link, 3600)
	if err != nil {
		t.Fatalf("Load(source behind a link) = %v, want nil", err)
	}
	if got := cfg.Jobs[0].Sandbox.Home.Source; got != slotHome {
		t.Fatalf("home.source = %s, want the resolved %s", got, slotHome)
	}
}
