//go:build linux

package supervisor

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

const homeTestConfig = "model = \"test\"\n"

// homeTestToken is a JSON Web Token whose payload holds exp; header and signature are not read.
func homeTestToken(exp int64) string {
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"sub":"slot","exp":%d}`, exp))
	return "eyJhbGciOiJub25lIn0." + payload + ".c2lnbmF0dXJl"
}

func homeTestAuth(exp int64) string {
	return fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":"rt-secret-value"}}`, homeTestToken(exp))
}

// homeTestSandbox makes a home source holding config.toml and auth.json and a sandbox that
// copies the one and binds the other.
func homeTestSandbox(t *testing.T, exp int64) config.SandboxConfig {
	t.Helper()
	source := t.TempDir()
	writeRT03File(t, filepath.Join(source, "config.toml"), homeTestConfig)
	writeRT03File(t, filepath.Join(source, "auth.json"), homeTestAuth(exp))
	sum := sha256.Sum256([]byte(homeTestConfig))
	return config.SandboxConfig{
		Mode: "enforce", Workspace: t.TempDir(), Network: "none", MemoryMax: "256M", CPUWeight: 100, TasksMax: 128,
		Home: &config.SandboxHome{
			Source: source, Env: "CODEX_HOME",
			Copy: []config.SandboxHomeCopy{{File: "config.toml", SHA256: hex.EncodeToString(sum[:])}},
			Bind: []config.SandboxHomeBind{{File: "auth.json", JWT: "tokens.access_token"}},
		},
	}
}

func TestShimArgsCarryTheHome(t *testing.T) {
	sandbox := homeTestSandbox(t, time.Now().Add(time.Hour).Unix())
	sandbox.Home.Bind = append(sandbox.Home.Bind, config.SandboxHomeBind{File: "plain", ExpiresMS: "login.expiresAt"})
	cfg := withShimSandbox(validShimConfig(t, "home-args"), sandbox)
	parsed, err := parseShimArgs(shimArgs(t, cfg))
	if err != nil {
		t.Fatalf("parseShimArgs() = %v, want nil", err)
	}
	got := parsed.Sandbox.Home
	if got == nil || got.Source != sandbox.Home.Source || got.Env != "CODEX_HOME" ||
		!slices.Equal(got.Copy, sandbox.Home.Copy) || !slices.Equal(got.Bind, sandbox.Home.Bind) {
		t.Fatalf("parseShimArgs() home = %+v, want %+v", got, sandbox.Home)
	}
	sandbox.Home = nil
	if args := sandboxFlagArgs(sandbox); slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "--sandbox-home") }) {
		t.Errorf("sandboxFlagArgs(no home) = %q, want no home flag", args)
	}
}

func TestShimRefusesABadHomeFlag(t *testing.T) {
	cases := []struct {
		flag  homeFlags
		want  string
		valid bool
	}{
		{homeFlags{}, "", true},
		{homeFlags{source: "/s", env: "E", copies: []string{"config.toml"}}, "is no file=sha256", false},
		{homeFlags{source: "/s", env: "E", binds: []string{"auth.json"}}, "is no file=jwt", false},
		{homeFlags{source: "/s", env: "E", binds: []string{"auth.json="}}, "", true},
		{homeFlags{source: "/s", env: "E", binds: []string{"auth.json==a.b"}}, "", true},
	}
	for _, tc := range cases {
		_, err := tc.flag.home()
		if tc.valid != (err == nil) || (!tc.valid && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("home(%+v) = %v, want valid=%v %q", tc.flag, err, tc.valid, tc.want)
		}
	}
	cfg := withShimSandbox(validShimConfig(t, "home-bad"), homeTestSandbox(t, time.Now().Add(time.Hour).Unix()))
	cfg.Sandbox.Home.Env = "PATH"
	if _, err := parseShimArgs(shimArgs(t, cfg)); err == nil || !strings.Contains(err.Error(), "home.env: PATH is set by the sandbox") {
		t.Fatalf("parseShimArgs(home env PATH) = %v, want the configuration's refusal", err)
	}
}

func TestPrepareHomeMakesAFreshHome(t *testing.T) {
	until := time.Now().Add(time.Hour)
	sandbox := homeTestSandbox(t, until.Add(homeTokenMargin+time.Minute).Unix())
	dir := homeDir(sandbox)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	writeRT03File(t, filepath.Join(dir, "left-over"), "from an earlier start")
	if err := prepareHome(sandbox, until); err != nil {
		t.Fatalf("prepareHome() = %v, want nil", err)
	}
	checkHomeContents(t, dir)
	if err := prepareHome(config.SandboxConfig{Workspace: sandbox.Workspace}, until); err != nil {
		t.Errorf("prepareHome(no home) = %v, want nil", err)
	}
}

func checkHomeContents(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() = %v, want nil", err)
	}
	names := make([]string, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		names = append(names, entries[i].Name())
	}
	if !slices.Equal(names, []string{"auth.json", "config.toml"}) {
		t.Fatalf("home holds %q, want the copy and the bind's placeholder alone", names)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "config.toml")); err != nil || string(body) != homeTestConfig {
		t.Errorf("config.toml = %q, %v, want the source's", body, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "auth.json")); err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("auth.json placeholder = %v, %v, want an empty 0600 file the login is bound over", info, err)
	}
}

func TestPrepareHomeRefuses(t *testing.T) {
	// Whole seconds, so that a token expiring at the limit is exactly at it.
	until := time.Now().Truncate(time.Second).Add(time.Hour)
	valid := until.Add(homeTokenMargin + time.Minute).Unix()
	cases := []struct {
		name  string
		exp   int64
		until time.Time
		edit  func(*testing.T, config.SandboxConfig)
		want  string
	}{
		{"unknown end", valid, time.Time{}, nil, "latest end is unknown"},
		{"token expires at the margin's end", until.Add(homeTokenMargin).Unix(), until, nil, "expires before the run can end"},
		{"token expired", until.Add(-2 * time.Hour).Unix(), until, nil, "expires before the run can end"},
		{"config changed", valid, until, func(t *testing.T, s config.SandboxConfig) {
			writeRT03File(t, filepath.Join(s.Home.Source, "config.toml"), "model = \"other\"\n")
		}, "copy config.toml hashes to"},
		{"config a symlink", valid, until, func(t *testing.T, s config.SandboxConfig) {
			target := filepath.Join(t.TempDir(), "config.toml")
			writeRT03File(t, target, homeTestConfig)
			replaceWithSymlink(t, target, filepath.Join(s.Home.Source, "config.toml"))
		}, "is no regular file"},
		{"auth a symlink", valid, until, func(t *testing.T, s config.SandboxConfig) {
			target := filepath.Join(t.TempDir(), "auth.json")
			writeRT03File(t, target, homeTestAuth(valid))
			replaceWithSymlink(t, target, filepath.Join(s.Home.Source, "auth.json"))
		}, "is no regular file"},
		{"auth missing", valid, until, func(t *testing.T, s config.SandboxConfig) {
			if err := os.Remove(filepath.Join(s.Home.Source, "auth.json")); err != nil {
				t.Fatalf("Remove() = %v, want nil", err)
			}
		}, "auth.json"},
		{".slot a symlink the job left", valid, until, func(t *testing.T, s config.SandboxConfig) {
			if err := os.Symlink(t.TempDir(), filepath.Join(s.Workspace, config.SandboxHomeTop)); err != nil {
				t.Fatalf("Symlink() = %v, want nil", err)
			}
		}, "is no directory"},
		{"home a file the job left", valid, until, func(t *testing.T, s config.SandboxConfig) {
			if err := os.Mkdir(filepath.Join(s.Workspace, config.SandboxHomeTop), 0o700); err != nil {
				t.Fatalf("Mkdir() = %v, want nil", err)
			}
			writeRT03File(t, homeDir(s), "")
		}, "is no directory"},
		{"source missing", valid, until, func(t *testing.T, s config.SandboxConfig) {
			if err := os.RemoveAll(s.Home.Source); err != nil {
				t.Fatalf("RemoveAll() = %v, want nil", err)
			}
		}, "home"},
	}
	for _, tc := range cases {
		checkPrepareHomeRefusalCase(t, tc.name, tc.exp, tc.until, tc.edit, tc.want)
	}
	expired := homeTestSandbox(t, until.Unix())
	if err := prepareHome(expired, until); !errors.Is(err, errHomeToken) {
		t.Errorf("prepareHome(expired) = %v, want errHomeToken", err)
	}
}

func checkPrepareHomeRefusalCase(t *testing.T, name string, exp int64, until time.Time, edit func(*testing.T, config.SandboxConfig), want string) {
	t.Helper()
	sandbox := homeTestSandbox(t, exp)
	if edit != nil {
		edit(t, sandbox)
	}
	err := prepareHome(sandbox, until)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: prepareHome() = %v, want an error holding %q", name, err, want)
	}
	if err != nil && strings.Contains(err.Error(), "rt-secret-value") {
		t.Errorf("%s: prepareHome() error carries the refresh token: %v", name, err)
	}
}

func replaceWithSymlink(t *testing.T, target string, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() = %v, want nil", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink() = %v, want nil", err)
	}
}

// The parser's errors name what is wrong and never the token: an error lands in logs.
func TestJWTExpiry(t *testing.T) {
	token := homeTestToken(1900000000)
	cases := []struct {
		body string
		path string
		want string
	}{
		{fmt.Sprintf(`{"t":{"a":%q}}`, token), "t.a", ""},
		{`not json ` + token, "t.a", "no JSON document"},
		{fmt.Sprintf(`{"t":[%q]}`, token), "t.a", "through no object"},
		{fmt.Sprintf(`{"t":{"b":%q}}`, token), "t.a", "names no value"},
		{`{"t":{"a":7}}`, "t.a", "no string"},
		{`{"t":{"a":"one.two"}}`, "t.a", "no JSON Web Token"},
		{`{"t":{"a":"h.!!!.s"}}`, "t.a", "no base64url"},
		{fmt.Sprintf(`{"t":{"a":"h.%s.s"}}`, base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`))), "t.a", "no exp claim"},
		{fmt.Sprintf(`{"t":{"a":"h.%s.s"}}`, base64.RawURLEncoding.EncodeToString([]byte(`{"exp":"soon"}`))), "t.a", "no exp claim"},
		{fmt.Sprintf(`{"t":{"a":"h.%s.s"}}`, base64.RawURLEncoding.EncodeToString([]byte(`{"exp":-1}`))), "t.a", "is no time"},
		{fmt.Sprintf(`{"t":{"a":"h.%s.s"}}`, base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1e300}`))), "t.a", "is no time"},
	}
	for _, tc := range cases {
		exp, err := jwtExpiry([]byte(tc.body), tc.path)
		if tc.want == "" {
			if err != nil || exp.Unix() != 1900000000 {
				t.Errorf("jwtExpiry(%s) = %v, %v, want 1900000000", tc.body, exp, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("jwtExpiry(%s) = %v, want an error holding %q", tc.body, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "eyJ") {
			t.Errorf("jwtExpiry(%s) error carries the token: %v", tc.body, err)
		}
	}
}

func TestBwrapArgsBindTheHome(t *testing.T) {
	sandbox := homeTestSandbox(t, time.Now().Add(time.Hour).Unix())
	args := appendHomeArgs(nil, sandbox)
	dir := homeDir(sandbox)
	want := []string{
		"--ro-bind", filepath.Join(sandbox.Home.Source, "auth.json"), filepath.Join(dir, "auth.json"),
		"--setenv", "CODEX_HOME", dir,
	}
	if !slices.Equal(args, want) {
		t.Fatalf("appendHomeArgs() = %q, want %q", args, want)
	}
	if slices.Contains(args, filepath.Join(sandbox.Home.Source, "config.toml")) {
		t.Errorf("appendHomeArgs() = %q, want the copy file left unbound", args)
	}
}

// The job reads its bound login and its copied settings through the home variable, and
// cannot change the login: the bind is read-only, and it cannot be renamed away either.
func TestSandboxedJobReadsItsHomeAndCannotWriteTheLogin(t *testing.T) {
	tools := rt03Tools(t, "none")
	sandbox := homeTestSandbox(t, time.Now().Add(24*time.Hour).Unix())
	script := `cat "$CODEX_HOME/config.toml" > settings.txt
cat "$CODEX_HOME/auth.json" > login.txt
if echo changed > "$CODEX_HOME/auth.json"; then echo wrote >> writes.txt; fi
if mv "$CODEX_HOME/auth.json" "$CODEX_HOME/moved" 2>/dev/null; then echo moved >> writes.txt; fi
if rm -f "$CODEX_HOME/auth.json" 2>/dev/null && [ ! -e "$CODEX_HOME/auth.json" ]; then echo removed >> writes.txt; fi
: >> writes.txt`
	built, err := buildSandboxCommand([]string{"/usr/bin/bash", "-c", script}, sandbox, sandboxBuildOptions{Tools: tools, Env: os.LookupEnv, Until: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("buildSandboxCommand() = %v, want nil", err)
	}
	if !runRT03Command(t, built) {
		t.Fatal("the job failed, want it to run and record what it could do")
	}
	read := func(name string) string {
		body, err := os.ReadFile(filepath.Join(sandbox.Workspace, name))
		if err != nil {
			t.Fatalf("ReadFile(%s) = %v, want nil", name, err)
		}
		return string(body)
	}
	if got := read("settings.txt"); got != homeTestConfig {
		t.Errorf("the job read settings %q, want %q", got, homeTestConfig)
	}
	if got, want := read("login.txt"), homeTestAuth(time.Now().Add(24*time.Hour).Unix()); !strings.Contains(got, "rt-secret-value") || len(got) != len(want) {
		t.Errorf("the job read login %q, want the bound file", got)
	}
	if got := read("writes.txt"); got != "" {
		t.Errorf("the job changed its login: %q, want nothing", got)
	}
	if body, err := os.ReadFile(filepath.Join(sandbox.Home.Source, "auth.json")); err != nil || !strings.Contains(string(body), "rt-secret-value") {
		t.Errorf("source auth.json = %q, %v, want it unchanged", body, err)
	}
}

func TestBuildSandboxCommandRefusesAnExpiringLogin(t *testing.T) {
	sandbox := homeTestSandbox(t, time.Now().Add(30*time.Minute).Unix())
	_, err := buildSandboxCommand([]string{"/bin/true"}, sandbox, sandboxBuildOptions{Tools: sandboxTools{Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env"}, Env: os.LookupEnv, Until: time.Now().Add(time.Hour)})
	if !errors.Is(err, errHomeToken) {
		t.Fatalf("buildSandboxCommand(login expiring in the run) = %v, want errHomeToken", err)
	}
}

func TestLatestEndCountsTheDeadlineAndTheGrace(t *testing.T) {
	now := time.Unix(1800000000, 0)
	cfg := shimConfig{MaxRuntimeSeconds: 600, StopGraceSeconds: 30}
	if got, want := latestEnd(cfg, now), now.Add(630*time.Second); !got.Equal(want) {
		t.Errorf("latestEnd(600 s, grace 30 s) = %s, want %s", got, want)
	}
	cfg.MaxRuntimeSeconds = 0
	if got, want := latestEnd(cfg, now), now.Add(time.Duration(config.MaxJobRuntimeSeconds+30)*time.Second); !got.Equal(want) {
		t.Errorf("latestEnd(no deadline) = %s, want the longest runtime: %s", got, want)
	}
	cfg = shimConfig{EventLogDir: "/events", SigningKey: "/key", MaxRuntimeSeconds: 60}
	opts := shimBuildOptions(cfg, sandboxProxy{Socket: "/s"}, now)
	if !opts.Until.Equal(now.Add(time.Minute)) || opts.Proxy.Socket != "/s" || opts.Paths.EventLogDir != "/events" || opts.Paths.SigningKey != "/key" || opts.Env == nil {
		t.Errorf("shimBuildOptions() = %+v, want the job's latest end, proxy, paths and environment", opts)
	}
}
