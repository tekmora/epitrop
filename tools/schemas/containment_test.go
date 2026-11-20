//go:build schemasrefresh

// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareOneContainment(t *testing.T) {
	base := t.TempDir()
	root, tmp := filepath.Join(base, "root"), filepath.Join(base, "tmp")
	for _, dir := range []string{root, tmp} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "local"), []byte("same"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(base, "outside")
	if err := os.WriteFile(outside, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := compareOne(root, tmp, "local"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../outside", outside, ".", ""} {
		err := compareOne(root, tmp, rel)
		if err == nil || !strings.Contains(err.Error(), rel) {
			t.Errorf("compareOne(%q) = %v, want named error", rel, err)
		}
	}
}

func TestWriteFragmentsContainment(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../outside", filepath.Join(base, "absolute"), ".", ""} {
		err := writeFragments(root, schemaSource{ID: "test"}, map[string][]byte{rel: []byte("bad")})
		if err == nil || !strings.Contains(err.Error(), rel) {
			t.Errorf("writeFragments(%q) = %v, want named error", rel, err)
		}
	}
}

func TestFragmentPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		valid bool
	}{
		{"nested", "a/b.yaml", true},
		{"within root", "a/../b", true},
		{"parent", "../x", false},
		{"absolute", filepath.Join(t.TempDir(), "x"), false},
		{"empty", "", false},
		{"dot", ".", false},
		{"clean dot", "a/..", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fragmentPath(tc.input)
			if tc.valid {
				if err != nil || got != filepath.Clean(filepath.FromSlash(tc.input)) {
					t.Errorf("fragmentPath(%q) = %q, %v; want %q, nil", tc.input, got, err, filepath.Clean(filepath.FromSlash(tc.input)))
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.input) {
				t.Errorf("fragmentPath(%q) = %q, %v; want named error", tc.input, got, err)
			}
		})
	}
}

func TestCompareOneContainmentSymlink(t *testing.T) {
	base := t.TempDir()
	root, tmp := filepath.Join(base, "root"), filepath.Join(base, "tmp")
	for _, dir := range []string{root, tmp} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(base, "outside")
	for _, path := range []string{outside, filepath.Join(tmp, "link")} {
		if err := os.WriteFile(path, []byte("same"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	err := compareOne(root, tmp, "link")
	if err == nil || !strings.Contains(err.Error(), "link") {
		t.Errorf("compareOne(link) = %v, want named error", err)
	}
}

func TestWriteFragmentsContainmentSymlink(t *testing.T) {
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outdir")
	for _, dir := range []string{root, outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	err := writeFragments(root, schemaSource{ID: "test"}, map[string][]byte{"linkdir/x": []byte("bad")})
	if err == nil || !strings.Contains(err.Error(), "linkdir/x") {
		t.Errorf("writeFragments(linkdir/x) = %v, want named error", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
		t.Errorf("outside x stat = %v, want not exist", err)
	}
}

func TestWriteFragmentsCleanPath(t *testing.T) {
	root := t.TempDir()
	if err := writeFragments(root, schemaSource{ID: "test"}, map[string][]byte{"a/../b.yaml": []byte("ok")}); err != nil {
		t.Fatal(err)
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := dir.Close(); err != nil {
			t.Errorf("close root: %v", err)
		}
	}()
	body, err := dir.ReadFile("b.yaml")
	if err != nil || string(body) != "ok" {
		t.Fatalf("read = %q, %v", body, err)
	}
}
