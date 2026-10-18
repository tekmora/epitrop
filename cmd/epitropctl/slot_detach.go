package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tekmora/epitrop/internal/slot"
)

// slotDetachTimeout bounds the start of a detached run: reading the template and handing
// the runner to the user manager, not the run itself.
const slotDetachTimeout = 30 * time.Second

// startDetached runs the systemd-run command line of a detached run, found on PATH, and
// returns what it printed. Tests replace it to see the command line without starting a
// service.
var startDetached = func(ctx context.Context, argv []string) ([]byte, error) {
	// #nosec G204 -- argv is systemd-run with the run's own resolved arguments.
	return exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
}

// detachSlotRun starts the run as a transient systemd user service and returns once the
// user manager has it. The service belongs to the user manager, not to the caller's
// session or editor, so the run goes on when the caller ends. It carries the template's
// memory limits as MemoryMax and MemoryHigh; the job itself runs in a scope of its own
// with the same limits. What became of the run is in the output directory, and
// `slot resume --out` takes up a run whose runner stopped.
func detachSlotRun(f slotRunFlags, req slot.RunRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), slotDetachTimeout)
	defer cancel()
	limits, err := slot.TemplateLimits(ctx, *f.config)
	if err != nil {
		return fmt.Errorf("slot run --detach: %w", err)
	}
	argv, unit, err := detachedSlotCommand(f, req, limits)
	if err != nil {
		return err
	}
	if out, startErr := startDetached(ctx, argv); startErr != nil {
		return fmt.Errorf("slot run --detach: start %s: %w: %s", unit, startErr, out)
	}
	lines := []string{"service\t" + unit, "out\t" + req.Out}
	for i := 0; i < len(lines); i++ {
		if _, printErr := fmt.Fprintln(slotOut, lines[i]); printErr != nil {
			return fmt.Errorf("slot run --detach: print: %w", printErr)
		}
	}
	return nil
}

// detachedSlotCommand is the systemd-run command line of a detached run and the name of its
// service. Every path in it is absolute, because the service does not start in the caller's
// directory, and --expand-environment=no keeps the arguments as they are written.
func detachedSlotCommand(f slotRunFlags, req slot.RunRequest, limits slot.Limits) ([]string, string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("slot run --detach: executable: %w", err)
	}
	files := []*string{f.config, f.brief, f.resume, f.decisions}
	for i := 0; i < len(files); i++ {
		if *files[i] == "" {
			continue
		}
		abs, absErr := filepath.Abs(*files[i])
		if absErr != nil {
			return nil, "", fmt.Errorf("slot run --detach: %w", absErr)
		}
		*files[i] = abs
	}
	unit := slotServiceName(req.Out)
	argv := []string{
		"systemd-run", "--user", "--quiet", "--collect", "--expand-environment=no", "--unit=" + unit,
		"-p", "MemoryMax=" + limits.Max,
		"-p", "MemoryHigh=" + limits.High,
		"-p", "RuntimeMaxSec=" + strconv.Itoa(int((slotRunTimeout+time.Minute)/time.Second)),
		"--setenv=PATH=" + os.Getenv("PATH"),
	}
	// A name without a value makes systemd-run take the value from its own environment, so
	// the job's env_allow values reach the runner without standing in the command line.
	for i := 0; i < len(limits.EnvAllow); i++ {
		if _, set := os.LookupEnv(limits.EnvAllow[i]); set {
			argv = append(argv, "--setenv="+limits.EnvAllow[i])
		}
	}
	argv = append(argv,
		"--",
		self, "slot", "run",
		"--config", *f.config, "--repo", req.Repo, "--base", req.Base, "--brief", *f.brief,
		"--workspace", req.Dir, "--out", req.Out,
	)
	if *f.resume != "" {
		argv = append(argv, "--resume", *f.resume)
	}
	if *f.decisions != "" {
		argv = append(argv, "--decisions", *f.decisions)
	}
	return append(argv, "--", req.Unit), unit, nil
}

// slotServiceName names the service of a run after its output directory, which no other
// run may share: the same directory always gives the same name, and two runs never do.
func slotServiceName(out string) string {
	sum := sha256.Sum256([]byte(out))
	return "epitrop-slot-" + hex.EncodeToString(sum[:6]) + ".service"
}
