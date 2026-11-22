package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The guard protects the location the open reaches and the lexical form the event log writer
// joins, resolved through its own symlinks too, so a symlink, a missing tail below one, a ..
// after one, and a symlinked directory left after the lexical clean are refused in both forms.
func TestLoadResolvesProtectedPathsLikeTheOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sandbox does not run on Windows")
	}
	w := newSandboxPathWorld(t)
	evlink := filepath.Join(w.root, "evlink")
	if err := os.Symlink(filepath.Join("state", "events"), evlink); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", evlink, err)
	}
	state := filepath.Join(w.root, "state")
	logs := filepath.Join(w.root, "logs")
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatalf("Mkdir(%s) = %v, want nil", logs, err)
	}
	ev3 := filepath.Join(w.root, "ev3")
	if err := os.Mkdir(ev3, 0o700); err != nil {
		t.Fatalf("Mkdir(%s) = %v, want nil", ev3, err)
	}
	realdir := filepath.Join(w.root, "realdir")
	ev5 := filepath.Join(realdir, "ev5")
	if err := os.MkdirAll(ev5, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) = %v, want nil", ev5, err)
	}
	mustSymlink(t, "realdir", filepath.Join(w.root, "alias"))
	mustSymlink(t, w.events, filepath.Join(realdir, "evl"))
	roots := []string{state, logs}
	cases := []struct {
		name      string
		roots     []string
		events    string
		workspace string
		want      string
	}{
		{"event log through a symlinked directory", roots, evlink, state, "overlaps protected path " + filepath.Join(state, "events")},
		{"missing event log below a symlinked directory", roots, filepath.Join(evlink, "new"), state, "overlaps protected path " + filepath.Join(state, "events", "new")},
		{"dot-dot after a symlink climbs from the target", roots, w.root + "/evlink/../ev2", state, "overlaps protected path " + filepath.Join(state, "ev2")},
		{"lexical dot-dot location is protected too", []string{state, logs, ev3}, w.root + "/evlink/../ev3", ev3, "overlaps protected path " + filepath.Join(w.root, "ev3")},
		{"lexical location through a symlinked directory is resolved", []string{state, logs, ev5}, w.root + "/alias/evl/../ev5", ev5, "overlaps protected path " + ev5},
		{"plain event log next to the workspace", roots, filepath.Join(logs, "events"), state, ""},
	}
	for _, tc := range cases {
		checkLoadWant(t, tc.name, w.loadWithEventLog(t, tc.roots, tc.events, tc.workspace, []string{}), tc.want)
	}
}

func checkLoadWant(t *testing.T, name string, err error, want string) {
	t.Helper()
	if want == "" && err != nil {
		t.Fatalf("%s: Load() = %v, want nil", name, err)
	}
	if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
		t.Fatalf("%s: Load() = %v, want error containing %q", name, err, want)
	}
}
