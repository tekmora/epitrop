package main

import (
	"context"
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

func TestDiscover(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		want      []string
		errorText string
	}{
		{"none", map[string]string{"a.go": "package a\n"}, []string{}, ""},
		{"tagged packages", map[string]string{"a.go": "//go:build alpha\n\npackage a\n", "nested/b.go": "//go:build beta\n\npackage b\n"}, []string{"alpha", "beta"}, ""},
		{"toolchain", map[string]string{"a.go": "//go:build linux || windows || amd64 || unix || cgo || gc || gccgo || go1.21 || goexperiment.x\n\npackage a\n"}, []string{}, ""},
		{"negation", map[string]string{"a.go": "//go:build !alpha\n\npackage a\n"}, []string{"alpha"}, ""},
		{"disjunction", map[string]string{"a.go": "//go:build alpha || beta\n\npackage a\n"}, []string{"alpha", "beta"}, ""},
		{"gap", map[string]string{"gap.go": "//go:build alpha && !beta\n\npackage a\n"}, nil, "gap.go"},
		{"bound", map[string]string{"bound.go": "//go:build a || b || c || d || e || f || g || h || i\n\npackage a\n"}, nil, "bound.go"},
		{"os excluded", map[string]string{"x_windows.go": "//go:build alpha && !beta\n\npackage a\n"}, []string{"alpha", "beta"}, ""},
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

func TestNonGitDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := discover(dir)
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("error = %v, want directory", err)
	}
}
