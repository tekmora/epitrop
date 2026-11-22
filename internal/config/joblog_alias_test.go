package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadJobLogPathAliases(t *testing.T) {
	t.Run("hard_link_refused", assertHardLinkJobLogRefused)
	t.Run("case_only_names_missing_refused", assertCaseOnlyNamesMissingRefused)
	t.Run("case_only_name_one_existing_refused", assertCaseOnlyNameOneExistingRefused)
	t.Run("case_only_missing_directories_refused", assertCaseOnlyMissingDirectoriesRefused)
	t.Run("case_alias_of_rotated_name_refused", assertCaseAliasOfRotatedNameRefused)
	t.Run("case_alias_through_rotated_directory_refused", assertCaseAliasThroughRotatedDirectoryRefused)
	t.Run("hard_link_to_rotated_file_refused", assertHardLinkToRotatedFileRefused)
	t.Run("distinct_names_load", assertDistinctAliasNamesLoad)
	t.Run("case_name_in_other_directory_loads", assertCaseNameInOtherDirectoryLoads)
	t.Run("hard_link_to_unrelated_file_loads", assertHardLinkToUnrelatedFileLoads)
	t.Run("hard_link_to_highest_rotated_index_refused", assertHardLinkToHighestRotatedIndexRefused)
	t.Run("hard_link_past_highest_rotated_index_loads", assertHardLinkPastHighestRotatedIndexLoads)
	t.Run("case_alias_of_highest_rotated_index_refused", assertCaseAliasOfHighestRotatedIndexRefused)
	t.Run("case_alias_of_rotated_index_zero_loads", assertCaseAliasOfRotatedIndexZeroLoads)
	t.Run("unicode_simple_fold_refused", assertUnicodeSimpleFoldRefused)
	t.Run("distinct_case_directories_load", assertDistinctCaseDirectoriesLoad)
}

func assertHardLinkJobLogRefused(t *testing.T) {
	root := newAliasTempDir(t)
	first := filepath.Join(root, "a.log")
	second := filepath.Join(root, "b.log")
	mustWriteAliasLogFile(t, first)
	mustHardLink(t, first, second)
	assertSameJobLogFileRefused(t, first, second)
}

func assertCaseOnlyNamesMissingRefused(t *testing.T) {
	root := newAliasTempDir(t)
	assertSameJobLogFileRefused(t, filepath.Join(root, "A.log"), filepath.Join(root, "a.log"))
}

func assertCaseOnlyNameOneExistingRefused(t *testing.T) {
	root := newAliasTempDir(t)
	first := filepath.Join(root, "A.log")
	mustWriteAliasLogFile(t, first)
	assertSameJobLogFileRefused(t, first, filepath.Join(root, "a.log"))
}

func assertCaseOnlyMissingDirectoriesRefused(t *testing.T) {
	root := newAliasTempDir(t)
	assertSameJobLogFileRefused(t, filepath.Join(root, "New", "a.log"), filepath.Join(root, "new", "a.log"))
}

func assertCaseAliasOfRotatedNameRefused(t *testing.T) {
	root := newAliasTempDir(t)
	assertRotatedJobLogRefused(t, filepath.Join(root, "A.log"), filepath.Join(root, "a.log.1"), 1, 0)
}

func assertCaseAliasThroughRotatedDirectoryRefused(t *testing.T) {
	root := newAliasTempDir(t)
	first := filepath.Join(root, "a.log")
	rotDir := filepath.Join(root, "A.LOG.1")
	second := filepath.Join(rotDir, "x.log")
	want := fmt.Sprintf("config: jobs/1/log_path: %q passes through %q, a rotated name of jobs/0 (%q)", second, rotDir, first)
	assertJobLogViaRefused(t, first, second, want)
}

func assertHardLinkToRotatedFileRefused(t *testing.T) {
	assertHardLinkToRotatedIndexRefused(t, "a.log.1")
}

func assertDistinctAliasNamesLoad(t *testing.T) {
	root := newAliasTempDir(t)
	first := filepath.Join(root, "a.log")
	second := filepath.Join(root, "b.log")
	mustWriteAliasLogFile(t, first)
	mustWriteAliasLogFile(t, second)
	assertJobLogPairLoads(t, first, second)
}

func assertCaseNameInOtherDirectoryLoads(t *testing.T) {
	root := newAliasTempDir(t)
	oneDir := filepath.Join(root, "one")
	twoDir := filepath.Join(root, "two")
	mustMkdirAlias(t, oneDir)
	mustMkdirAlias(t, twoDir)
	assertJobLogPairLoads(t, filepath.Join(oneDir, "a.log"), filepath.Join(twoDir, "A.log"))
}

func assertHardLinkToUnrelatedFileLoads(t *testing.T) {
	root := newAliasTempDir(t)
	other := filepath.Join(root, "other.txt")
	first := filepath.Join(root, "a.log")
	second := filepath.Join(root, "b.log")
	mustWriteAliasLogFile(t, other)
	mustHardLink(t, other, second)
	assertJobLogPairLoads(t, first, second)
}

func assertHardLinkToHighestRotatedIndexRefused(t *testing.T) {
	assertHardLinkToRotatedIndexRefused(t, "a.log.101")
}

func assertHardLinkPastHighestRotatedIndexLoads(t *testing.T) {
	root := newAliasTempDir(t)
	first := filepath.Join(root, "a.log")
	past := filepath.Join(root, "a.log.102")
	second := filepath.Join(root, "b.log")
	mustWriteAliasLogFile(t, past)
	mustHardLink(t, past, second)
	assertJobLogPairLoads(t, first, second)
}

func assertCaseAliasOfHighestRotatedIndexRefused(t *testing.T) {
	root := newAliasTempDir(t)
	assertRotatedJobLogRefused(t, filepath.Join(root, "A.log"), filepath.Join(root, "a.log.101"), 1, 0)
}

func assertCaseAliasOfRotatedIndexZeroLoads(t *testing.T) {
	root := newAliasTempDir(t)
	assertJobLogPairLoads(t, filepath.Join(root, "A.log"), filepath.Join(root, "a.log.0"))
}

func assertUnicodeSimpleFoldRefused(t *testing.T) {
	root := newAliasTempDir(t)
	assertSameJobLogFileRefused(t, filepath.Join(root, "Ä.log"), filepath.Join(root, "ä.log"))
}

func assertDistinctCaseDirectoriesLoad(t *testing.T) {
	root := newAliasTempDir(t)
	upperDir := filepath.Join(root, "Logs")
	mustMkdirAlias(t, upperDir)
	lowerDir := filepath.Join(root, "logs")
	if err := os.Mkdir(lowerDir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			t.Skipf("temporary directory volume folds directory case: %v", err)
		}
		t.Fatalf("Mkdir(%s) = %v, want nil", lowerDir, err)
	}
	first := filepath.Join(upperDir, "a.log")
	second := filepath.Join(lowerDir, "a.log")
	mustWriteAliasLogFile(t, first)
	mustWriteAliasLogFile(t, second)
	assertJobLogPairLoads(t, first, second)
}

func assertHardLinkToRotatedIndexRefused(t *testing.T, rotatedName string) {
	t.Helper()
	root := newAliasTempDir(t)
	first := filepath.Join(root, "a.log")
	rotated := filepath.Join(root, rotatedName)
	second := filepath.Join(root, "b.log")
	mustWriteAliasLogFile(t, rotated)
	mustHardLink(t, rotated, second)
	want := fmt.Sprintf("config: jobs/1/log_path: %q names the same file as %q, a rotated name of jobs/0 (%q)", second, rotated, first)
	_, err := Load(testContext(t), writeConfig(t, twoJobsLogPathConfig(first, second)))
	if err == nil || err.Error() != want {
		t.Fatalf("Load(%q vs %q) = %v, want %q", first, second, err, want)
	}
}

func assertJobLogPairLoads(t *testing.T, firstPath, secondPath string) {
	t.Helper()
	if _, err := Load(testContext(t), writeConfig(t, twoJobsLogPathConfig(firstPath, secondPath))); err != nil {
		t.Fatalf("Load(%q vs %q) = %v, want nil", firstPath, secondPath, err)
	}
}

func newAliasTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temporary directory) = %v, want nil", err)
	}
	return root
}

func mustWriteAliasLogFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("log\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) = %v, want nil", path, err)
	}
}

func mustMkdirAlias(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir(%s) = %v, want nil", dir, err)
	}
}

func mustHardLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Link(target, link); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skipf("hard links unsupported: %v", err)
		}
		t.Fatalf("Link(%s -> %s) = %v, want nil", link, target, err)
	}
}
