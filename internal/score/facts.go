package score

import "github.com/tekmora/epitrop/internal/graphlog"

// Facts folds what the log says about each node.
type Facts = graphlog.RankFacts

// NewFacts returns an empty Facts store ready to fold event-log records.
func NewFacts() *Facts {
	return graphlog.NewRankFacts()
}
