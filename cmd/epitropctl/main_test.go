package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestHelpOutputFramesEpitropAsGraphRouter(t *testing.T) {
	bin := buildCLI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, bin, "--help").CombinedOutput() // #nosec G204 -- bin is the CLI built by the test
	if err != nil {
		t.Fatalf("epitropctl --help: %v\n%s", err, output)
	}
	if want := []byte("epitropctl - model data sync for the Epitrop graph router\n"); !bytes.HasPrefix(output, want) {
		t.Fatalf("epitropctl --help = %q, want banner %q", output, want)
	}
	if old := []byte("Epitrop model catalog"); bytes.Contains(output, old) {
		t.Fatalf("epitropctl --help retains old framing %q: %q", old, output)
	}
	if !bytes.Contains(output, []byte("\nUsage:\n")) {
		t.Fatalf("epitropctl --help lost usage section: %q", output)
	}
	if !bytes.Contains(output, []byte("epitropctl schema")) {
		t.Fatalf("epitropctl --help missing schema command: %q", output)
	}
}

func TestSchemaCommandPrintsSnapshotSchema(t *testing.T) {
	bin := buildCLI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, bin, "schema").CombinedOutput() // #nosec G204 -- bin is the CLI built by the test
	if err != nil {
		t.Fatalf("epitropctl schema: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte(`"schema_version"`)) {
		t.Fatalf("epitropctl schema output missing schema_version: %s", output)
	}
	if !bytes.Contains(output, []byte(`"https://github.com/tekmora/epitrop/catalog/snapshot.schema.json"`)) {
		t.Fatalf("epitropctl schema output missing $id: %s", output)
	}
}

func TestVersionOutputUsesInjectedVersion(t *testing.T) {
	bin := buildCLI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, bin, "version").CombinedOutput() // #nosec G204 -- bin is the CLI built by the test
	if err != nil {
		t.Fatalf("epitropctl version: %v\n%s", err, output)
	}
	if want := []byte(cliTestVersion + "\n"); !bytes.Equal(output, want) {
		t.Fatalf("epitropctl version = %q, want %q", output, want)
	}
}

// TestRelayCommandIsDispatched: the sandbox of a job with network=proxy runs this program
// as the relay in front of the job's command.
func TestRelayCommandIsDispatched(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the relay runs inside the sandbox, which is Linux only")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	socket := filepath.Join(t.TempDir(), "none.sock")
	output, err := exec.CommandContext(ctx, "go", "run", ".", "__egress-relay", "--socket", socket, "--listen", "127.0.0.1:0", "--", "/usr/bin/printf", "relayed").CombinedOutput() // #nosec G204 -- fixed go run uses a socket path under t.TempDir
	if err != nil {
		t.Fatalf("epitropctl __egress-relay: %v\n%s", err, output)
	}
	if want := []byte("relayed"); !bytes.Equal(output, want) {
		t.Fatalf("epitropctl __egress-relay = %q, want the command's output %q", output, want)
	}
}
