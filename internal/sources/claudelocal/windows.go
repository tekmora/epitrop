package claudelocal

// Ported from cordanaLLM/praetor tribunus/internal/sources/claudelocal at 5d4acfb (EUPL-1.2).

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

// windowKinds maps a rateLimitType prefix to its window length. A type with
// a suffix (seven_day_opus) is a per-model sub-limit: only usage of models
// whose id names that family counts toward it. five_hour is the type seen in
// the logs on kcromm; seven_day and its suffixed forms follow the same
// naming and are not yet observed there.
var windowKinds = []struct {
	prefix string
	length time.Duration
}{
	{"five_hour", 5 * time.Hour},
	{"seven_day", weeklyWindow},
}

// alwaysReported are the windows every fetch reports, observed or not.
var alwaysReported = []string{"five_hour", "seven_day"}

// Absent keys of the usage window fields, named as catalog.Record.Absent
// names a nested field.
const (
	absentWindowKey = "usage_window"
	absentUsedKey   = "usage_window.used_percent"
	absentBurnKey   = "usage_window.burn_percent_per_hour"
	absentResetKey  = "usage_window.resets_at"
)

// maxCeilingWindows bounds how many past windows with a rejection calibrate a ceiling.
const maxCeilingWindows = 64

type windowSpec struct {
	limitType string
	length    time.Duration
	family    string
	known     bool
}

func specFor(limitType string) windowSpec {
	spec := windowSpec{limitType: limitType}
	for _, kind := range windowKinds {
		if limitType == kind.prefix || strings.HasPrefix(limitType, kind.prefix+"_") {
			spec.length, spec.known = kind.length, true
			spec.family = strings.TrimPrefix(strings.TrimPrefix(limitType, kind.prefix), "_")
			return spec
		}
	}
	return spec
}

// records returns one record per reported window, in a stable order.
func (s *logScan) records() []catalog.Record {
	sort.Slice(s.usage, func(i, j int) bool { return s.usage[i].at.Before(s.usage[j].at) })
	types := append([]string(nil), alwaysReported...)
	known := map[string]bool{"five_hour": true, "seven_day": true}
	for i := 0; i < len(s.rejections) && i < maxRejections; i++ {
		if limitType := s.rejections[i].limitType; !known[limitType] {
			known[limitType] = true
			types = append(types, limitType)
		}
	}
	sort.Strings(types[len(alwaysReported):])
	records := make([]catalog.Record, 0, len(types))
	for _, limitType := range types {
		records = append(records, s.record(specFor(limitType)))
	}
	return records
}

func (s *logScan) record(spec windowSpec) catalog.Record {
	record := baseRecord(spec.limitType, s.now)
	window := &catalog.UsageWindow{}
	record.UsageWindow = window
	if !spec.known {
		record.Absent[absentWindowKey] = "window length of rateLimitType " + spec.limitType + " is not known to this source"
		return record
	}
	minutes := int(spec.length / time.Minute)
	window.WindowMinutes = &minutes
	agents := s.activeAgents(spec.family, s.now.Add(-burnWindow), s.now)
	window.ActiveAgents = &agents
	hits := s.hitsFor(spec.limitType)
	ceiling, reason := s.calibrate(spec, hits)
	if reason != "" {
		record.Absent[absentUsedKey], record.Absent[absentBurnKey] = reason, reason
		s.addLastReset(record, window, spec, hits)
		return record
	}
	reset := currentReset(ceiling.WindowReset, spec.length, s.now)
	window.ResetsAt = &reset
	used, ok := s.sum(spec.family, reset.Add(-spec.length), s.now)
	if !ok {
		reason := "usage of a model without rates falls in the current window"
		record.Absent[absentUsedKey], record.Absent[absentBurnKey] = reason, reason
		return record
	}
	usedPercent := used / ceiling.Ceiling * 100
	window.UsedPercent = &usedPercent
	s.addBurn(record, window, spec.family, ceiling.Ceiling)
	return record
}

// addLastReset sets the reset time from the newest rejection of the window
// when no ceiling is known; without any rejection it stays absent.
func (s *logScan) addLastReset(record catalog.Record, window *catalog.UsageWindow, spec windowSpec, hits []rejection) {
	if len(hits) == 0 || hits[len(hits)-1].resetsAt.IsZero() {
		record.Absent[absentResetKey] = "no rejected " + spec.limitType + " request in the last 7 days names a reset time"
		return
	}
	reset := currentReset(hits[len(hits)-1].resetsAt, spec.length, s.now)
	window.ResetsAt = &reset
}

// addBurn sets the sliding-window burn.
func (s *logScan) addBurn(record catalog.Record, window *catalog.UsageWindow, family string, ceiling float64) {
	burn, ok := s.sum(family, s.now.Add(-burnWindow), s.now)
	if !ok {
		record.Absent[absentBurnKey] = "usage of a model without rates falls in the burn window"
		return
	}
	perHour := burn / ceiling * 100 * float64(time.Hour/burnWindow)
	window.BurnPercentPerHour = &perHour
}

func baseRecord(limitType string, now time.Time) catalog.Record {
	return catalog.Record{
		ModelID:    "claude-code/" + limitType,
		Provider:   "anthropic",
		AccessPath: catalog.AccessSubscriptionCLI,
		Provenance: catalog.Provenance{Source: SourceName, FetchedAt: now, Kind: catalog.KindMeasured},
		Absent: map[string]string{
			"context_window":  "not reported by Claude Code session logs",
			"price_in_per_m":  "subscription plan, not metered pricing",
			"price_out_per_m": "subscription plan, not metered pricing",
			"limits":          "the logs report rejected requests, not rpm/tpm caps",
		},
	}
}

// hitsFor returns the rejections of one window type, oldest first.
func (s *logScan) hitsFor(limitType string) []rejection {
	var hits []rejection
	for i := 0; i < len(s.rejections) && i < maxRejections; i++ {
		if s.rejections[i].limitType == limitType {
			hits = append(hits, s.rejections[i])
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].at.Before(hits[j].at) })
	return hits
}

// currentReset advances the last reported reset in whole window steps until
// it lies after now.
func currentReset(last time.Time, length time.Duration, now time.Time) time.Time {
	if last.After(now) {
		return last
	}
	steps := now.Sub(last)/length + 1
	return last.Add(steps * length)
}

// calibrate returns the window's ceiling: the one the logs show, or the
// stored one when it comes from a later rejection or the logs show none. A
// ceiling whose rejection is older than the maximum age is unknown. A newer
// ceiling from the logs replaces the stored one. WindowReset is the newest
// reset reported for the window, the anchor the current reset advances from.
func (s *logScan) calibrate(spec windowSpec, hits []rejection) (storedCeiling, string) {
	observed, found := s.observedCeiling(spec, hits)
	stored, haveStored := s.storedCeiling(spec.limitType)
	choice := stored
	switch {
	case found && (!haveStored || observed.RejectedAt.After(stored.RejectedAt)):
		choice = observed
		s.remember(spec.limitType, observed)
	case !haveStored:
		return storedCeiling{}, fmt.Sprintf("no rejected %s request with fully logged, priced usage in the last 7 days and none stored, so the ceiling is unknown", spec.limitType)
	}
	if age := s.now.Sub(choice.RejectedAt); age > s.maxAge {
		return storedCeiling{}, fmt.Sprintf("the %s ceiling comes from a rejection on %s, older than %s, so it is unknown again",
			spec.limitType, choice.RejectedAt.UTC().Format(time.RFC3339), s.maxAge)
	}
	if len(hits) > 0 && hits[len(hits)-1].resetsAt.After(choice.WindowReset) {
		choice.WindowReset = hits[len(hits)-1].resetsAt
	}
	return choice, ""
}

func (s *logScan) storedCeiling(limitType string) (storedCeiling, bool) {
	if s.store == nil {
		return storedCeiling{}, false
	}
	entry, ok := s.store.Ceilings[limitType]
	return entry, ok
}

// remember puts a ceiling observed in the logs into the store, when one is kept.
func (s *logScan) remember(limitType string, observed storedCeiling) {
	if s.store == nil {
		return
	}
	observed.RecordedAt = s.now
	s.store.Ceilings[limitType] = observed
	s.storeChanged = true
}

// observedCeiling is the smallest cost-equivalent a window held at its first
// rejection, over the windows whose usage the scanned logs hold completely,
// with the rejection that set it.
func (s *logScan) observedCeiling(spec windowSpec, hits []rejection) (storedCeiling, bool) {
	first, resets := firstRejections(hits)
	var best storedCeiling
	for i := 0; i < len(resets) && i < maxCeilingWindows; i++ {
		hit := first[resets[i]]
		held := s.heldAtRejection(spec, resets[i], hit.at)
		if held > 0 && (best.Ceiling == 0 || held < best.Ceiling) {
			best = storedCeiling{Ceiling: held, RejectedAt: hit.at, SourceFile: hit.file, WindowReset: hit.resetsAt}
		}
	}
	return best, best.Ceiling > 0
}

// firstRejections maps each reported reset to its window's earliest
// rejection and lists the resets newest first.
func firstRejections(hits []rejection) (map[time.Time]rejection, []time.Time) {
	first := make(map[time.Time]rejection)
	var resets []time.Time
	for _, hit := range hits {
		earliest, ok := first[hit.resetsAt]
		if !ok {
			resets = append(resets, hit.resetsAt)
		}
		if !ok || hit.at.Before(earliest.at) {
			first[hit.resetsAt] = hit
		}
	}
	sort.Slice(resets, func(i, j int) bool { return resets[i].After(resets[j]) })
	return first, resets
}

// heldAtRejection is the usage a window held when its first request was
// rejected; 0 when the window started before the scanned logs or holds
// unpriced usage.
func (s *logScan) heldAtRejection(spec windowSpec, reset, rejectedAt time.Time) float64 {
	start := reset.Add(-spec.length)
	if start.Before(s.now.Add(-weeklyWindow)) || rejectedAt.Before(start) {
		return 0
	}
	held, ok := s.sum(spec.family, start, rejectedAt)
	if !ok {
		return 0
	}
	return held
}

// sum adds the cost of the family's usage in [from, to]; ok is false when
// unpriced usage falls in the range. s.usage is sorted by time.
func (s *logScan) sum(family string, from, to time.Time) (total float64, ok bool) {
	i := sort.Search(len(s.usage), func(k int) bool { return !s.usage[k].at.Before(from) })
	for ; i < len(s.usage) && !s.usage[i].at.After(to); i++ {
		event := s.usage[i]
		if !inFamily(event.model, family) {
			continue
		}
		if event.unpriced {
			return 0, false
		}
		total += event.cost
	}
	return total, true
}

func (s *logScan) activeAgents(family string, from, to time.Time) int {
	agents := make(map[string]bool)
	i := sort.Search(len(s.usage), func(k int) bool { return !s.usage[k].at.Before(from) })
	for ; i < len(s.usage) && !s.usage[i].at.After(to); i++ {
		if inFamily(s.usage[i].model, family) {
			agents[s.usage[i].agent] = true
		}
	}
	return len(agents)
}

// inFamily reports whether a model id names the family (claude-opus-5-5 is
// in opus); the empty family holds every model.
func inFamily(model, family string) bool {
	return family == "" || strings.Contains(model, "-"+family+"-") || strings.HasSuffix(model, "-"+family)
}
