package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadJobLogPathThroughRotatedName(t *testing.T) {
	t.Run("chained_link_through_own_rotated_name", assertChainedLinkThroughOwnRotatedName)
	t.Run("chained_link_existing_target_through_own_rotated_name", assertChainedLinkExistingTargetThroughOwnRotatedName)
	t.Run("chained_link_through_other_rotated_name", assertChainedLinkThroughOtherRotatedName)
	t.Run("existing_directory_named_as_rotated_name", assertExistingDirNamedAsRotatedName)
	t.Run("symlinked_directory_named_as_rotated_name", assertSymlinkedDirNamedAsRotatedName)
	t.Run("missing_directory_named_as_rotated_name", assertMissingDirNamedAsRotatedName)
	t.Run("chain_and_directories_outside_rotated_names_load", assertChainAndDirsOutsideRotatedNamesLoad)
}

func assertChainedLinkThroughOwnRotatedName(t *testing.T) {
	root := newResolvedTempDir(t)
	first := filepath.Join(root, "a.log")
	mid := filepath.Join(root, "a.log.1")
	mustSymlink(t, "a.log.1", first)
	mustSymlink(t, "target.log", mid)
	second := filepath.Join(root, "z.log")
	want := fmt.Sprintf("config: jobs/0/log_path: %q passes through %q, a rotated name of jobs/0 (%q)", first, mid, first)
	assertJobLogViaRefused(t, first, second, want)
}

func assertChainedLinkExistingTargetThroughOwnRotatedName(t *testing.T) {
	root := newResolvedTempDir(t)
	target := filepath.Join(root, "target.log")
	if err := os.WriteFile(target, []byte("log\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) = %v, want nil", target, err)
	}
	first := filepath.Join(root, "a.log")
	mid := filepath.Join(root, "a.log.1")
	mustSymlink(t, "a.log.1", first)
	mustSymlink(t, "target.log", mid)
	second := filepath.Join(root, "z.log")
	want := fmt.Sprintf("config: jobs/0/log_path: %q passes through %q, a rotated name of jobs/0 (%q)", first, mid, first)
	assertJobLogViaRefused(t, first, second, want)
}

func assertChainedLinkThroughOtherRotatedName(t *testing.T) {
	root := newResolvedTempDir(t)
	first := filepath.Join(root, "a.log")
	second := filepath.Join(root, "c.log")
	mid := filepath.Join(root, "a.log.2")
	mustSymlink(t, "a.log.2", second)
	mustSymlink(t, "t.log", mid)
	want := fmt.Sprintf("config: jobs/1/log_path: %q passes through %q, a rotated name of jobs/0 (%q)", second, mid, first)
	assertJobLogViaRefused(t, first, second, want)
}

func assertExistingDirNamedAsRotatedName(t *testing.T) {
	root := newResolvedTempDir(t)
	rotDir := filepath.Join(root, "b.log.1")
	if err := os.Mkdir(rotDir, 0o700); err != nil {
		t.Fatalf("Mkdir(%s) = %v, want nil", rotDir, err)
	}
	first := filepath.Join(root, "b.log")
	second := filepath.Join(rotDir, "a.log")
	want := fmt.Sprintf("config: jobs/1/log_path: %q passes through %q, a rotated name of jobs/0 (%q)", second, rotDir, first)
	assertJobLogViaRefused(t, first, second, want)
}

func assertSymlinkedDirNamedAsRotatedName(t *testing.T) {
	root := newResolvedTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("Mkdir(%s) = %v, want nil", realDir, err)
	}
	rotDir := filepath.Join(root, "b.log.1")
	mustSymlink(t, "real", rotDir)
	first := filepath.Join(root, "b.log")
	second := filepath.Join(rotDir, "a.log")
	want := fmt.Sprintf("config: jobs/1/log_path: %q passes through %q, a rotated name of jobs/0 (%q)", second, rotDir, first)
	assertJobLogViaRefused(t, first, second, want)
}

func assertMissingDirNamedAsRotatedName(t *testing.T) {
	root := newResolvedTempDir(t)
	rotDir := filepath.Join(root, "b.log.3")
	first := filepath.Join(root, "b.log")
	second := filepath.Join(rotDir, "a.log")
	want := fmt.Sprintf("config: jobs/1/log_path: %q passes through %q, a rotated name of jobs/0 (%q)", second, rotDir, first)
	assertJobLogViaRefused(t, first, second, want)
}

func assertChainAndDirsOutsideRotatedNamesLoad(t *testing.T) {
	root := newResolvedTempDir(t)
	first := filepath.Join(root, "a.log")
	mid := filepath.Join(root, "mid.log")
	mustSymlink(t, "mid.log", first)
	mustSymlink(t, "t.log", mid)
	dir := filepath.Join(root, "b.log.d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir(%s) = %v, want nil", dir, err)
	}
	second := filepath.Join(dir, "x.log")
	if _, err := Load(testContext(t), writeConfig(t, twoJobsLogPathConfig(first, second))); err != nil {
		t.Fatalf("Load(%q vs %q) = %v, want nil", first, second, err)
	}
}

func assertJobLogViaRefused(t *testing.T, first, second, want string) {
	t.Helper()
	_, err := Load(testContext(t), writeConfig(t, twoJobsLogPathConfig(first, second)))
	if err == nil || err.Error() != want {
		t.Fatalf("Load(%q vs %q) = %v, want %q", first, second, err, want)
	}
}

// mustSymlink creates link -> target; callers start from newResolvedTempDir,
// which skips on Windows.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink(%s -> %s) = %v, want nil", link, target, err)
	}
}
