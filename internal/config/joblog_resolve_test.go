package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type unresolvedAbsCase struct {
	name string
	in   string
	want string
}

func TestUnresolvedAbs(t *testing.T) {
	wd, wdErr := os.Getwd()
	if wdErr != nil {
		t.Fatalf("Getwd() = %v, want nil", wdErr)
	}
	cases := unresolvedAbsCases(wd)
	for i := 0; i < len(cases); i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			got, err := unresolvedAbs(tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("unresolvedAbs(%q) = %q, %v, want %q, nil", tc.in, got, err, tc.want)
			}
		})
	}
}

func unresolvedAbsCases(wd string) []unresolvedAbsCase {
	if runtime.GOOS == "windows" {
		absWithDotDot := strings.Join([]string{wd, "..", "a.log"}, "\\")
		return []unresolvedAbsCase{
			{"absolute_cleaned_on_windows", absWithDotDot, filepath.Join(filepath.Dir(wd), "a.log")},
			{"relative_cleaned_on_windows", "x\\..\\a.log", filepath.Join(wd, "a.log")},
			{"drive_root_relative_on_windows", "\\a.log", filepath.VolumeName(wd) + "\\a.log"},
		}
	}
	sep := string(filepath.Separator)
	absWithDotDot := strings.Join([]string{wd, "..", "a.log"}, sep)
	return []unresolvedAbsCase{
		{"absolute_preserves_dot_dot", absWithDotDot, absWithDotDot},
		{"relative_prepends_getwd_uncleaned", "x/../a.log", wd + sep + "x/../a.log"},
	}
}

type spliceLogPathTargetCase struct {
	name         string
	resolved     string
	target       string
	rest         []string
	wantResolved string
	wantPending  []string
}

func TestSpliceLogPathTarget(t *testing.T) {
	cases := spliceLogPathTargetCases()
	for i := 0; i < len(cases); i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			gotResolved, gotPending := spliceLogPathTarget(tc.resolved, tc.target, tc.rest)
			if gotResolved != tc.wantResolved || !slices.Equal(gotPending, tc.wantPending) {
				t.Fatalf("spliceLogPathTarget(%q, %q, %q) = (%q, %q), want (%q, %q)", tc.resolved, tc.target, tc.rest, gotResolved, gotPending, tc.wantResolved, tc.wantPending)
			}
		})
	}
}

func spliceLogPathTargetCases() []spliceLogPathTargetCase {
	rest := []string{"z"}
	if runtime.GOOS == "windows" {
		return []spliceLogPathTargetCase{
			{"absolute_target_on_other_drive", `C:\r\d`, `D:\x\a.log`, rest, `D:\`, []string{"x", "a.log", "z"}},
			{"root_relative_target_on_windows", `C:\r\d`, `\x\a.log`, rest, `C:\`, []string{"x", "a.log", "z"}},
			{"unc_target_on_windows", `C:\r\d`, `\\srv\share\a.log`, rest, `\\srv\share\`, []string{"a.log", "z"}},
			{"relative_target_on_windows", `C:\r\d`, `x\a.log`, rest, `C:\r\d`, []string{"x", "a.log", "z"}},
		}
	}
	return []spliceLogPathTargetCase{
		{"absolute_target", "/r/d", "/x/a.log", rest, "/", []string{"x", "a.log", "z"}},
		{"relative_target", "/r/d", "x/a.log", rest, "/r/d", []string{"x", "a.log", "z"}},
		{"dot_dot_target", "/r/d", "../y/a.log", rest, "/r/d", []string{"..", "y", "a.log", "z"}},
	}
}

func TestResolveExistingAncestor(t *testing.T) {
	cases := []struct {
		name  string
		paths func(*testing.T) (string, string)
	}{
		{"two_missing_components_below_alias", ancestorTwoMissingBelowAlias},
		{"existing_file_through_link", ancestorExistingFileThroughLink},
		{"missing_file_in_real_dir", ancestorMissingFileInRealDir},
		{"dot_dot_below_missing_component_cleaned", ancestorDotDotBelowMissing},
		{"dot_dot_after_symlink_climbs_from_target", ancestorDotDotAfterSymlinkClimbs},
		{"dot_dot_after_symlink_below_missing", ancestorDotDotAfterSymlinkBelowMissing},
		{"unreadable_directory_through_alias", ancestorUnreadableDirThroughAlias},
		{"dangling_file_link_follows_to_target", ancestorDanglingFileLink},
		{"dangling_link_absolute_target_below_missing", ancestorDanglingLinkAbsTargetBelowMissing},
		{"dangling_link_chain", ancestorDanglingLinkChain},
		{"dangling_link_chain_at_hop_cap", ancestorDanglingLinkChainAtHopCap},
		{"dangling_link_chain_past_hop_cap", ancestorDanglingLinkChainPastHopCap},
		{"link_cycle_stops_at_hop_cap", ancestorLinkCycleStopsAtHopCap},
	}
	for i := 0; i < len(cases); i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			in, want := tc.paths(t)
			if got := resolveExistingAncestor(in); got != want {
				t.Fatalf("resolveExistingAncestor(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func ancestorTwoMissingBelowAlias(t *testing.T) (string, string) {
	t.Helper()
	_, realDir, aliasDir := newJobLogAliasDirs(t)
	return filepath.Join(aliasDir, "new", "sub", "a.log"), filepath.Join(realDir, "new", "sub", "a.log")
}

func ancestorExistingFileThroughLink(t *testing.T) (string, string) {
	t.Helper()
	realLog, linkLog := newLinkedJobLogFile(t)
	return linkLog, realLog
}

func ancestorMissingFileInRealDir(t *testing.T) (string, string) {
	t.Helper()
	_, realDir, _ := newJobLogAliasDirs(t)
	missingLog := filepath.Join(realDir, "a.log")
	return missingLog, missingLog
}

func ancestorDotDotBelowMissing(t *testing.T) (string, string) {
	t.Helper()
	_, realDir, aliasDir := newJobLogAliasDirs(t)
	uncleaned := strings.Join([]string{aliasDir, "x", "..", "new", "a.log"}, string(filepath.Separator))
	return uncleaned, filepath.Join(realDir, "new", "a.log")
}

func ancestorDotDotAfterSymlinkClimbs(t *testing.T) (string, string) {
	t.Helper()
	outside, alias := newNestedJobLogAlias(t)
	uncleaned := strings.Join([]string{alias, "..", "a.log"}, string(filepath.Separator))
	return uncleaned, filepath.Join(outside, "a.log")
}

func ancestorDotDotAfterSymlinkBelowMissing(t *testing.T) (string, string) {
	t.Helper()
	outside, alias := newNestedJobLogAlias(t)
	uncleaned := strings.Join([]string{alias, "..", "new", "sub", "a.log"}, string(filepath.Separator))
	return uncleaned, filepath.Join(outside, "new", "sub", "a.log")
}

func ancestorUnreadableDirThroughAlias(t *testing.T) (string, string) {
	t.Helper()
	locked, lockAlias := newLockedJobLogAliasDirs(t)
	return filepath.Join(lockAlias, "sub", "a.log"), filepath.Join(locked, "sub", "a.log")
}

func ancestorDanglingFileLink(t *testing.T) (string, string) {
	t.Helper()
	return newDanglingJobLogLink(t)
}

func ancestorDanglingLinkAbsTargetBelowMissing(t *testing.T) (string, string) {
	t.Helper()
	root := newResolvedTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) = %v, want nil", realDir, err)
	}
	target := filepath.Join(realDir, "new", "a.log")
	linkLog := filepath.Join(root, "link.log")
	if err := os.Symlink(target, linkLog); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", linkLog, err)
	}
	return linkLog, target
}

func ancestorDanglingLinkChain(t *testing.T) (string, string) {
	t.Helper()
	root := newResolvedTempDir(t)
	firstLink := filepath.Join(root, "l1")
	secondLink := filepath.Join(root, "l2")
	if err := os.Symlink("l2", firstLink); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", firstLink, err)
	}
	if err := os.Symlink("real.log", secondLink); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", secondLink, err)
	}
	return firstLink, filepath.Join(root, "real.log")
}

func ancestorDanglingLinkChainAtHopCap(t *testing.T) (string, string) {
	t.Helper()
	root := newResolvedTempDir(t)
	buildDanglingLinkChain(t, root, 40)
	// On Linux, O_CREAT open through a 40-symlink chain succeeds and creates real.log.
	return filepath.Join(root, "l1"), filepath.Join(root, "real.log")
}

func ancestorDanglingLinkChainPastHopCap(t *testing.T) (string, string) {
	t.Helper()
	root := newResolvedTempDir(t)
	buildDanglingLinkChain(t, root, 41)
	// On Linux, O_CREAT open through a 41-symlink chain fails with ELOOP.
	return filepath.Join(root, "l1"), filepath.Join(root, "l41")
}

func buildDanglingLinkChain(t *testing.T, root string, linkCount int) {
	t.Helper()
	for i := 1; i <= linkCount; i++ {
		target := fmt.Sprintf("l%d", i+1)
		if i == linkCount {
			target = "real.log"
		}
		linkPath := filepath.Join(root, fmt.Sprintf("l%d", i))
		if err := os.Symlink(target, linkPath); err != nil {
			t.Fatalf("Symlink(%s -> %s) = %v, want nil", linkPath, target, err)
		}
	}
}

func ancestorLinkCycleStopsAtHopCap(t *testing.T) (string, string) {
	t.Helper()
	root := newResolvedTempDir(t)
	linkA := filepath.Join(root, "a")
	linkB := filepath.Join(root, "b")
	if err := os.Symlink("b", linkA); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", linkA, err)
	}
	if err := os.Symlink("a", linkB); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", linkB, err)
	}
	// Starting at root/a (0 hops), odd hops splice "b" and even hops splice "a";
	// maxLogPathLinkHops = 40 completes 20 a->b->a pairs, so hop 40 leaves "a"
	// pending and the cap stops before hop 41 at root/a.
	return linkA, filepath.Join(root, "a")
}
