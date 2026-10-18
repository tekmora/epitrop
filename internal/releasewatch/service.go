package releasewatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

const (
	maxWatchLoopIterations = 1000000
	// passTimeout bounds one pass: every source, issue search and delivery it makes.
	passTimeout = 10 * time.Minute
)

type Service struct {
	cfg    config.Config
	client *Client
	state  *State
}

func NewService(cfg config.Config, clientOpts ClientOptions) (*Service, error) {
	stateDir := cfg.ReleaseWatch.StateDir
	if stateDir == "" && len(cfg.ReleaseWatch.Routes) > 0 {
		return nil, errors.New("state_dir is required when routes are configured")
	}
	var state *State
	var err error
	if stateDir != "" {
		state, err = LoadState(stateDir)
		if err != nil {
			return nil, fmt.Errorf("load state: %w", err)
		}
	} else {
		state = &State{seen: make(map[string]string)}
	}

	if clientOpts.RateLimitFloor <= 0 {
		clientOpts.RateLimitFloor = cfg.ReleaseWatch.RateLimitFloor
	}
	client, err := NewClient(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	return &Service{
		cfg:    cfg,
		client: client,
		state:  state,
	}, nil
}

func (s *Service) State() *State {
	return s.state
}

// Run makes one pass with once, else a pass every interval until ctx ends. A supervisor
// stop cancels ctx; that ends the loop cleanly and is not an error. Reaching ctx's runtime
// bound is an error, so a restart policy starts a fresh process. A failed pass is logged
// and the loop goes on: upstream outages are expected to pass.
func (s *Service) Run(ctx context.Context, once bool, interval time.Duration) error {
	if once {
		return s.timedPass(ctx)
	}
	if interval <= 0 {
		interval = time.Hour
	}
	for iter := 0; iter < maxWatchLoopIterations; iter++ {
		if err := s.timedPass(ctx); err != nil {
			s.client.Logf("release-watch: pass error: %v\n", err)
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("release-watch: runtime bound reached: %w", ctx.Err())
			}
			return nil
		case <-time.After(interval):
		}
	}
	return nil
}

func (s *Service) timedPass(ctx context.Context) error {
	passCtx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	return s.RunPass(passCtx)
}

// passTally counts what one pass did, for its summary line and its error.
type passTally struct {
	polled, failedPolls, newItems, filed, deferred int
	failed                                         []string
}

// RunPass makes one pass over every route and ends it with one summary line, so a pass
// that finds nothing new still leaves a line in the job log (#271). The rate-limit
// throttle belongs to one pass: a later pass asks GitHub again.
func (s *Service) RunPass(ctx context.Context) error {
	s.client.ResetThrottle()
	var tally passTally
	maxActions := s.cfg.ReleaseWatch.MaxActionsPerRun
	if maxActions <= 0 {
		maxActions = config.DefaultMaxActionsPerRun
	}

	throttled := false
	for r := 0; r < len(s.cfg.ReleaseWatch.Routes); r++ {
		err := s.processRoute(ctx, s.cfg.ReleaseWatch.Routes[r], &tally, maxActions)
		if err != nil && errors.Is(err, ErrThrottled) {
			throttled = true
			break
		}
	}
	s.logPassSummary(tally, throttled)

	if len(tally.failed) > 0 {
		return fmt.Errorf("release-watch pass failed: %s", strings.Join(tally.failed, ", "))
	}
	return nil
}

func (s *Service) logPassSummary(tally passTally, throttled bool) {
	end := ""
	if throttled {
		end = "; stopped early by the rate-limit throttle"
	}
	s.client.Logf("release-watch: pass done: sources %d polled, %d failed; releases %d new, %d filed, %d deferred%s\n",
		tally.polled, tally.failedPolls, tally.newItems, tally.filed, tally.deferred, end)
}

func (s *Service) processRoute(ctx context.Context, route config.ReleaseWatchRoute, tally *passTally, maxActions int) error {
	for i := 0; i < len(route.Sources); i++ {
		src := route.Sources[i]
		sourceID := sourceIdentifier(src)
		items, err := FetchSource(ctx, s.client, src)
		if err != nil {
			if errors.Is(err, ErrThrottled) {
				return ErrThrottled
			}
			s.client.Logf("release-watch: source %s failed: %v\n", sourceID, err)
			tally.polled++
			tally.failedPolls++
			tally.failed = append(tally.failed, sourceID)
			continue
		}
		tally.polled++
		if err = s.processSourceItems(ctx, route.Sinks, items, tally, maxActions); err != nil {
			if errors.Is(err, ErrThrottled) {
				return ErrThrottled
			}
		}
	}
	return nil
}

func (s *Service) processSourceItems(ctx context.Context, sinks []config.ReleaseWatchSink, items []ReleaseItem, tally *passTally, maxActions int) error {
	unseen := s.filterAndOrderUnseen(items)
	tally.newItems += len(unseen)
	perCap := s.cfg.ReleaseWatch.PerSourceCap
	if perCap <= 0 {
		perCap = config.DefaultPerSourceCap
	}
	if len(unseen) > perCap {
		for i := perCap; i < len(unseen); i++ {
			s.client.Logf("release-watch: deferred %s (cap)\n", unseen[i].ID)
		}
		tally.deferred += len(unseen) - perCap
		unseen = unseen[:perCap]
	}

	for i := 0; i < len(unseen); i++ {
		item := unseen[i]
		if tally.filed >= maxActions {
			s.client.Logf("release-watch: deferred %s (cap)\n", item.ID)
			tally.deferred++
			continue
		}
		filed, err := s.deliverItem(ctx, sinks, item)
		if err != nil {
			if errors.Is(err, ErrThrottled) {
				return ErrThrottled
			}
			s.client.Logf("release-watch: delivery %s failed: %v\n", item.ID, err)
			tally.failed = append(tally.failed, fmt.Sprintf("delivery:%s", item.ID))
			continue
		}
		if filed {
			tally.filed++
		}
	}
	return nil
}

func (s *Service) filterAndOrderUnseen(items []ReleaseItem) []ReleaseItem {
	unseen := make([]ReleaseItem, 0, len(items))
	for i := 0; i < len(items); i++ {
		if !s.state.Has(items[i].ID) {
			unseen = append(unseen, items[i])
		}
	}
	// Items arrive newest first from API; reverse to oldest first
	for i, j := 0, len(unseen)-1; i < j; i, j = i+1, j-1 {
		unseen[i], unseen[j] = unseen[j], unseen[i]
	}
	return unseen
}

func (s *Service) deliverItem(ctx context.Context, sinks []config.ReleaseWatchSink, item ReleaseItem) (bool, error) {
	anyFiled := false
	for i := 0; i < len(sinks); i++ {
		filed, err := s.deliverToSink(ctx, sinks[i], item)
		if err != nil {
			return false, err
		}
		anyFiled = anyFiled || filed
	}
	return anyFiled, nil
}

// deliverToSink delivers the item to the one sink kind the entry names and reports whether
// it filed anything.
func (s *Service) deliverToSink(ctx context.Context, sink config.ReleaseWatchSink, item ReleaseItem) (bool, error) {
	filed := false
	if sink.GitHubIssue != nil {
		delivered, err := DeliverGitHubIssue(ctx, s.client, s.state, sink.GitHubIssue, item)
		if err != nil {
			return false, err
		}
		filed = delivered
	}
	if sink.Ntfy != nil {
		delivered, err := DeliverNtfy(ctx, s.client, s.state, sink.Ntfy, item)
		if err != nil {
			return false, err
		}
		filed = filed || delivered
	}
	return filed, nil
}

func (s *Service) Seed(ctx context.Context) error {
	var failedSources []string
	for r := 0; r < len(s.cfg.ReleaseWatch.Routes); r++ {
		route := s.cfg.ReleaseWatch.Routes[r]
		for i := 0; i < len(route.Sources); i++ {
			src := route.Sources[i]
			sourceID := sourceIdentifier(src)
			items, err := FetchSource(ctx, s.client, src)
			if err != nil {
				s.client.Logf("release-watch: source %s failed: %v\n", sourceID, err)
				failedSources = append(failedSources, sourceID)
				continue
			}
			for j := 0; j < len(items); j++ {
				if !s.state.Has(items[j].ID) {
					s.state.MarkSeen(items[j].ID)
					s.client.Logf("release-watch: seeded %s\n", items[j].ID)
				}
			}
		}
	}
	if err := s.state.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if len(failedSources) > 0 {
		return fmt.Errorf("release-watch seed failed: %s", strings.Join(failedSources, ", "))
	}
	return nil
}

func sourceIdentifier(src config.ReleaseWatchSource) string {
	if src.GitHub != "" {
		return fmt.Sprintf("github:%s", src.GitHub)
	}
	return fmt.Sprintf("hf:%s", src.HuggingFaceOrg)
}
