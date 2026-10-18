// Package budget refuses a task before it starts when its declared budget does
// not fit what the platform budget layer has left (#6). The measured basis is
// the Praetor efficiency ledger; a figure the ledger has not measured refuses
// the task rather than counting as zero use.
package budget

import (
	"context"
	"errors"
	"fmt"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/ledger"
	"github.com/tekmora/epitrop/internal/telemetry"
)

// maxGaugeTokens is the largest use the budget gauge records; a ledger figure
// above it is still checked, only not recorded.
const maxGaugeTokens = 1 << 53

// ErrRefused is the error every budget refusal wraps.
var ErrRefused = errors.New("budget: refused")

// RefusedError says which layer refused a task and on what measured basis.
type RefusedError struct {
	Layer  string
	Reason string
}

// Error formats the refusal with its layer and reason.
func (e *RefusedError) Error() string {
	return fmt.Sprintf("budget: %s layer refused the task: %s", e.Layer, e.Reason)
}

// Unwrap returns ErrRefused.
func (e *RefusedError) Unwrap() error { return ErrRefused }

// Check admits a task declaring task when the platform layer is off
// (platform.FrontierTokens is 0) or when task.Tokens fit what rep leaves of
// platform.FrontierTokens; a task that fits exactly is admitted. Otherwise it
// returns a *RefusedError. It records the platform layer's use on tel when the
// ledger measured it, and nothing when it did not.
func Check(ctx context.Context, tel *telemetry.Telemetry, task config.TaskBudget, platform config.PlatformBudget, rep ledger.Report) error {
	if platform.FrontierTokens == 0 {
		return nil
	}
	used := rep.FrontierTokens
	if !used.Known() {
		return &RefusedError{Layer: telemetry.BudgetLayerPlatform, Reason: fmt.Sprintf(
			"the ledger reports frontier tokens as %q; with a platform budget of %d set, no task starts on an unmeasured basis",
			used.Reason, platform.FrontierTokens)}
	}
	if *used.Value <= maxGaugeTokens {
		recorded := int64(*used.Value)
		tel.BudgetUsed(ctx, telemetry.BudgetLayerPlatform, &recorded)
	}
	left := float64(platform.FrontierTokens) - *used.Value
	if float64(task.Tokens) > left {
		return &RefusedError{Layer: telemetry.BudgetLayerPlatform, Reason: fmt.Sprintf(
			"the task declares %d frontier tokens; the platform budget is %d and the ledger measured %.0f used, which leaves %.0f",
			task.Tokens, platform.FrontierTokens, *used.Value, max(left, 0))}
	}
	return nil
}
