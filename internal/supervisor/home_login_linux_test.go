//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

const homeNestedLogin = ".client/login/token.json"

// homeTestCredentials is a login file that keeps its expiry in Unix milliseconds beside the
// tokens, as Claude Code keeps .credentials.json.
func homeTestCredentials(expMS int64) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"at-secret-value","refreshToken":"rt-secret-value","expiresAt":%d}}`, expMS)
}

// homeLoginSandbox makes a home that takes HOME and binds .credentials.json and a login file
// two folders down, each with its expiry in milliseconds.
func homeLoginSandbox(t *testing.T, expMS int64) config.SandboxConfig {
	t.Helper()
	sandbox := homeTestSandbox(t, time.Now().Add(time.Hour).Unix())
	source := sandbox.Home.Source
	writeRT03File(t, filepath.Join(source, ".credentials.json"), homeTestCredentials(expMS))
	if err := os.MkdirAll(filepath.Join(source, ".client", "login"), 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	writeRT03File(t, filepath.Join(source, filepath.FromSlash(homeNestedLogin)), homeTestCredentials(expMS))
	sandbox.Home.Env = "HOME"
	sandbox.Home.Bind = []config.SandboxHomeBind{
		{File: ".credentials.json", ExpiresMS: "claudeAiOauth.expiresAt"},
		{File: homeNestedLogin, ExpiresMS: "claudeAiOauth.expiresAt"},
	}
	return sandbox
}

// The parser's errors name what is wrong and never the token: an error lands in logs.
func TestMillisExpiry(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"o":{"e":1900000000123}}`, ""},
		{`{"o":{"e":1}}`, ""},
		{`not json at-secret-value`, "no JSON document"},
		{`{"o":[1900000000123]}`, "through no object"},
		{`{"o":{"f":1900000000123}}`, "names no value"},
		{`{"o":{"e":"1900000000123"}}`, "no number"},
		{`{"o":{"e":0}}`, "no time in milliseconds"},
		{`{"o":{"e":-5}}`, "no time in milliseconds"},
		{`{"o":{"e":1900000000123.5}}`, "no time in milliseconds"},
		{`{"o":{"e":1e300}}`, "no time in milliseconds"},
		{`{"o":{"e":4611686018427387903}}`, "no time in milliseconds"},
	}
	for _, tc := range cases {
		exp, err := millisExpiry([]byte(tc.body), "o.e")
		if tc.want == "" {
			var want int64
			if _, scanErr := fmt.Sscanf(tc.body, `{"o":{"e":%d}}`, &want); scanErr != nil || err != nil || exp.UnixMilli() != want {
				t.Errorf("millisExpiry(%s) = %v, %v, want %d ms", tc.body, exp, err, want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("millisExpiry(%s) = %v, want an error holding %q", tc.body, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("millisExpiry(%s) error carries the token: %v", tc.body, err)
		}
	}
}

// A login whose expiry in milliseconds ends at the margin's end is refused, one a
// millisecond later taken; folders a login file lies in are made in the fresh home.
func TestPrepareHomeChecksAnExpiryInMilliseconds(t *testing.T) {
	until := time.Now().Truncate(time.Second).Add(time.Hour)
	limit := until.Add(homeTokenMargin).UnixMilli()
	if err := prepareHome(homeLoginSandbox(t, limit), until); err == nil || !strings.Contains(err.Error(), "expires before the run can end") {
		t.Fatalf("prepareHome(expiry at the margin's end) = %v, want it refused", err)
	}
	sandbox := homeLoginSandbox(t, limit+1)
	if err := prepareHome(sandbox, until); err != nil {
		t.Fatalf("prepareHome(expiry a millisecond beyond) = %v, want nil", err)
	}
	checkHomeLoginContents(t, homeDir(sandbox))
}

// checkHomeLoginContents checks that both logins' placeholders and the folders of the nested
// one are in the home.
func checkHomeLoginContents(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{".credentials.json", homeNestedLogin} {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || !info.Mode().IsRegular() || info.Size() != 0 || info.Mode().Perm() != 0o600 {
			t.Errorf("placeholder %s = %v, %v, want an empty 0600 file", name, info, err)
		}
	}
	for _, name := range []string{".client", ".client/login"} {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("folder %s = %v, %v, want a 0700 directory", name, info, err)
		}
	}
}

func TestPrepareHomeRefusesALoginItCannotTrust(t *testing.T) {
	until := time.Now().Truncate(time.Second).Add(time.Hour)
	valid := until.Add(homeTokenMargin + time.Minute).UnixMilli()
	cases := []struct {
		name string
		edit func(*testing.T, config.SandboxConfig)
		want string
	}{
		{"expiry a string", func(t *testing.T, s config.SandboxConfig) {
			writeRT03File(t, filepath.Join(s.Home.Source, ".credentials.json"), `{"claudeAiOauth":{"refreshToken":"rt-secret-value","expiresAt":"soon"}}`)
		}, "no number"},
		{"expiry missing", func(t *testing.T, s config.SandboxConfig) {
			writeRT03File(t, filepath.Join(s.Home.Source, ".credentials.json"), `{"claudeAiOauth":{"refreshToken":"rt-secret-value"}}`)
		}, "names no value"},
		{"bind naming no expiry", func(_ *testing.T, s config.SandboxConfig) { s.Home.Bind[0].ExpiresMS = "" }, "names no expiry"},
		{"a folder on the way a symlink", func(t *testing.T, s config.SandboxConfig) {
			target := t.TempDir()
			if err := os.Rename(filepath.Join(s.Home.Source, ".client", "login"), filepath.Join(target, "login")); err != nil {
				t.Fatalf("Rename() = %v, want nil", err)
			}
			if err := os.Remove(filepath.Join(s.Home.Source, ".client")); err != nil {
				t.Fatalf("Remove() = %v, want nil", err)
			}
			if err := os.Symlink(target, filepath.Join(s.Home.Source, ".client")); err != nil {
				t.Fatalf("Symlink() = %v, want nil", err)
			}
		}, "is no directory"},
		{"a folder on the way a file", func(t *testing.T, s config.SandboxConfig) {
			if err := os.RemoveAll(filepath.Join(s.Home.Source, ".client", "login")); err != nil {
				t.Fatalf("RemoveAll() = %v, want nil", err)
			}
			writeRT03File(t, filepath.Join(s.Home.Source, ".client", "login"), "")
		}, "is no directory"},
	}
	for _, tc := range cases {
		sandbox := homeLoginSandbox(t, valid)
		tc.edit(t, sandbox)
		err := prepareHome(sandbox, until)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: prepareHome() = %v, want an error holding %q", tc.name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: prepareHome() error carries a token: %v", tc.name, err)
		}
	}
}

func TestAppendHomeArgsPinsTheFoldersOfANestedLogin(t *testing.T) {
	sandbox := homeLoginSandbox(t, time.Now().Add(time.Hour).UnixMilli())
	dir, source := homeDir(sandbox), sandbox.Home.Source
	want := []string{
		"--bind", filepath.Join(dir, ".client"), filepath.Join(dir, ".client"),
		"--bind", filepath.Join(dir, ".client", "login"), filepath.Join(dir, ".client", "login"),
		"--ro-bind", filepath.Join(source, ".credentials.json"), filepath.Join(dir, ".credentials.json"),
		"--ro-bind", filepath.Join(source, ".client", "login", "token.json"), filepath.Join(dir, ".client", "login", "token.json"),
	}
	if got := appendHomeArgs(nil, sandbox); !slices.Equal(got, want) {
		t.Fatalf("appendHomeArgs() = %q, want %q", got, want)
	}
}

// A home that takes HOME is the job's one HOME; any other home leaves HOME at the workspace.
func TestBwrapArgsSetHOMEOnce(t *testing.T) {
	sandbox := homeLoginSandbox(t, time.Now().Add(time.Hour).UnixMilli())
	tools := sandboxTools{Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env"}
	for _, tc := range []struct{ env, home string }{{"HOME", homeDir(sandbox)}, {"CODEX_HOME", sandbox.Workspace}} {
		sandbox.Home.Env = tc.env
		args := bwrapArgs("/usr/bin/true", []string{"/usr/bin/true"}, sandbox, tools, sandboxProxy{}, nil)
		var homes []string
		for i := 0; i+2 < len(args); i++ {
			if args[i] == "--setenv" && args[i+1] == "HOME" {
				homes = append(homes, args[i+2])
			}
		}
		if len(homes) != 1 || homes[0] != tc.home {
			t.Errorf("bwrapArgs(home.env %s) sets HOME to %q, want once to %s", tc.env, homes, tc.home)
		}
	}
	sandbox.Home = nil
	args := strings.Join(bwrapArgs("/usr/bin/true", []string{"/usr/bin/true"}, sandbox, tools, sandboxProxy{}, nil), " ")
	if !strings.Contains(args, "--setenv HOME "+sandbox.Workspace+" ") {
		t.Errorf("bwrapArgs(no home) = %q, want HOME at the workspace", args)
	}
}

// The job finds its logins below HOME, the one below two folders included, and can change
// neither of them.
func TestSandboxedJobReadsItsLoginsBelowHOME(t *testing.T) {
	tools := rt03Tools(t, "none")
	sandbox := homeLoginSandbox(t, time.Now().Add(24*time.Hour).UnixMilli())
	script := `printf '%s' "$HOME" > home.txt
cat "$HOME/.credentials.json" > login.txt
cat "$HOME/` + homeNestedLogin + `" > nested.txt
for f in "$HOME/.credentials.json" "$HOME/` + homeNestedLogin + `"; do
	if echo changed > "$f"; then echo "wrote $f" >> writes.txt; fi
	if mv "$f" "$f.moved" 2>/dev/null; then echo "moved $f" >> writes.txt; fi
done
if mv "$HOME/.client" "$HOME/.aside" 2>/dev/null; then echo "moved the folder" >> writes.txt; fi
if mv "$HOME/.client/login" "$HOME/.client/aside" 2>/dev/null; then echo "moved the inner folder" >> writes.txt; fi
: >> writes.txt`
	built, err := buildSandboxCommand([]string{"/usr/bin/bash", "-c", script}, sandbox, sandboxBuildOptions{Tools: tools, Env: os.LookupEnv, Until: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("buildSandboxCommand() = %v, want nil", err)
	}
	if !runRT03Command(t, built) {
		t.Fatal("the job failed, want it to run and record what it could do")
	}
	read := func(name string) string {
		body, err := os.ReadFile(filepath.Join(sandbox.Workspace, name)) // #nosec G304 -- path inside the test's own workspace.
		if err != nil {
			t.Fatalf("ReadFile(%s) = %v, want nil", name, err)
		}
		return string(body)
	}
	if got := read("home.txt"); got != homeDir(sandbox) {
		t.Errorf("the job's HOME = %q, want its home %s", got, homeDir(sandbox))
	}
	for _, name := range []string{"login.txt", "nested.txt"} {
		if got := read(name); !strings.Contains(got, "rt-secret-value") {
			t.Errorf("the job read %s = %q, want the bound login", name, got)
		}
	}
	if got := read("writes.txt"); got != "" {
		t.Errorf("the job changed its logins: %q, want nothing", got)
	}
}
