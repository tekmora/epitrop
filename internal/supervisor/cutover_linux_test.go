//go:build linux

package supervisor

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
)

// The cutover 1 example (docs/cutover-1.md) is a configuration the supervisor accepts, and it
// declares one job per background job the plan moves plus the shadow jobs of the parallel run.
func TestCutoverOneExampleIsASupervisorConfig(t *testing.T) {
	cfg, err := config.Load(testContext(t), filepath.Join("..", "..", "docs", "cutover-1.example.yaml"))
	if err != nil {
		t.Fatalf("Load(docs/cutover-1.example.yaml) = %v, want nil", err)
	}
	if err = checkConfig(cfg); err != nil {
		t.Fatalf("checkConfig(cutover 1) = %v, want nil", err)
	}
	names := make([]string, 0, len(cfg.Jobs))
	for _, job := range cfg.Jobs {
		names = append(names, job.Name)
	}
	want := []string{"landing-daemon", "auto-rebase", "update-intake", "fleet-monitor", "scratch-cleanup", "landing-daemon-shadow", "auto-rebase-shadow", "update-intake-shadow", "fleet-monitor-shadow"}
	if !slices.Equal(names, want) {
		t.Fatalf("cutover 1 jobs = %v, want %v", names, want)
	}
}

// A shadow job of the parallel run runs its script's dry mode, which writes nothing, and declares
// a heartbeat bound, because the dry mode prints a beat line every pass. Without DRYRUN=1 the
// shadow would act beside the live script.
func TestCutoverOneShadowJobsRunDryWithHeartbeat(t *testing.T) {
	cfg, err := config.Load(testContext(t), filepath.Join("..", "..", "docs", "cutover-1.example.yaml"))
	if err != nil {
		t.Fatalf("Load(docs/cutover-1.example.yaml) = %v, want nil", err)
	}
	shadows := 0
	for _, job := range cfg.Jobs {
		if !strings.HasSuffix(job.Name, "-shadow") {
			continue
		}
		shadows++
		if len(job.Command) < 3 || job.Command[0] != "/usr/bin/env" || job.Command[1] != "DRYRUN=1" {
			t.Errorf("job %s command = %v, want /usr/bin/env DRYRUN=1 <script>", job.Name, job.Command)
		}
		if job.Heartbeat.MaxAgeSeconds <= 0 {
			t.Errorf("job %s heartbeat.max_age_seconds = %d, want > 0", job.Name, job.Heartbeat.MaxAgeSeconds)
		}
	}
	if shadows == 0 {
		t.Fatal("cutover 1 declares no shadow job, want at least one")
	}
}
