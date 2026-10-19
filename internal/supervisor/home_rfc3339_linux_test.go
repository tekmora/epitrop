//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

const homeAgyLogin = ".gemini/antigravity-cli/antigravity-oauth-token"

// homeTestAgyToken is a login file shaped as the Antigravity CLI keeps its token: the
// expiry an RFC 3339 string beside the tokens.
func homeTestAgyToken(expiry string) string {
	return fmt.Sprintf(`{"token":{"access_token":"at-secret-value","token_type":"Bearer","refresh_token":"rt-secret-value","expiry":%q},"auth_method":"oauth"}`, expiry)
}

// homeAgySandbox makes a home that takes HOME and binds the agy login file with its expiry
// at token.expiry.
func homeAgySandbox(t *testing.T, expiry string) config.SandboxConfig {
	t.Helper()
	sandbox := homeTestSandbox(t, time.Now().Add(time.Hour).Unix())
	source := sandbox.Home.Source
	if err := os.MkdirAll(filepath.Join(source, ".gemini", "antigravity-cli"), 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	writeRT03File(t, filepath.Join(source, filepath.FromSlash(homeAgyLogin)), homeTestAgyToken(expiry))
	sandbox.Home.Env = "HOME"
	sandbox.Home.Bind = []config.SandboxHomeBind{{File: homeAgyLogin, ExpiresRFC3339: "token.expiry"}}
	return sandbox
}

// The parser's errors name what is wrong and never the token or the value: an error lands
// in logs.
func TestRFC3339Expiry(t *testing.T) {
	cases := []struct {
		body string
		want string
		at   time.Time
	}{
		{`{"o":{"e":"2030-01-02T03:04:05Z"}}`, "", time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)},
		{`{"o":{"e":"2030-01-02T05:04:05.123456789+02:00"}}`, "", time.Date(2030, 1, 2, 3, 4, 5, 123456789, time.UTC)},
		{`not json at-secret-value`, "no JSON document", time.Time{}},
		{`{"o":["2030-01-02T03:04:05Z"]}`, "through no object", time.Time{}},
		{`{"o":{"f":"2030-01-02T03:04:05Z"}}`, "names no value", time.Time{}},
		{`{"o":{"e":1893553445}}`, "no string", time.Time{}},
		{`{"o":{"e":null}}`, "no string", time.Time{}},
		{`{"o":{"e":""}}`, "no RFC 3339 time", time.Time{}},
		{`{"o":{"e":"2030-01-02"}}`, "no RFC 3339 time", time.Time{}},
		{`{"o":{"e":"2030-01-02T03:04:05"}}`, "no RFC 3339 time", time.Time{}},
		{`{"o":{"e":"Wed, 02 Jan 2030 03:04:05 GMT"}}`, "no RFC 3339 time", time.Time{}},
		{`{"o":{"e":"rt-secret-value"}}`, "no RFC 3339 time", time.Time{}},
	}
	for _, tc := range cases {
		exp, err := rfc3339Expiry([]byte(tc.body), "o.e")
		if tc.want == "" {
			if err != nil || !exp.Equal(tc.at) {
				t.Errorf("rfc3339Expiry(%s) = %v, %v, want %v", tc.body, exp, err, tc.at)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("rfc3339Expiry(%s) = %v, want an error holding %q", tc.body, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "2030")) {
			t.Errorf("rfc3339Expiry(%s) error carries the value: %v", tc.body, err)
		}
	}
}

// An agy login whose expiry ends at the margin's end is refused, one a second later taken;
// its two folders are made in the fresh home and the placeholder lies where agy looks.
func TestPrepareHomeChecksAnRFC3339Expiry(t *testing.T) {
	until := time.Now().Truncate(time.Second).Add(time.Hour)
	limit := until.Add(homeTokenMargin)
	err := prepareHome(homeAgySandbox(t, limit.Format(time.RFC3339)), until)
	if err == nil || !strings.Contains(err.Error(), "expires before the run can end") {
		t.Fatalf("prepareHome(expiry at the margin's end) = %v, want it refused", err)
	}
	sandbox := homeAgySandbox(t, limit.Add(time.Second).Format(time.RFC3339))
	if err = prepareHome(sandbox, until); err != nil {
		t.Fatalf("prepareHome(expiry a second beyond) = %v, want nil", err)
	}
	info, err := os.Lstat(filepath.Join(homeDir(sandbox), filepath.FromSlash(homeAgyLogin)))
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("placeholder %s = %v, %v, want an empty 0600 file", homeAgyLogin, info, err)
	}
}

func TestPrepareHomeRefusesAnAgyLoginItCannotRead(t *testing.T) {
	until := time.Now().Truncate(time.Second).Add(time.Hour)
	valid := until.Add(homeTokenMargin + time.Minute).Format(time.RFC3339)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"expiry missing", `{"token":{"refresh_token":"rt-secret-value"}}`, "names no value"},
		{"expiry a number", `{"token":{"refresh_token":"rt-secret-value","expiry":1893553445}}`, "no string"},
		{"expiry unparsable", `{"token":{"refresh_token":"rt-secret-value","expiry":"rt-secret-value"}}`, "no RFC 3339 time"},
		{"expiry only in the id token", `{"id_token":"a.b.c","token":{"refresh_token":"rt-secret-value"}}`, "names no value"},
	}
	for _, tc := range cases {
		sandbox := homeAgySandbox(t, valid)
		writeRT03File(t, filepath.Join(sandbox.Home.Source, filepath.FromSlash(homeAgyLogin)), tc.body)
		err := prepareHome(sandbox, until)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: prepareHome() = %v, want an error holding %q", tc.name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: prepareHome() error carries a token: %v", tc.name, err)
		}
	}
}

// The shim gets every expiry kind through its flags, the RFC 3339 one included.
func TestHomeFlagsCarryEveryExpiryKind(t *testing.T) {
	home := &config.SandboxHome{Source: "/s", Env: "HOME", Bind: []config.SandboxHomeBind{
		{File: "auth.json", JWT: "tokens.access_token"},
		{File: ".credentials.json", ExpiresMS: "claudeAiOauth.expiresAt"},
		{File: homeAgyLogin, ExpiresRFC3339: "token.expiry"},
	}}
	args := homeFlagArgs(home)
	var flags homeFlags
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--sandbox-home-source":
			flags.source = args[i+1]
		case "--sandbox-home-env":
			flags.env = args[i+1]
		case "--sandbox-home-bind":
			flags.binds = append(flags.binds, args[i+1])
		}
	}
	got, err := flags.home()
	if err != nil || len(got.Bind) != len(home.Bind) {
		t.Fatalf("home() = %+v, %v, want the binds back", got, err)
	}
	for i := 0; i < len(home.Bind); i++ {
		if got.Bind[i] != home.Bind[i] {
			t.Errorf("bind %d = %+v, want %+v", i, got.Bind[i], home.Bind[i])
		}
	}
}
