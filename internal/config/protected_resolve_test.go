package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The guard resolves each protected path the way the open does, so a symlink in it, a
// missing tail below a symlink, and a .. after a symlink all name the physical location.
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
		{"lexical dot-dot location is not protected", []string{state, logs, ev3}, w.root + "/evlink/../ev3", ev3, ""},
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
