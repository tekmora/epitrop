package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/effects"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/gitexec"
	"github.com/tekmora/epitrop/internal/graphlog"
	"github.com/tekmora/epitrop/internal/slot"
	"github.com/tekmora/epitrop/internal/supervisor"
	"github.com/tekmora/epitrop/internal/units"
)

func writeUnitsConfig(t *testing.T) (string, string) {
	t.Helper()
	base := jobsTempDir(t)
	keyPath := filepath.Join(base, "seed.hex")
	seed := bytes.Repeat([]byte{7}, 32)
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatalf("WriteFile(key) = %v", err)
	}
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(filepath.Join(stateDir, ".git"), 0o700); err != nil {
		t.Fatalf("MkdirAll(state/.git) = %v", err)
	}
	cfgPath := filepath.Join(base, "config.json")
	body := fmt.Sprintf(`{"event_log":{"dir":%s,"signing_key_path":%s}}`, jsonQuote(stateDir), jsonQuote(keyPath))
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(config) = %v", err)
	}
	return cfgPath, stateDir
}

func captureStdoutUnits(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() = %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("Close pipe writer = %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("Copy pipe reader = %v", err)
	}
	return buf.String()
}

func TestRunUnitsRejectsMissingAndUnknownCommand(t *testing.T) {
	if err := runUnits(nil); err == nil || !strings.Contains(err.Error(), "units command required") {
		t.Fatalf("runUnits(nil) = %v, want command required", err)
	}
	if err := runUnits([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "unknown units command: bogus") {
		t.Fatalf("runUnits(bogus) = %v, want unknown command", err)
	}
}

func TestRunUnitsRequiresConfig(t *testing.T) {
	for _, action := range []string{"resume", "set", "note", "inbox", "preempt"} {
		t.Run(action, func(t *testing.T) {
			var args []string
			if action == "resume" {
				args = []string{action}
			} else {
				args = []string{action, "repo#1"}
			}
			switch action {
			case "set":
				args = append(args, "--status=ready")
			case "note":
				args = append(args, "--from=alice", "hello")
			case "preempt":
				args = append(args, "--reason=emergency")
			}
			err := runUnits(args)
			if err == nil || !strings.Contains(err.Error(), "--config is required") {
				t.Fatalf("runUnits(%s) = %v, want --config required", action, err)
			}
		})
	}
}

func TestRunUnitsBadFlagsExit2(t *testing.T) {
	for _, action := range []string{"set", "resume", "show", "note", "inbox", "preempt"} {
		t.Run(action, func(t *testing.T) {
			err := runUnits([]string{action, "-bad-flag"})
			if err == nil || !strings.Contains(err.Error(), "flag provided") {
				t.Fatalf("runUnits(%s, -bad-flag) = %v, want flag error", action, err)
			}
			var ec interface{ ExitCode() int }
			if !errors.As(err, &ec) || ec.ExitCode() != 2 {
				t.Fatalf("runUnits(%s) error exit code = %v, want 2", action, ec)
			}
		})
	}
}

func TestCLIUnitsBadFlagsExit2Subprocess(t *testing.T) {
	bin := buildCLI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "units", "set", "--bad-flag") // #nosec G204 -- bin is the CLI built by the test
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("epitropctl units set --bad-flag = nil error, want exit 2")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("exit code = %v, want 2; stderr = %s", err, stderr.String())
	}
}

func TestRunUnitsDispatchEndToEnd(t *testing.T) {
	cfgPath, _ := writeUnitsConfig(t)
	id := "tekmora/epitrop#22"

	setArgs := []string{"--config=" + cfgPath, id, "--status=implementing", "--pr=5", "--worktree=/wt/1", "--branch=feat/22", "--lane=core"}
	if err := runUnits(append([]string{"set"}, setArgs...)); err != nil {
		t.Fatalf("runUnits set: %v", err)
	}

	noteArgs := []string{"--config=" + cfgPath, id, "--from=reviewer", "first", "note", "for", "agent"}
	if err := runUnits(append([]string{"note"}, noteArgs...)); err != nil {
		t.Fatalf("runUnits note: %v", err)
	}

	checkUnitsDispatchReviewAndInbox(t, cfgPath, id)
	checkUnitsDispatchLandingAndResume(t, cfgPath, id)
}

func checkUnitsDispatchReviewAndInbox(t *testing.T, cfgPath string, id string) {
	t.Helper()
	setOut := captureStdoutUnits(t, func() {
		reviewArgs := []string{id, "--status=review", "--config=" + cfgPath}
		if err := runUnits(append([]string{"set"}, reviewArgs...)); err != nil {
			t.Fatalf("runUnits set review: %v", err)
		}
	})
	if !strings.Contains(setOut, `from reviewer: "first note for agent"`) {
		t.Fatalf("set review output = %q, want note 1 printed", setOut)
	}

	inboxOut := captureStdoutUnits(t, func() {
		inboxArgs := []string{id, "--config=" + cfgPath}
		if err := runUnits(append([]string{"inbox"}, inboxArgs...)); err != nil {
			t.Fatalf("runUnits inbox: %v", err)
		}
	})
	if !strings.Contains(inboxOut, `from reviewer: "first note for agent"`) {
		t.Fatalf("inbox output = %q, want note 1 printed", inboxOut)
	}
}

func checkUnitsDispatchLandingAndResume(t *testing.T, cfgPath string, id string) {
	t.Helper()
	landingOut := captureStdoutUnits(t, func() {
		landingArgs := []string{id, "--status=landing", "--config=" + cfgPath}
		if err := runUnits(append([]string{"set"}, landingArgs...)); err != nil {
			t.Fatalf("runUnits set landing: %v", err)
		}
	})
	if landingOut != "" {
		t.Fatalf("set landing output = %q, want empty after inbox read", landingOut)
	}

	resumeOut := captureStdoutUnits(t, func() {
		resumeArgs := []string{"--config=" + cfgPath}
		if err := runUnits(append([]string{"resume"}, resumeArgs...)); err != nil {
			t.Fatalf("runUnits resume: %v", err)
		}
	})
	checkUnitsDispatchResumeOutput(t, resumeOut, id)
}

func checkUnitsDispatchResumeOutput(t *testing.T, resumeOut string, id string) {
	t.Helper()
	if !strings.Contains(resumeOut, "id\tstatus\tupdated_at\tworktree\tbranch\tpr\tlane\trelaunch") {
		t.Fatalf("resume header missing in: %q", resumeOut)
	}
	if !strings.Contains(resumeOut, id+"\tlanding\t") || !strings.Contains(resumeOut, "no: has PR #5; check its state before relaunching") {
		t.Fatalf("resume row mismatch in: %q", resumeOut)
	}
	if !strings.Contains(resumeOut, "\t/wt/1\tfeat/22\t5\tcore\t") {
		t.Fatalf("resume row = %q, want worktree, branch, PR and lane kept from the first set", resumeOut)
	}
}

const testIdentity = `{"physical_model":"model-x","harness":"harness-a","harness_version":"1.2.3","tool_count":3,"future_key":"kept"}`

// TestRunUnitsIdentityFromFileAndStdin: the run identity is taken as an object from a file
// or stdin, stored with its key, and shown again by `units show`.
// TestRunUnitsSetRecordsAnEvidencePointer: --evidence takes the pointer form used in reports,
// units show prints it back, and a malformed pointer is refused before anything is recorded.
func TestRunUnitsSetRecordsAnEvidencePointer(t *testing.T) {
	cfgPath, _ := writeUnitsConfig(t)
	id := "tekmora/epitrop#22"
	if err := runUnits([]string{"set", "--config=" + cfgPath, id, "--status=implementing"}); err != nil {
		t.Fatalf("runUnits set: %v", err)
	}
	checkUnitsSetEvidenceRefusal(t, cfgPath, id)
	checkUnitsSetEvidencePreserved(t, cfgPath, id)
}

func checkUnitsSetEvidenceRefusal(t *testing.T, cfgPath string, id string) {
	t.Helper()
	err := runUnits([]string{"set", "--config=" + cfgPath, id, "--status=review", "--evidence=out/result.json sha256:nothex lines:3"})
	if err == nil || !strings.Contains(err.Error(), "evidence sha256 must be") {
		t.Fatalf("runUnits set (malformed evidence) = %v, want the digest refusal", err)
	}
	before := captureStdoutUnits(t, func() {
		if showErr := runUnits([]string{"show", "--config=" + cfgPath, id}); showErr != nil {
			t.Fatalf("runUnits show: %v", showErr)
		}
	})
	if strings.Contains(before, `"evidence"`) || !strings.Contains(before, `"status": "implementing"`) && !strings.Contains(before, `"status":"implementing"`) {
		t.Fatalf("show after a refused set = %s, want the unit unchanged and without evidence", before)
	}
}

func checkUnitsSetEvidencePreserved(t *testing.T, cfgPath string, id string) {
	t.Helper()
	pointer := "--evidence=.workingdir/evidence/unit 22.md sha256:0123456789ab lines:57"
	if err := runUnits([]string{"set", "--config=" + cfgPath, id, "--status=review", pointer}); err != nil {
		t.Fatalf("runUnits set (evidence): %v", err)
	}
	if err := runUnits([]string{"set", "--config=" + cfgPath, id, "--status=review", "--lane=orchestrator"}); err != nil {
		t.Fatalf("runUnits set (no evidence): %v", err)
	}
	after := captureStdoutUnits(t, func() {
		if showErr := runUnits([]string{"show", "--config=" + cfgPath, id}); showErr != nil {
			t.Fatalf("runUnits show: %v", showErr)
		}
	})
	for _, want := range []string{`.workingdir/evidence/unit 22.md`, `0123456789ab`, `57`, `orchestrator`} {
		if !strings.Contains(after, want) {
			t.Fatalf("show = %s, want %q: the pointer is kept across a record without one", after, want)
		}
	}
}

func TestRunUnitsIdentityFromFileAndStdin(t *testing.T) {
	cfgPath, _ := writeUnitsConfig(t)
	identityFile := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(identityFile, []byte(testIdentity+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(identity) = %v", err)
	}
	key := "sha256:" + strings.Repeat("ab", 32)
	fromFile := "tekmora/epitrop#1"
	if err := runUnits([]string{"set", fromFile, "--status=implementing", "--config=" + cfgPath, "--identity=" + identityFile, "--identity-key=" + key, "--target=cordana/coding", "--resolved-model=model-x"}); err != nil {
		t.Fatalf("units set --identity=file: %v", err)
	}
	oldStdin := unitsStdin
	t.Cleanup(func() { unitsStdin = oldStdin })
	unitsStdin = strings.NewReader(testIdentity)
	fromStdin := "tekmora/epitrop#2"
	if err := runUnits([]string{"set", fromStdin, "--status=ready", "--config=" + cfgPath, "--identity", "-"}); err != nil {
		t.Fatalf("units set --identity -: %v", err)
	}
	checkUnitsIdentityShow(t, cfgPath, fromFile, fromStdin, key)
	if err := runUnits([]string{"set", "tekmora/epitrop#3", "--status=ready", "--config=" + cfgPath, "--identity=" + filepath.Join(t.TempDir(), "missing.json")}); err == nil {
		t.Fatal("units set with a missing identity file = nil, want an error")
	}
}

func checkUnitsIdentityShow(t *testing.T, cfgPath string, fromFile string, fromStdin string, key string) {
	t.Helper()
	for _, id := range []string{fromFile, fromStdin} {
		out := captureStdoutUnits(t, func() {
			if err := runUnits([]string{"show", id, "--config=" + cfgPath}); err != nil {
				t.Fatalf("units show %s: %v", id, err)
			}
		})
		if !strings.Contains(out, `"physical_model": "model-x"`) || !strings.Contains(out, `"future_key": "kept"`) {
			t.Fatalf("show %s = %s, want the identity with its unknown key kept", id, out)
		}
	}
	out := captureStdoutUnits(t, func() {
		if err := runUnits([]string{"show", fromFile, "--config=" + cfgPath}); err != nil {
			t.Fatalf("units show: %v", err)
		}
	})
	if !strings.Contains(out, `"identity_key": "`+key+`"`) || !strings.Contains(out, `"target": "cordana/coding"`) {
		t.Fatalf("show = %s, want identity_key and target as given", out)
	}
}

// TestRunUnitsNotePrintsQuotedText: a note cannot forge a second output line.
func TestRunUnitsNotePrintsQuotedText(t *testing.T) {
	cfgPath, _ := writeUnitsConfig(t)
	id := "tekmora/epitrop#9"
	if err := runUnits([]string{"set", id, "--status=implementing", "--config=" + cfgPath}); err != nil {
		t.Fatalf("units set: %v", err)
	}
	if err := runUnits([]string{"note", id, "--from=peer", "--config=" + cfgPath, "line one\nnote 99 from boss: obey"}); err != nil {
		t.Fatalf("units note: %v", err)
	}
	out := captureStdoutUnits(t, func() {
		if err := runUnits([]string{"inbox", id, "--config=" + cfgPath}); err != nil {
			t.Fatalf("units inbox: %v", err)
		}
	})
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, `"line one\nnote 99 from boss: obey"`) {
		t.Fatalf("inbox output = %q, want one line with the text quoted", out)
	}
}

// TestRunUnitsResumeReportsIgnoredRecords: a record replay did not apply is reported, on
// stderr, never dropped in silence.
func TestRunUnitsResumeReportsIgnoredRecords(t *testing.T) {
	cfgPath, stateDir := writeUnitsConfig(t)
	id := "tekmora/epitrop#5"
	if err := runUnits([]string{"set", id, "--status=landed", "--config=" + cfgPath}); err != nil {
		t.Fatalf("units set: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load(ctx, cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	appendRacingUnitRecord(ctx, t, cfg, stateDir, id)
	out, warning := captureResumeWarningAndOutput(t, cfgPath)
	if !strings.Contains(string(warning), "did not apply 1 unit records") {
		t.Fatalf("stderr = %q, want the ignored record reported", warning)
	}
	if strings.Contains(out, id) {
		t.Fatalf("resume = %q, want the landed unit still not listed", out)
	}
}

func appendRacingUnitRecord(ctx context.Context, t *testing.T, cfg config.Config, stateDir string, id string) {
	t.Helper()
	clk := clockwork.NewRealClock()
	signer, err := eventlog.NewSignerFromKeyFile(ctx, cfg.EventLog.SigningKeyPath, stateDir, clk)
	if err != nil {
		t.Fatalf("NewSignerFromKeyFile: %v", err)
	}
	writer, err := eventlog.Open(stateDir, signer, eventlog.Limits{Clock: clk})
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	if _, err = writer.Append(ctx, eventlog.Event{Type: "unit.recorded", TaskID: id, Payload: []byte(`{"status":"implementing"}`)}); err != nil {
		t.Fatalf("Append(racing record): %v", err)
	}
}

func captureResumeWarningAndOutput(t *testing.T, cfgPath string) (string, []byte) {
	t.Helper()
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stderr = w
	out := captureStdoutUnits(t, func() {
		if runErr := runUnits([]string{"resume", "--config=" + cfgPath}); runErr != nil {
			t.Errorf("units resume: %v", runErr)
		}
	})
	os.Stderr = oldStderr
	if err = w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	warning, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll(stderr): %v", err)
	}
	return out, warning
}

func TestRunUnitsSetNamesTheMissingStatus(t *testing.T) {
	cfgPath, _ := writeUnitsConfig(t)
	err := runUnits([]string{"set", "tekmora/epitrop#1", "--config=" + cfgPath})
	if err == nil || !strings.Contains(err.Error(), "--status is required") {
		t.Fatalf("units set without --status = %v, want the flag named", err)
	}
}

func TestRunUnitsResumeWithStartFileEvidence(t *testing.T) {
	cfgPath, _ := writeUnitsConfig(t)
	idWithStart := "tekmora/epitrop#10"
	idWithoutStart := "tekmora/epitrop#11"

	outDir, pointer := setupStartFileEvidence(t, idWithStart)

	setArgs1 := []string{"set", idWithStart, "--status=implementing", "--evidence=" + pointer, "--config=" + cfgPath}
	if err := runUnits(setArgs1); err != nil {
		t.Fatalf("runUnits set 1: %v", err)
	}
	setArgs2 := []string{"set", idWithoutStart, "--status=implementing", "--config=" + cfgPath}
	if err := runUnits(setArgs2); err != nil {
		t.Fatalf("runUnits set 2: %v", err)
	}

	out := captureStdoutUnits(t, func() {
		if err := runUnits([]string{"resume", "--config=" + cfgPath}); err != nil {
			t.Fatalf("runUnits resume: %v", err)
		}
	})

	checkResumeWithStartOutput(t, out, outDir, idWithStart, idWithoutStart)
}

func setupStartFileEvidence(t *testing.T, idWithStart string) (string, string) {
	t.Helper()
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(outDir): %v", err)
	}
	startBody := []byte("{\n  \"unit\": \"" + idWithStart + "\",\n  \"workspace\": \"/workspaces/slot-ws\",\n  \"branch\": \"unit/takeup\",\n  \"base\": \"abcd1234\",\n  \"job\": \"slot-harness\",\n  \"prepared_at\": \"2026-10-14T10:00:00Z\"\n}\n")
	startPath := filepath.Join(outDir, slot.StartFile)
	if err := os.WriteFile(startPath, startBody, 0o600); err != nil {
		t.Fatalf("WriteFile(start.json): %v", err)
	}
	h := sha256.Sum256(startBody)
	digest := hex.EncodeToString(h[:])
	pointer := fmt.Sprintf("%s sha256:%s lines:8", startPath, digest)
	return outDir, pointer
}

func checkResumeWithStartOutput(t *testing.T, out string, outDir string, idWithStart string, idWithoutStart string) {
	t.Helper()
	wantTakeUp := fmt.Sprintf("no: take up with slot resume --out %s (job slot-harness, workspace /workspaces/slot-ws)", outDir)
	if !strings.Contains(out, idWithStart) || !strings.Contains(out, wantTakeUp) {
		t.Fatalf("resume output = %q, want take up relaunch text %q", out, wantTakeUp)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var lineWithout string
	for i := 0; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], idWithoutStart+"\t") {
			lineWithout = lines[i]
			break
		}
	}
	if lineWithout == "" || !strings.HasSuffix(lineWithout, "\tsafe") {
		t.Fatalf("row for unit without pointer = %q, want safe relaunch", lineWithout)
	}
}

func TestRunUnitsPreemptAndResume(t *testing.T) {
	cfgPath, _ := writeUnitsConfig(t)
	id := "tekmora/epitrop#33"

	if err := runUnits([]string{"set", "--config=" + cfgPath, id, "--status=implementing"}); err != nil {
		t.Fatalf("runUnits set: %v", err)
	}

	if err := runUnits([]string{"preempt", "--config=" + cfgPath, id}); err == nil {
		t.Fatal("preempt without --reason succeeded, want error")
	}

	out := captureStdoutUnits(t, func() {
		err := runUnits([]string{"preempt", "--config=" + cfgPath, id, "--reason=shift priority", "--by=alice"})
		if err != nil {
			t.Fatalf("runUnits preempt: %v", err)
		}
	})
	if !strings.Contains(out, `"status": "blocked"`) || !strings.Contains(out, `"reason": "shift priority"`) {
		t.Fatalf("preempt output = %q, want blocked status and preemption reason", out)
	}

	resumeOut := captureStdoutUnits(t, func() {
		if err := runUnits([]string{"resume", "--config=" + cfgPath}); err != nil {
			t.Fatalf("runUnits resume: %v", err)
		}
	})
	if !strings.Contains(resumeOut, id+"\tblocked\t") || !strings.Contains(resumeOut, "no: preempted: shift priority") {
		t.Fatalf("resume output = %q, want blocked with preempted relaunch reason", resumeOut)
	}
}

type eventLogSignerCase struct {
	name         string
	keyPath      *string
	wantErr      bool
	wantNotExist bool
}

func TestEventLogSigner(t *testing.T) {
	missing := filepath.Join(jobsTempDir(t), "missing-key.hex")
	cases := []eventLogSignerCase{
		{name: "configured key opens"},
		{name: "missing key file fails with its path", keyPath: &missing, wantErr: true, wantNotExist: true},
		{name: "empty key path fails", keyPath: new(string), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := signerTestConfig(t, tc.keyPath)
			signer, clk, err := eventLogSigner(cfg)
			if !tc.wantErr {
				if err != nil || signer == nil || clk == nil {
					t.Fatalf("eventLogSigner() = %v, %v, %v; want a signer, a clock and nil", signer, clk, err)
				}
				return
			}
			checkSignerFailure(t, tc, signer != nil || clk != nil, err, cfg.EventLog.SigningKeyPath)
		})
	}
}

// signerTestConfig loads the units test configuration, with the signing key path replaced
// when keyPath is set.
func signerTestConfig(t *testing.T, keyPath *string) config.Config {
	t.Helper()
	cfgPath, _ := writeUnitsConfig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load(ctx, cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if keyPath != nil {
		cfg.EventLog.SigningKeyPath = *keyPath
	}
	return cfg
}

func checkSignerFailure(t *testing.T, tc eventLogSignerCase, gotValue bool, err error, keyPath string) {
	t.Helper()
	if err == nil || gotValue {
		t.Fatalf("eventLogSigner() error = %v, value returned = %v; want an error and no value", err, gotValue)
	}
	if tc.wantNotExist && (!errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), keyPath)) {
		t.Fatalf("eventLogSigner() error = %v, want os.ErrNotExist naming %s", err, keyPath)
	}
}

func TestRunUnitsSetRecordsTheLogInGitWhenEnabled(t *testing.T) {
	repo, cfgPath := gitLogConfig(t, false)
	env := gitexec.Env(os.Environ())
	if err := runUnits([]string{"set", "--config=" + cfgPath, "tekmora/epitrop#58", "--status=ready"}); err != nil {
		t.Fatalf("runUnits set: %v", err)
	}
	files, err := gitexec.Run(testContext(t), repo, env, nil, "ls-tree", "-r", "--name-only", "refs/epitrop/events")
	if err != nil || !strings.Contains(files, "events/HEAD.json") || !strings.Contains(files, "checkpoints/units/") {
		t.Fatalf("refs/epitrop/events holds %q (%v), want the log and its checkpoint", files, err)
	}
	fillASegment(t, cfgPath, "tekmora/epitrop#58")
	if _, err = os.Stat(filepath.Join(repo, ".epitrop", "checkpoints", eventlog.BaseName)); !os.IsNotExist(err) {
		t.Fatalf("base without event_log.git.compact = %v, want none", err)
	}
	if segs, err := os.ReadDir(filepath.Join(repo, ".epitrop", "events", "segments")); err != nil || len(segs) == 0 {
		t.Fatalf("segments without event_log.git.compact = %d (%v), want kept", len(segs), err)
	}
}

// gitLogConfig writes a configuration whose event log lies in a new Git repository, with
// event_log.git enabled and event_log.git.compact set to compact, and returns the repository
// and the configuration's path.
func gitLogConfig(t *testing.T, compact bool) (string, string) {
	t.Helper()
	base := t.TempDir()
	keyPath := filepath.Join(base, "seed.hex")
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(bytes.Repeat([]byte{7}, 32))), 0o600); err != nil {
		t.Fatalf("WriteFile(key) = %v", err)
	}
	repo := filepath.Join(base, "repo")
	if _, err := gitexec.Run(testContext(t), "", gitexec.Env(os.Environ()), nil, "init", "--quiet", repo); err != nil {
		t.Fatalf("git init = %v", err)
	}
	cfgPath := filepath.Join(base, "config.json")
	body := fmt.Sprintf(`{"event_log":{"dir":%s,"signing_key_path":%s,"git":{"enabled":true,"compact":%t,"author_name":"epitrop","author_email":"epitrop@example.invalid"}}}`,
		jsonQuote(filepath.Join(repo, ".epitrop")), jsonQuote(keyPath), compact)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(config) = %v", err)
	}
	return repo, cfgPath
}

// fillASegment writes enough records to unit through the configuration at cfgPath for a
// segment, then two status changes that rotate it and record it in Git, and returns the
// loaded configuration.
func fillASegment(t *testing.T, cfgPath string, unit string) config.Config {
	t.Helper()
	cfg, err := config.Load(testContext(t), cfgPath)
	if err != nil {
		t.Fatalf("config.Load() = %v", err)
	}
	svc, err := newUnitsService(cfg)
	if err != nil {
		t.Fatalf("newUnitsService() = %v", err)
	}
	// One segment holds 256 records; notes fill it without a checkpoint.
	for i := 0; i < 256; i++ {
		if _, err = svc.Note(testContext(t), unit, "tester", fmt.Sprintf("note %d", i)); err != nil {
			t.Fatalf("Note(%d) = %v", i, err)
		}
	}
	for _, status := range []string{"claimed", "implementing"} {
		if err = runUnits([]string{"set", "--config=" + cfgPath, unit, "--status=" + status}); err != nil {
			t.Fatalf("runUnits set %s: %v", status, err)
		}
	}
	return cfg
}

func TestLogFoldsNameEveryReaderOfTheLog(t *testing.T) {
	var names []string
	for _, f := range logFolds(config.Default()) {
		names = append(names, f.Name)
	}
	want := []string{effects.FoldName, graphlog.FoldName, supervisor.FoldName, units.FoldName}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("logFolds() = %v, want %v", names, want)
	}
}

func TestRunUnitsSetCompactsTheLogWhenAsked(t *testing.T) {
	repo, cfgPath := gitLogConfig(t, true)
	const unit = "tekmora/epitrop#58"
	if err := runUnits([]string{"set", "--config=" + cfgPath, unit, "--status=ready"}); err != nil {
		t.Fatalf("runUnits set: %v", err)
	}
	cfg := fillASegment(t, cfgPath, unit)
	if _, err := os.Stat(filepath.Join(repo, ".epitrop", "checkpoints", eventlog.BaseName)); err != nil {
		t.Fatalf("base = %v, want written", err)
	}
	signer, _, err := eventLogSigner(cfg)
	if err != nil {
		t.Fatalf("eventLogSigner() = %v", err)
	}
	for _, f := range logFolds(cfg) {
		if _, err = eventlog.ReplayFold(testContext(t), cfg.EventLog.Dir, signer.PublicKey(), eventLogLimits(cfg, clockwork.NewRealClock()), f.Name, f.Reduce); err != nil {
			t.Fatalf("ReplayFold(%s) on the compacted log = %v", f.Name, err)
		}
	}
}
