// Package logsetup opens the shared event log the way every binary of this repository
// reads and writes it: the signer from the configured key, the configured limits, and the
// units service with Git recording and compaction as the config asks.
package logsetup

import (
	"context"
	"fmt"
	"time"

	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/effects"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graphlog"
	"github.com/tekmora/epitrop/internal/supervisor"
	"github.com/tekmora/epitrop/internal/units"
)

// OpenTimeout bounds opening the signing key.
const OpenTimeout = 30 * time.Second

// UnitsService opens the units service over the configured event log.
func UnitsService(cfg config.Config) (*units.Service, error) {
	signer, clk, err := Signer(cfg)
	if err != nil {
		return nil, err
	}
	svc, err := units.NewService(cfg.EventLog.Dir, signer, clk, Limits(cfg, clk))
	if err != nil {
		return nil, err
	}
	if g := cfg.EventLog.Git; g.Enabled {
		svc.RecordInGit(eventlog.GitRecord{
			Ref: g.Ref, AuthorName: g.AuthorName, AuthorEmail: g.AuthorEmail,
			Timeout: time.Duration(g.TimeoutSeconds) * time.Second,
		})
		if g.Compact {
			svc.CompactWith(Folds(cfg))
		}
	}
	return svc, nil
}

// EffectsLedger opens the effect ledger over the configured event log.
func EffectsLedger(cfg config.Config) (*effects.Ledger, error) {
	signer, clk, err := Signer(cfg)
	if err != nil {
		return nil, err
	}
	return effects.NewLedger(cfg.EventLog.Dir, signer, clk, Limits(cfg, clk))
}

// Folds names every reader of the shared event log, so that a compacted log's base holds
// the state each one replays from.
func Folds(cfg config.Config) []eventlog.Fold {
	return []eventlog.Fold{effects.Fold(), graphlog.Fold(), supervisor.Fold(cfg.Jobs), units.Fold()}
}

// Signer opens the signing key for the event log with a bounded context and returns the
// signer and the clock used to initialize it.
func Signer(cfg config.Config) (*corereceipt.Signer, clockwork.Clock, error) {
	clk := clockwork.NewRealClock()
	ctx, cancel := context.WithTimeout(context.Background(), OpenTimeout)
	defer cancel()
	signer, err := eventlog.NewSignerFromKeyFile(ctx, cfg.EventLog.SigningKeyPath, cfg.EventLog.Dir, clk)
	if err != nil {
		return nil, nil, fmt.Errorf("event-log signer %s: %w", cfg.EventLog.SigningKeyPath, err)
	}
	return signer, clk, nil
}

// Limits are the limits a command reads and writes the event log under: the configured
// replay bound and lock timeout.
func Limits(cfg config.Config, clk clockwork.Clock) eventlog.Limits {
	limits := eventlog.Limits{Clock: clk, MaxReplayRecords: cfg.EventLog.MaxReplayRecords}
	if cfg.EventLog.LockTimeoutSeconds > 0 {
		limits.LockTimeout = time.Duration(cfg.EventLog.LockTimeoutSeconds) * time.Second
	}
	return limits
}
