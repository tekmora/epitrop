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
