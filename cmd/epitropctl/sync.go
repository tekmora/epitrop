package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tekmora/epitrop/catalog"
	"github.com/tekmora/epitrop/internal/atomicfile"
	"github.com/tekmora/epitrop/internal/sources/codexlocal"
	"github.com/tekmora/epitrop/internal/sources/litellmgateway"
	"github.com/tekmora/epitrop/internal/sources/ollamalocal"
	"github.com/tekmora/epitrop/internal/sources/publiccatalog"
)

// defaultSnapshotPath is where sync writes when --out is not given.
const defaultSnapshotPath = "epitrop-snapshot.json"

// defaultOllamaEndpoint is Ollama's standard local daemon address.
const defaultOllamaEndpoint = "http://localhost:11434"

// syncTimeout bounds the whole sync command, on top of each source's own
// per-request timeout, so a hung DNS lookup or similar cannot block forever
// (HISS-02).
const syncTimeout = 3 * time.Minute

// maxSnapshotFileBytes bounds the snapshot file sync writes (HISS-02).
const maxSnapshotFileBytes = 64 << 20

// allSourceNames lists every source sync knows, in the order runSync
// attempts them.
var allSourceNames = []string{
	codexlocal.SourceName,
	litellmgateway.SourceName,
	litellmgateway.KeyBudgetSourceName,
	ollamalocal.SourceName,
	publiccatalog.SourceName,
}

// syncFlags holds sync's parsed command-line flags.
type syncFlags struct {
	sources          string
	out              string
	litellmBase      string
	litellmToken     string
	ollama           string
	openRouterURL    string
	litellmPricesURL string
	sessionsDir      string
}

func parseSyncFlags(args []string) (syncFlags, error) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	var f syncFlags
	fs.StringVar(&f.sources, "sources", strings.Join(allSourceNames, ","), "comma-separated source names to run")
	fs.StringVar(&f.out, "out", defaultSnapshotPath, "path to write the JSON snapshot")
	fs.StringVar(&f.litellmBase, "litellm-base", "", "LiteLLM gateway base URL (required for litellm-gateway and litellm-key-budget)")
	fs.StringVar(&f.litellmToken, "litellm-token-file", "", "path to a file holding the LiteLLM bearer token (required for litellm-gateway and litellm-key-budget)")
	fs.StringVar(&f.ollama, "ollama", defaultOllamaEndpoint, "Ollama daemon endpoint")
	fs.StringVar(&f.openRouterURL, "openrouter-url", publiccatalog.DefaultOpenRouterURL, "OpenRouter public models API URL")
	fs.StringVar(&f.litellmPricesURL, "litellm-prices-url", publiccatalog.DefaultLiteLLMPriceMapURL, "LiteLLM public price map URL")
	fs.StringVar(&f.sessionsDir, "sessions-dir", "", "Codex CLI sessions directory (default: ~/.codex/sessions)")
	if err := fs.Parse(args); err != nil {
		return syncFlags{}, err
	}
	return f, nil
}

// runSync is epitropctl's "sync" subcommand: it runs every selected
// source, prints one ok/skip/fail line per source, and writes the combined
// snapshot to --out. Individual source failures are reported on their own
// lines, but the command fails when every selected source fails.
func runSync(args []string) error {
	f, err := parseSyncFlags(args)
	if err != nil {
		return err
	}
	selected, err := selectSources(f.sources)
	if err != nil {
		return err
	}
	if err := validateSelectedRecordCap(selected); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()

	snap, err := collectSourceRuns(ctx, selected, f)
	if err != nil {
		return err
	}

	data, err := snap.Marshal()
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	if len(data) > maxSnapshotFileBytes {
		return fmt.Errorf("snapshot exceeds %d bytes, refusing to write", maxSnapshotFileBytes)
	}
	if err := writeSnapshotFile(f.out, data); err != nil {
		return err
	}
	fmt.Printf("wrote %d records to %s\n", len(snap.Records), f.out)
	return nil
}

// selectSources parses the --sources flag into a validated, ordered list.
func selectSources(raw string) ([]string, error) {
	fields := strings.Split(raw, ",")
	if len(fields) > len(allSourceNames) {
		return nil, fmt.Errorf("--sources lists more than %d sources", len(allSourceNames))
	}
	known := make(map[string]bool, len(allSourceNames))
	for _, n := range allSourceNames {
		known[n] = true
	}
	seen := make(map[string]bool, len(allSourceNames))
	var out []string
	for _, field := range fields {
		name := strings.TrimSpace(field)
		if name == "" {
			return nil, fmt.Errorf("--sources contains an empty entry")
		}
		if !known[name] {
			return nil, fmt.Errorf("unknown source %q (known: %s)", name, strings.Join(allSourceNames, ", "))
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate source %q", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--sources selected no sources")
	}
	return out, nil
}

func validateSelectedRecordCap(selected []string) error {
	selectedCap, err := selectedRecordCap(selected)
	if err != nil {
		return err
	}
	if selectedCap > catalog.MaxSnapshotRecords {
		return fmt.Errorf("selected source cap %d exceeds %d", selectedCap, catalog.MaxSnapshotRecords)
	}
	return nil
}

// sourceOutcome normalizes every source package's own Result type into one
// shape runSync can report and fold into a Snapshot.
type sourceOutcome struct {
	Records []catalog.Record
	Status  catalog.Status
	Count   int
	Detail  string
}

func (o sourceOutcome) SourceRun(name string) catalog.SourceRun {
	return catalog.SourceRun{Source: name, Status: o.Status, Count: o.Count, Detail: o.Detail}
}

func collectSourceRuns(ctx context.Context, selected []string, f syncFlags) (catalog.Snapshot, error) {
	snap := catalog.Snapshot{SchemaVersion: catalog.SnapshotSchemaVersion, GeneratedAt: time.Now().UTC()}
	allFailed := true
	for _, name := range selected {
		run := rejectInvalidRecords(runOneSource(ctx, name, f))
		snap.SourceRuns = append(snap.SourceRuns, run.SourceRun(name))
		snap.Records = append(snap.Records, run.Records...)
		fmt.Println(formatSourceLine(name, run))
		if run.Status != catalog.StatusFail {
			allFailed = false
		}
	}
	if allFailed {
		return catalog.Snapshot{}, fmt.Errorf("every selected source failed")
	}
	return snap, nil
}

// maxRejectNotes bounds how many rejected records rejectInvalidRecords names
// in a source's detail line; the rejected=N count always covers all of them.
const maxRejectNotes = 3

func rejectInvalidRecords(run sourceOutcome) sourceOutcome {
	if len(run.Records) == 0 {
		return run
	}
	kept := make([]catalog.Record, 0, len(run.Records))
	rejected := 0
	for i, rec := range run.Records {
		if err := rec.Validate(); err != nil {
			rejected++
			if rejected <= maxRejectNotes {
				run.Detail = appendDetail(run.Detail, fmt.Sprintf("rejected record %d: %v", i, err))
			}
			continue
		}
		kept = append(kept, rec)
	}
	if rejected == 0 {
		return run
	}
	run.Records = kept
	run.Count = len(kept)
	run.Detail = appendDetail(run.Detail, fmt.Sprintf("rejected=%d", rejected))
	if len(kept) == 0 {
		run.Status = catalog.StatusFail
	} else {
		run.Status = catalog.StatusDegraded
	}
	return run
}

func appendDetail(existing, note string) string {
	if existing == "" {
		return note
	}
	return existing + "; " + note
}

// runOneSource dispatches to the named source's Fetch and normalizes its
// result. Flag values a source needs but was not given (e.g. litellm-base
// for litellm-gateway) are reported as a skip, not a crash.
func runOneSource(ctx context.Context, name string, f syncFlags) sourceOutcome {
	switch name {
	case codexlocal.SourceName:
		return runCodexLocal(ctx, f)
	case litellmgateway.SourceName:
		return runLiteLLMGateway(ctx, f)
	case litellmgateway.KeyBudgetSourceName:
		return runLiteLLMKeyBudget(ctx, f)
	case ollamalocal.SourceName:
		res := ollamalocal.Fetch(ctx, f.ollama)
		return sourceOutcome{Records: res.Records, Status: res.Status, Count: res.Count, Detail: res.Detail}
	case publiccatalog.SourceName:
		res := publiccatalog.Fetch(ctx, f.openRouterURL, f.litellmPricesURL)
		return sourceOutcome{Records: res.Records, Status: res.Status, Count: res.Count, Detail: res.Detail}
	default:
		return sourceOutcome{Status: catalog.StatusSkip, Detail: "unknown source"}
	}
}

func sourceRecordCap(name string) (int, bool) {
	switch name {
	case codexlocal.SourceName:
		return codexlocal.MaxRecords, true
	case litellmgateway.SourceName:
		return litellmgateway.MaxRecords, true
	case litellmgateway.KeyBudgetSourceName:
		return litellmgateway.KeyBudgetMaxRecords, true
	case ollamalocal.SourceName:
		return ollamalocal.MaxRecords, true
	case publiccatalog.SourceName:
		return publiccatalog.MaxRecords, true
	default:
		return 0, false
	}
}

func selectedRecordCap(names []string) (int, error) {
	total := 0
	if len(names) > len(allSourceNames) {
		return 0, fmt.Errorf("selected source list exceeds %d entries", len(allSourceNames))
	}
	for _, name := range names {
		capacity, ok := sourceRecordCap(name)
		if !ok {
			return 0, fmt.Errorf("unknown source %q", name)
		}
		total += capacity
	}
	return total, nil
}

func runCodexLocal(ctx context.Context, f syncFlags) sourceOutcome {
	dir := f.sessionsDir
	if dir == "" {
		resolved, err := codexlocal.DefaultSessionsDir()
		if err != nil {
			return sourceOutcome{Status: catalog.StatusFail, Detail: err.Error()}
		}
		dir = resolved
	}
	res := codexlocal.Fetch(ctx, dir)
	return sourceOutcome{Records: res.Records, Status: res.Status, Count: res.Count, Detail: res.Detail}
}

func runLiteLLMGateway(ctx context.Context, f syncFlags) sourceOutcome {
	if f.litellmBase == "" || f.litellmToken == "" {
		return sourceOutcome{Status: catalog.StatusSkip, Detail: "--litellm-base and --litellm-token-file are required for litellm-gateway"}
	}
	res := litellmgateway.Fetch(ctx, f.litellmBase, f.litellmToken)
	return sourceOutcome{Records: res.Records, Status: res.Status, Count: res.Count, Detail: res.Detail}
}

func runLiteLLMKeyBudget(ctx context.Context, f syncFlags) sourceOutcome {
	if f.litellmBase == "" || f.litellmToken == "" {
		return sourceOutcome{Status: catalog.StatusSkip, Detail: "--litellm-base and --litellm-token-file are required for litellm-key-budget"}
	}
	res := litellmgateway.FetchKeyBudget(ctx, f.litellmBase, f.litellmToken)
	return sourceOutcome{Records: res.Records, Status: res.Status, Count: res.Count, Detail: res.Detail}
}

func formatSourceLine(name string, o sourceOutcome) string {
	line := fmt.Sprintf("%-16s %-4s count=%d", name, o.Status, o.Count)
	if o.Detail != "" {
		line += " " + o.Detail
	}
	return line
}

// writeSnapshotFile replaces path with data atomically, creating parent directories as
// needed, so a reader or a crash never sees a half-written snapshot.
func writeSnapshotFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create output directory %s: %w", dir, err)
		}
	}
	if err := atomicfile.Replace(path, data, 0o600); err != nil {
		return fmt.Errorf("write snapshot %s: %w", path, err)
	}
	return nil
}
