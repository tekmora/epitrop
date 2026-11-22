package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type rotatedNameCase struct {
	firstPath  string
	secondPath string
	rotIdx     int
	ownerIdx   int
}

func TestLoadJobLogRotatedNames(t *testing.T) {
	t.Run("rotated_name_refused", func(t *testing.T) {
		assertLiteralRotatedNamesRefused(t)
	})
	t.Run("unrelated_names_load", func(t *testing.T) {
		assertUnrelatedRotatedNamesLoad(t)
	})
	t.Run("symlinked_directory_alias_refused", func(t *testing.T) {
		assertSymlinkedDirRotatedNameRefused(t)
	})
	t.Run("link_into_rotated_names_refused", func(t *testing.T) {
		assertLinkIntoRotatedNamesRefused(t)
	})
	t.Run("symlinked_active_log_refused", func(t *testing.T) {
		assertSymlinkedActiveLogRefused(t)
	})
	t.Run("link_to_own_rotated_name_refused", func(t *testing.T) {
		assertLinkToOwnRotatedNameRefused(t)
	})
	t.Run("symlinked_rotated_name_refused", func(t *testing.T) {
		assertSymlinkedRotatedNameRefused(t)
	})
	t.Run("owner_through_directory_alias_refused", func(t *testing.T) {
		assertOwnerThroughDirAliasRefused(t)
	})
}

func assertLiteralRotatedNamesRefused(t *testing.T) {
	t.Helper()
	cases := []rotatedNameCase{
		{firstPath: "/var/log/a.log", secondPath: "/var/log/a.log.1", rotIdx: 1, ownerIdx: 0},
		{firstPath: "/var/log/a.log.1", secondPath: "/var/log/a.log", rotIdx: 0, ownerIdx: 1},
		{firstPath: "/var/log/a.log", secondPath: "/var/log/a.log.101", rotIdx: 1, ownerIdx: 0},
		{firstPath: "/var/log/a.log", secondPath: "/var/log/x/../a.log.5", rotIdx: 1, ownerIdx: 0},
	}
	for i := 0; i < len(cases); i++ {
		tc := cases[i]
		assertRotatedJobLogRefused(t, tc.firstPath, tc.secondPath, tc.rotIdx, tc.ownerIdx)
	}
}

func assertUnrelatedRotatedNamesLoad(t *testing.T) {
	t.Helper()
	pairs := [][2]string{
		{"/var/log/a.log", "/var/log/a.log.102"},
		{"/var/log/a.log", "/var/log/a.log.01"},
		{"/var/log/a.log", "/var/log/a.log.0"},
		{"/var/log/a.log", "/var/log/a.log.+1"},
		{"/var/log/a.log", "/var/log/a.log.1.1"},
		{"/var/log/a.log", "/var/log/a.log1"},
		{"/var/log/a.log", "/var/log/b.log.1"},
	}
	for i := 0; i < len(pairs); i++ {
		body := twoJobsLogPathConfig(pairs[i][0], pairs[i][1])
		if _, err := Load(testContext(t), writeConfig(t, body)); err != nil {
			t.Fatalf("Load(%q vs %q) = %v, want nil", pairs[i][0], pairs[i][1], err)
		}
	}
}

func assertSymlinkedDirRotatedNameRefused(t *testing.T) {
	t.Helper()
	root := newResolvedTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("Mkdir(%s) = %v, want nil", realDir, err)
	}
	aliasDir := filepath.Join(root, "alias")
	if err := os.Symlink("real", aliasDir); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", aliasDir, err)
	}
	assertRotatedJobLogRefused(t, filepath.Join(realDir, "a.log"), filepath.Join(aliasDir, "a.log.1"), 1, 0)
}

func assertLinkIntoRotatedNamesRefused(t *testing.T) {
	t.Helper()
	root := newResolvedTempDir(t)
	firstPath := filepath.Join(root, "a.log")
	secondPath := filepath.Join(root, "b.log")
	if err := os.Symlink("a.log.3", secondPath); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", secondPath, err)
	}
	assertRotatedJobLogRefused(t, firstPath, secondPath, 1, 0)
}

func assertSymlinkedActiveLogRefused(t *testing.T) {
	t.Helper()
	root := newResolvedTempDir(t)
	firstPath := filepath.Join(root, "a.log")
	if err := os.Symlink("t.log", firstPath); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", firstPath, err)
	}
	assertRotatedJobLogRefused(t, firstPath, filepath.Join(root, "a.log.1"), 1, 0)
}

func assertLinkToOwnRotatedNameRefused(t *testing.T) {
	t.Helper()
	root := newResolvedTempDir(t)
	firstPath := filepath.Join(root, "a.log")
	if err := os.Symlink("a.log.1", firstPath); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", firstPath, err)
	}
	assertRotatedJobLogRefused(t, firstPath, filepath.Join(root, "z.log"), 0, 0)
}

func assertSymlinkedRotatedNameRefused(t *testing.T) {
	t.Helper()
	root := newResolvedTempDir(t)
	// The rename of a.log replaces the link a.log.1, so the entry owns the name, not its target.
	secondPath := filepath.Join(root, "a.log.1")
	if err := os.Symlink("x.log", secondPath); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", secondPath, err)
	}
	assertRotatedJobLogRefused(t, filepath.Join(root, "a.log"), secondPath, 1, 0)
}

func assertOwnerThroughDirAliasRefused(t *testing.T) {
	t.Helper()
	root := newResolvedTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("Mkdir(%s) = %v, want nil", realDir, err)
	}
	aliasDir := filepath.Join(root, "alias")
	if err := os.Symlink("real", aliasDir); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", aliasDir, err)
	}
	assertRotatedJobLogRefused(t, filepath.Join(aliasDir, "a.log"), filepath.Join(realDir, "a.log.1"), 1, 0)
}

func assertRotatedJobLogRefused(t *testing.T, firstPath, secondPath string, rotIdx, ownerIdx int) {
	t.Helper()
	paths := [2]string{firstPath, secondPath}
	_, err := Load(testContext(t), writeConfig(t, twoJobsLogPathConfig(firstPath, secondPath)))
	want := fmt.Sprintf("config: jobs/%d/log_path: %q is a rotated name of jobs/%d (%q)", rotIdx, paths[rotIdx], ownerIdx, paths[ownerIdx])
	if err == nil || err.Error() != want {
		t.Fatalf("Load(%q vs %q) = %v, want %q", firstPath, secondPath, err, want)
	}
}
