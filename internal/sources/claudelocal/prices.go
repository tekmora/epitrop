package claudelocal

// Ported from cordanaLLM/praetor tribunus/internal/sources/claudelocal at 5d4acfb (EUPL-1.2).

import (
	"regexp"
	"strings"
)

// rates are first-party API prices in USD per million tokens. They weigh
// usage across models; the absolute scale cancels against the ceiling,
// which is measured in the same unit.
type rates struct {
	input, output, cacheRead float64
}

// Cache writes cost 1.25x the input rate for the five-minute TTL and 2x for
// the one-hour TTL; fast mode costs 2x standard.
const (
	cacheWrite5mFactor = 1.25
	cacheWrite1hFactor = 2.0
	fastSpeedFactor    = 2.0
)

// claudeRates is the price list as of 2026-10-06 (Anthropic first-party
// rates, claude-api reference cached 2026-10-06). A model missing here is
// unpriced: its usage makes every window it falls in unknown rather than
// guessed. Haiku 5.5's higher rate above 100K-token prompts is not modelled.
var claudeRates = map[string]rates{
	"claude-fable-5-1":  {10, 50, 0.25},
	"claude-mythos-5-1": {10, 50, 0.25},
	"claude-fable-5":    {10, 50, 1},
	"claude-mythos-5":   {10, 50, 1},
	"claude-opus-5-5":   {4, 20, 0.20},
	"claude-opus-5":     {5, 25, 0.50},
	"claude-opus-4-8":   {5, 25, 0.50},
	"claude-opus-4-7":   {5, 25, 0.50},
	"claude-opus-4-6":   {5, 25, 0.50},
	"claude-sonnet-5-5": {2, 10, 0.20},
	"claude-sonnet-5":   {2, 10, 0.20},
	"claude-sonnet-4-6": {3, 15, 0.30},
	"claude-haiku-5-5":  {0.10, 0.50, 0.01},
	"claude-haiku-4-5":  {1, 5, 0.10},
}

// dateSuffix matches a dated snapshot id's -YYYYMMDD tail.
var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// costOf returns the cost-equivalent of one response. priced is false for a
// model without rates that used tokens; a response without tokens costs 0.
func costOf(model string, u usage) (cost float64, priced bool) {
	tokens := u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	if tokens <= 0 {
		return 0, true
	}
	r, ok := claudeRates[dateSuffix.ReplaceAllString(model, "")]
	if !ok {
		return 0, false
	}
	write5m, write1h := u.CacheCreationInputTokens, int64(0)
	if u.CacheCreation != nil {
		write5m, write1h = u.CacheCreation.Ephemeral5m, u.CacheCreation.Ephemeral1h
	}
	cost = (float64(u.InputTokens)*r.input + float64(u.OutputTokens)*r.output +
		float64(write5m)*r.input*cacheWrite5mFactor + float64(write1h)*r.input*cacheWrite1hFactor +
		float64(u.CacheReadInputTokens)*r.cacheRead) / 1e6
	if strings.EqualFold(u.Speed, "fast") {
		cost *= fastSpeedFactor
	}
	return cost, true
}
