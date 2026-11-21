package main

import (
	"context"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixture callers supply fixed git init and add arguments
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	fixtureGit(t, dir, "init", "-q")
	if _, ok := files["go.mod"]; !ok {
		files["go.mod"] = "module example.com/fixture\n\ngo 1.27\n"
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixtureGit(t, dir, "add", "--", ".")
	return dir
}

func excludedOS() string {
	if build.Default.GOOS == "windows" {
		return "linux"
	}
	return "windows"
}

func TestDiscover(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		want      []string
		errorText string
	}{
		{"none", map[string]string{"a.go": "package a\n"}, []string{}, ""},
		{"tagged packages", map[string]string{"a.go": "//go:build alpha\n\npackage a\n", "nested/b.go": "//go:build beta\n\npackage b\n"}, []string{"alpha", "beta"}, ""},
		{"toolchain", map[string]string{"a.go": "//go:build linux || windows || amd64 || unix || cgo || gc || gccgo || go1.21 || goexperiment.x || amd64.v2\n\npackage a\n"}, []string{}, ""},
		{"os feature name", map[string]string{"a.go": "//go:build linux.x\n\npackage a\n"}, []string{"linux.x"}, ""},
		{"defined feature tags", map[string]string{"a.go": "//go:build 386.387 || 386.sse2 || 386.softfloat || amd64.v1 || amd64.v3 || amd64.v4 || arm.5 || arm.7 || arm64.v8.0 || arm64.v9.5 || mips.hardfloat || mipsle.softfloat || mips64.softfloat || mips64le.hardfloat || ppc64.power8 || ppc64le.power10 || riscv64.rva20u64 || riscv64.rva23u64 || wasm.satconv || wasm.signext\n\npackage a\n"}, []string{}, ""},
		{"arch prefix custom", map[string]string{"a.go": "//go:build amd64.audit\n\npackage a\n"}, []string{"amd64.audit"}, ""},
		{"undefined feature names", map[string]string{"a.go": "//go:build amd64.v5 || 386.sse3 || arm64.v9.6 || arm64.v8 || riscv64.rva23 || wasm.audit\n\npackage a\n"}, []string{"386.sse3", "amd64.v5", "arm64.v8", "arm64.v9.6", "riscv64.rva23", "wasm.audit"}, ""},
		{"feature and custom", map[string]string{"a.go": "//go:build amd64.v2 || amd64.audit\n\npackage a\n"}, []string{"amd64.audit"}, ""},
		{"negation", map[string]string{"a.go": "//go:build !alpha\n\npackage a\n"}, []string{"alpha"}, ""},
		{"disjunction", map[string]string{"a.go": "//go:build alpha || beta\n\npackage a\n"}, []string{"alpha", "beta"}, ""},
		{"gap", map[string]string{"gap.go": "//go:build alpha && !beta\n\npackage a\n"}, nil, "gap.go"},
		{"bound", map[string]string{"bound.go": "//go:build a || b || c || d || e || f || g || h || i\n\npackage a\n"}, nil, "bound.go"},
		{"os excluded", map[string]string{"x_" + excludedOS() + ".go": "//go:build alpha && !beta\n\npackage a\n"}, []string{"alpha", "beta"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := fixture(t, tc.files)
			got, err := discover(dir)
			if tc.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorText) {
					t.Fatalf("error = %v, want %q", err, tc.errorText)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func checkDiscoverResult(t *testing.T, got []string, err error, want, wantErr []string) {
	t.Helper()
	if len(wantErr) > 0 {
		if err == nil {
			t.Fatalf("got %v, want error containing %v", got, wantErr)
		}
		for _, text := range wantErr {
			if !strings.Contains(err.Error(), text) {
				t.Fatalf("error = %v, want %q", err, text)
			}
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverPlusBuild(t *testing.T) {
	otherOSFile := "x_" + excludedOS() + ".go"
	cases := []struct {
		name    string
		files   map[string]string
		want    []string
		wantErr []string
	}{
		{"single", map[string]string{"p.go": "// +build alpha\n\npackage a\n"}, nil, []string{"p.go", `"// +build alpha"`, "//go:build"}},
		{"multiple", map[string]string{"two.go": "// +build alpha\n// +build beta\n\npackage a\n"}, nil, []string{"two.go", `"// +build alpha"`, "//go:build"}},
		{"negated", map[string]string{"neg.go": "// +build !alpha\n\npackage a\n"}, nil, []string{"neg.go", `"// +build !alpha"`, "//go:build"}},
		{"os excluded", map[string]string{otherOSFile: "// +build alpha\n\npackage a\n"}, nil, []string{otherOSFile, `"// +build alpha"`, "//go:build"}},
		{"gobuild before plusbuild", map[string]string{"a.go": "//go:build alpha\n// +build alpha\n\npackage a\n"}, []string{"alpha"}, nil},
		{"plusbuild before gobuild", map[string]string{"a.go": "// +build alpha\n//go:build alpha\n\npackage a\n"}, []string{"alpha"}, nil},
		{"after package", map[string]string{"a.go": "package a\n\n// +build alpha\n"}, []string{}, nil},
		{"plain comment", map[string]string{"a.go": "// plain comment\n\npackage a\n"}, []string{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := fixture(t, tc.files)
			got, err := discover(dir)
			checkDiscoverResult(t, got, err, tc.want, tc.wantErr)
		})
	}
}

// TestHostToolTags checks the feature tags the running toolchain sets for this
// host against the defined list, so a level the list misses fails here.
func TestHostToolTags(t *testing.T) {
	known, err := toolchainTags()
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range build.Default.ToolTags {
		if isCustom(tag, known) {
			t.Errorf("toolchain tag %q is reported as custom", tag)
		}
	}
	if !isCustom("amd64.audit", known) {
		t.Error("amd64.audit is not reported as custom")
	}
}

// featureTagsFor asks the go command which feature tags it sets for one spec:
// GOOS, GOARCH and optional NAME=value feature settings.
func featureTagsFor(t *testing.T, spec string) []string {
	t.Helper()
	fields := strings.Fields(spec)
	t.Setenv("GOOS", fields[0])
	t.Setenv("GOARCH", fields[1])
	for _, pair := range fields[2:] {
		name, value, _ := strings.Cut(pair, "=")
		t.Setenv(name, value)
	}
	out, err := goCommand("list", "-find", "-f", "{{join context.ToolTags \" \"}}", "unsafe")
	if err != nil {
		t.Fatalf("%s: %v", spec, err)
	}
	var tags []string
	for _, tag := range strings.Fields(string(out)) {
		if !strings.HasPrefix(tag, "goexperiment.") {
			tags = append(tags, tag)
		}
	}
	return tags
}

// TestFeatureTagsMatchToolchain checks featureTags both ways against the go
// command: each architecture at its highest level, and each value where levels
// do not include each other. A tag the toolchain sets must be listed, and a
// listed tag must be one the toolchain sets.
func TestFeatureTagsMatchToolchain(t *testing.T) {
	listed := map[string]bool{}
	for _, word := range strings.Fields(featureTags) {
		listed[word] = true
	}
	seen := map[string]bool{}
	for _, spec := range []string{
		"linux 386 GO386=387", "linux 386 GO386=sse2", "linux 386 GO386=softfloat",
		"linux amd64 GOAMD64=v4", "linux arm GOARM=7", "linux arm64 GOARM64=v9.5",
		"linux mips GOMIPS=hardfloat", "linux mips GOMIPS=softfloat",
		"linux mipsle GOMIPS=hardfloat", "linux mipsle GOMIPS=softfloat",
		"linux mips64 GOMIPS64=hardfloat", "linux mips64 GOMIPS64=softfloat",
		"linux mips64le GOMIPS64=hardfloat", "linux mips64le GOMIPS64=softfloat",
		"linux ppc64 GOPPC64=power10", "linux ppc64le GOPPC64=power10",
		"linux riscv64 GORISCV64=rva23u64", "js wasm",
	} {
		t.Run(spec, func(t *testing.T) {
			for _, tag := range featureTagsFor(t, spec) {
				seen[tag] = true
				if !listed[tag] {
					t.Errorf("toolchain tag %q is missing from featureTags", tag)
				}
			}
		})
	}
	for word := range listed {
		if !seen[word] {
			t.Errorf("featureTags word %q is not set by the toolchain", word)
		}
	}
}

func TestNonGitDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := discover(dir)
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("error = %v, want directory", err)
	}
}

func TestInheritedTags(t *testing.T) {
	for _, tc := range []struct {
		flags string
		bad   bool
		want  string
	}{
		{"-tags=alpha", true, "alpha"},
		{"--tags=alpha,beta", true, "alpha,beta"},
		{"-p=2 -tags=alpha", true, "alpha"},
		{`"-tags=alpha"`, true, "alpha"},
		{"'-tags=alpha'", true, "alpha"},
		{"-p=2 '--tags=alpha'", true, "alpha"},
		{`"-tags=alpha`, true, ""},
		{"-tagsx=1", true, ""},
		{"", false, ""},
		{"-p=2", false, ""},
		{"-mod=readonly", false, ""},
		{`"-mod=readonly"`, false, ""},
		{"'-p=2'", false, ""},
		// An empty tag list applies no tag.
		{"-tags=", false, ""},
	} {
		t.Run(tc.flags, func(t *testing.T) {
			t.Setenv("GOFLAGS", tc.flags)
			err := checkInheritedTags()
			if tc.bad {
				if err == nil || !strings.Contains(err.Error(), "GOFLAGS") || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("flags %q: %v, want GOFLAGS error containing %q", tc.flags, err, tc.want)
				}
			} else if err != nil {
				t.Fatalf("flags %q: %v", tc.flags, err)
			}
		})
	}
}

func deletedFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := fixture(t, map[string]string{"gone.go": "package a\n"})
	path := filepath.Join(dir, "gone.go")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestDiscoverDeleted(t *testing.T) {
	dir, _ := deletedFixture(t)
	if _, err := discover(dir); err != nil {
		t.Fatalf("discover: %v", err)
	}
}

func TestDiscoverUnreadable(t *testing.T) {
	dir, path := deletedFixture(t)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := discover(dir)
	if err == nil || !strings.Contains(err.Error(), "gone.go") {
		t.Fatalf("discover: %v", err)
	}
}

func TestDiscoverInheritedTags(t *testing.T) {
	dir := fixture(t, map[string]string{"a.go": "package a\n"})
	for _, flags := range []string{"-tags=x", `"-tags=x"`} {
		t.Run(flags, func(t *testing.T) {
			t.Setenv("GOFLAGS", flags)
			_, err := discover(dir)
			if err == nil || !strings.Contains(err.Error(), "GOFLAGS") {
				t.Fatalf("discover: %v, want GOFLAGS error", err)
			}
		})
	}
}

func TestDiscoverExcluded(t *testing.T) {
	bad := "//go:build alpha && !beta\n\npackage p\n"
	good := "//go:build gamma\n\npackage p\n"
	files := map[string]string{"go.mod": "module example.com/fixture\n\ngo 1.27\n\nignore (\n ./build\n x\n)\n", "a/vendor/v.go": good, "keep/gen2/k.go": "//go:build epsilon\n\npackage p\n", "keep/build/k.go": "//go:build zeta\n\npackage p\n", "only/d.go": "//go:build delta\n\npackage p\n", "nested/go.mod": "module example.com/nested\n"}
	for _, name := range []string{"testdata/a.go", ".hidden/a.go", "_under/a.go", "plain/_x.go", "nested/a.go", "a/vendor/b/a.go", "build/a.go", "gen/x/a.go"} {
		files[name] = bad
	}
	dir := fixture(t, files)
	// Git ignores some names by default; force-add every exclusion fixture.
	fixtureGit(t, dir, "add", "-f", "--", ".")
	got, err := discover(dir)
	if err != nil || !reflect.DeepEqual(got, []string{"delta", "epsilon", "gamma", "zeta"}) {
		t.Fatalf("discover = %v, %v", got, err)
	}
}
