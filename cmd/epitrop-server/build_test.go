package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/mcp"
)

// serverBuildTimeout bounds one build of the command; a cold build on a loaded runner can
// take longer than a test's own deadline.
const serverBuildTimeout = 5 * time.Minute

// serverBuild holds the one build of the command that the tests running it share.
var serverBuild struct {
	once sync.Once
	dir  string
	bin  string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if serverBuild.dir != "" {
		if err := os.RemoveAll(serverBuild.dir); err != nil {
			fmt.Fprintf(os.Stderr, "remove the command build: %v\n", err)
		}
	}
	os.Exit(code)
}

// buildServer returns the path of the command, built once for this test process.
func buildServer(t *testing.T) string {
	t.Helper()
	serverBuild.once.Do(func() {
		serverBuild.bin, serverBuild.err = buildServerOnce()
	})
	if serverBuild.err != nil {
		t.Fatal(serverBuild.err)
	}
	return serverBuild.bin
}

func buildServerOnce() (string, error) {
	dir, err := os.MkdirTemp("", "epitrop-server-test-")
	if err != nil {
		return "", fmt.Errorf("create the build directory: %w", err)
	}
	serverBuild.dir = dir
	ctx, cancel := context.WithTimeout(context.Background(), serverBuildTimeout)
	defer cancel()
	bin := filepath.Join(dir, "epitrop-server")
	out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build epitrop-server: %w\n%s", err, out)
	}
	return bin, nil
}

// TestBuiltServerStartsAndStopsAJob runs the built command, whose supervisor starts the
// job shim as that same command.
func TestBuiltServerStartsAndStopsAJob(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the supervisor runs jobs on linux only, not %s", runtime.GOOS)
	}
	bin := buildServer(t)
	ctx := testContext(t)
	cmd := exec.CommandContext(ctx, bin, "--config="+writeConfigServing(t, `{"allow_mutations":true}`))
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("Connect() = %v", err)
	}
	t.Cleanup(func() {
		if err := cs.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	})
	callMutation(t, cs, mcp.ToolJobsStart, "start-1")
	waitJobState(t, cs, "running")
	callMutation(t, cs, mcp.ToolJobsStop, "stop-1")
	waitJobState(t, cs, "stopped")
}

func callMutation(t *testing.T, cs *sdkmcp.ClientSession, tool, key string) {
	t.Helper()
	res, err := cs.CallTool(testContext(t), &sdkmcp.CallToolParams{Name: tool, Arguments: map[string]any{"key": key, "name": "idle"}})
	if err != nil {
		t.Fatalf("CallTool(%s) = %v", tool, err)
	}
	var out mcp.MutationOut
	structured(t, res, &out)
	if !out.Executed {
		t.Fatalf("%s = %+v, want executed", tool, out)
	}
}

// waitJobState polls jobs_status until the idle job is in state, for at most 10 seconds.
func waitJobState(t *testing.T, cs *sdkmcp.ClientSession, state string) {
	t.Helper()
	var last mcp.JobsStatusOut
	for i := 0; i < 100; i++ {
		res, err := cs.CallTool(testContext(t), &sdkmcp.CallToolParams{Name: mcp.ToolJobsStatus, Arguments: map[string]any{"name": "idle"}})
		if err != nil {
			t.Fatalf("CallTool(jobs_status) = %v", err)
		}
		structured(t, res, &last)
		if len(last.Jobs) == 1 && last.Jobs[0].State == state {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("jobs_status = %+v, want idle %s", last, state)
}
