package claudelocal

// Ported from cordanaLLM/praetor tribunus/internal/sources/claudelocal at 5d4acfb (EUPL-1.2).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// logLine decodes the top-level fields of one session log line. Message is
// kept raw and decoded into messageUsage only for usage lines, so a line whose
// message has another shape (an API error string) still yields its quotaLimits.
type logLine struct {
	Timestamp   time.Time       `json:"timestamp"`
	UUID        string          `json:"uuid"`
	RequestID   string          `json:"requestId"`
	SessionID   string          `json:"sessionId"`
	AgentID     string          `json:"agentId"`
	Message     json.RawMessage `json:"message"`
	QuotaLimits *quotaLimits    `json:"quotaLimits"`
}

// messageUsage names only the usage fields of a message; content is not a field.
type messageUsage struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage *usage `json:"usage"`
}

type usage struct {
	InputTokens              int64          `json:"input_tokens"`
	OutputTokens             int64          `json:"output_tokens"`
	CacheCreationInputTokens int64          `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64          `json:"cache_read_input_tokens"`
	CacheCreation            *cacheCreation `json:"cache_creation"`
	Speed                    string         `json:"speed"`
}

type cacheCreation struct {
	Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
}

type quotaLimits struct {
	Status        string `json:"status"`
	RateLimitType string `json:"rateLimitType"`
	ResetsAt      *int64 `json:"resetsAt"`
}

// usageEvent is one deduplicated assistant response. An unpriced one has no
// cost and makes every window it falls in unknown.
type usageEvent struct {
	at       time.Time
	model    string
	cost     float64
	agent    string
	unpriced bool
}

// rejection is one request the provider refused for a usage window, and the
// log file (relative to the projects folder) that recorded it.
type rejection struct {
	at        time.Time
	limitType string
	resetsAt  time.Time
	file      string
}

// logScan accumulates usage and rejections across every file of one fetch.
type logScan struct {
	now          time.Time
	currentFile  string
	store        *ceilingStore
	maxAge       time.Duration
	storeChanged bool
	files        int
	skippedLines int
	seen         map[string]bool
	usage        []usageEvent
	rejections   []rejection
	overflow     string
}

func newLogScan(now time.Time) *logScan {
	return &logScan{now: now, seen: make(map[string]bool), maxAge: DefaultMaxCeilingAge}
}

var (
	usageMarker = []byte(`"usage"`)
	quotaMarker = []byte(`"quotaLimits"`)
)

// line handles one session log line. Lines without a usage or quota marker
// are not decoded at all; malformed lines are skipped.
func (s *logScan) line(raw []byte) {
	hasUsage, hasQuota := bytes.Contains(raw, usageMarker), containsQuota(raw)
	if !hasUsage && !hasQuota {
		return
	}
	var entry logLine
	if json.Unmarshal(raw, &entry) != nil || entry.Timestamp.IsZero() {
		return
	}
	if entry.QuotaLimits != nil {
		s.addRejection(entry)
	}
	if hasUsage && len(entry.Message) > 0 {
		s.addUsage(entry)
	}
}

func containsQuota(raw []byte) bool {
	return bytes.Contains(raw, quotaMarker)
}

func (s *logScan) addRejection(entry logLine) {
	quota := entry.QuotaLimits
	if quota.Status != "rejected" || quota.RateLimitType == "" || quota.ResetsAt == nil {
		return
	}
	if len(s.rejections) >= maxRejections {
		s.overflow = fmt.Sprintf("more than %d rejected requests", maxRejections)
		return
	}
	s.rejections = append(s.rejections, rejection{at: entry.Timestamp.UTC(), limitType: quota.RateLimitType,
		resetsAt: time.Unix(*quota.ResetsAt, 0).UTC(), file: s.currentFile})
}

func (s *logScan) addUsage(entry logLine) {
	var message messageUsage
	if json.Unmarshal(entry.Message, &message) != nil || message.Usage == nil {
		return
	}
	key := dedupeKey(entry, message)
	if key != "" && s.seen[key] {
		return
	}
	cost, priced := costOf(message.Model, *message.Usage)
	if priced && cost == 0 {
		return
	}
	if len(s.usage) >= maxUsageEvents {
		s.overflow = fmt.Sprintf("more than %d usage entries", maxUsageEvents)
		return
	}
	if key != "" {
		s.seen[key] = true
	}
	s.usage = append(s.usage, usageEvent{at: entry.Timestamp.UTC(), model: message.Model, cost: cost,
		agent: entry.SessionID + "/" + entry.AgentID, unpriced: !priced})
}

// dedupeKey identifies one API response. Claude Code writes one line per
// content block of a response, each with the same usage, so the message id
// and request id together count it once.
func dedupeKey(entry logLine, message messageUsage) string {
	if message.ID != "" || entry.RequestID != "" {
		return message.ID + "|" + entry.RequestID
	}
	return entry.UUID
}
