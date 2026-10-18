package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

// runHeadroom is epitropctl's "headroom" subcommand: it reads a snapshot
// file written by sync and prints one JSON reading per usage window. A
// reading older than --max-age is unknown, never zero usage. --max-age has
// no default, so every consumer states how old a reading it accepts.
func runHeadroom(args []string) error {
	fs := flag.NewFlagSet("headroom", flag.ContinueOnError)
	in := fs.String("in", defaultSnapshotPath, "path to a JSON snapshot written by sync")
	maxAge := fs.Duration("max-age", 0, "oldest reading still known, e.g. 10m (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	snap, err := readSnapshotFile(*in)
	if err != nil {
		return err
	}
	return writeHeadroom(os.Stdout, snap, time.Now().UTC(), *maxAge)
}

func writeHeadroom(w io.Writer, snap catalog.Snapshot, now time.Time, maxAge time.Duration) error {
	readings, err := catalog.ReadHeadroom(snap, now, maxAge)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(readings); err != nil {
		return fmt.Errorf("write headroom: %w", err)
	}
	return nil
}
