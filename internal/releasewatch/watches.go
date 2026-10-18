package releasewatch

import (
	"context"
	"fmt"

	"github.com/tekmora/epitrop/internal/config"
)

// Watches reads the configured routes and the seen-set as they are on disk. It changes
// nothing: a reader of the watches never polls a source or acts on a sink.
type Watches struct {
	cfg config.ReleaseWatchConfig
}

// NewWatches returns the reader over a loaded configuration's release watches.
func NewWatches(cfg config.ReleaseWatchConfig) Watches {
	return Watches{cfg: cfg}
}

// Routes returns the configured routes.
func (w Watches) Routes() []config.ReleaseWatchRoute {
	return w.cfg.Routes
}

// Seen returns the seen-set, release id to the time it was first seen (RFC 3339, UTC). A
// configuration without a state directory has seen nothing.
func (w Watches) Seen(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("release-watch: read seen-set: %w", err)
	}
	if w.cfg.StateDir == "" {
		return map[string]string{}, nil
	}
	state, err := LoadState(w.cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("release-watch: %w", err)
	}
	return state.Seen(), nil
}

// Seen returns a copy of the seen-set.
func (s *State) Seen() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.seen))
	for id, at := range s.seen {
		out[id] = at
	}
	return out
}

// SourceKind names the kind of a configured source: "github" or "huggingface".
func SourceKind(src config.ReleaseWatchSource) string {
	if src.GitHub != "" {
		return "github"
	}
	return "huggingface"
}

// SourceName is the repository or organization a source watches.
func SourceName(src config.ReleaseWatchSource) string {
	if src.GitHub != "" {
		return src.GitHub
	}
	return src.HuggingFaceOrg
}

// SeenPrefix is the prefix every release id of src starts with in the seen-set; the ids are
// made in sources.go.
func SeenPrefix(src config.ReleaseWatchSource) string {
	if src.GitHub != "" {
		return "github:" + src.GitHub + "@"
	}
	return "hf:" + src.HuggingFaceOrg + "/"
}
