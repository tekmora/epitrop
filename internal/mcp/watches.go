package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/releasewatch"
)

// Tool names of the release watches; a client sees them prefixed with the server's name.
const (
	ToolWatchesList = "watches_list"
	ToolWatchStatus = "watch_status"
)

// WatchReader reads the release watches; releasewatch.Watches is one.
type WatchReader interface {
	Routes() []config.ReleaseWatchRoute
	Seen(ctx context.Context) (map[string]string, error)
}

// errNoWatches answers a watch tool on a server that was given no release watches.
var errNoWatches = errors.New("the server reads no release watches")

// registerWatches adds the read-only release watch tools over w to s.
func registerWatches(s *sdkmcp.Server, w WatchReader) {
	sdkmcp.AddTool(s, readOnly(ToolWatchesList, "List the configured release watch routes in configuration order, each with its sources as kind:name and the kinds of its sinks."), watchesList(w))
	sdkmcp.AddTool(s, readOnly(ToolWatchStatus, "Show one release watch route by name: its sources with how many releases each has seen and the release it saw last, and its sinks. An ntfy sink's topic is left out."), watchStatus(w))
}

// WatchesListIn takes no arguments.
type WatchesListIn struct{}

// WatchSummary is one route in a list.
type WatchSummary struct {
	Name    string   `json:"name"`
	Sources []string `json:"sources"`
	Sinks   []string `json:"sinks"`
}

// WatchesListOut lists the routes.
type WatchesListOut struct {
	Watches []WatchSummary `json:"watches"`
}

func watchesList(w WatchReader) sdkmcp.ToolHandlerFor[WatchesListIn, WatchesListOut] {
	return func(_ context.Context, _ *sdkmcp.CallToolRequest, _ WatchesListIn) (*sdkmcp.CallToolResult, WatchesListOut, error) {
		if w == nil {
			return nil, WatchesListOut{}, fmt.Errorf("%s: %w", ToolWatchesList, errNoWatches)
		}
		routes := w.Routes()
		out := WatchesListOut{Watches: make([]WatchSummary, 0, len(routes))}
		for i := 0; i < len(routes); i++ {
			out.Watches = append(out.Watches, summarizeWatch(routes[i]))
		}
		return nil, out, nil
	}
}

func summarizeWatch(route config.ReleaseWatchRoute) WatchSummary {
	sum := WatchSummary{Name: route.Name, Sources: make([]string, 0, len(route.Sources)), Sinks: make([]string, 0, len(route.Sinks))}
	for i := 0; i < len(route.Sources); i++ {
		src := route.Sources[i]
		sum.Sources = append(sum.Sources, releasewatch.SourceKind(src)+":"+releasewatch.SourceName(src))
	}
	for i := 0; i < len(route.Sinks); i++ {
		sum.Sinks = append(sum.Sinks, sinkOf(route.Sinks[i]).Kind)
	}
	return sum
}

// WatchStatusIn names the route.
type WatchStatusIn struct {
	Name string `json:"name" jsonschema:"route name"`
}

// WatchSource is one source of a route. Seen counts its releases in the seen-set; LastSeen
// is the one it saw last and LastSeenAt when, both empty before the first.
type WatchSource struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Seen       int    `json:"seen"`
	LastSeen   string `json:"last_seen,omitempty"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

// WatchSink is one sink of a route. A GitHub issue sink names its repository and labels;
// an ntfy sink only its kind, since its topic is all a reader needs to read it.
type WatchSink struct {
	Kind   string   `json:"kind"`
	Repo   string   `json:"repo,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// WatchStatusOut is one route.
type WatchStatusOut struct {
	Name    string        `json:"name"`
	Sources []WatchSource `json:"sources"`
	Sinks   []WatchSink   `json:"sinks"`
}

func watchStatus(w WatchReader) sdkmcp.ToolHandlerFor[WatchStatusIn, WatchStatusOut] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in WatchStatusIn) (*sdkmcp.CallToolResult, WatchStatusOut, error) {
		if w == nil {
			return nil, WatchStatusOut{}, fmt.Errorf("%s: %w", ToolWatchStatus, errNoWatches)
		}
		route, ok := routeNamed(w.Routes(), in.Name)
		if !ok {
			return nil, WatchStatusOut{}, fmt.Errorf("%s: no release watch route %q", ToolWatchStatus, in.Name)
		}
		ctx, cancel := context.WithTimeout(ctx, CallTimeout)
		defer cancel()
		seen, err := w.Seen(ctx)
		if err != nil {
			return nil, WatchStatusOut{}, fmt.Errorf("%s: %w", ToolWatchStatus, err)
		}
		out := WatchStatusOut{Name: route.Name, Sources: make([]WatchSource, 0, len(route.Sources)), Sinks: make([]WatchSink, 0, len(route.Sinks))}
		for i := 0; i < len(route.Sources); i++ {
			out.Sources = append(out.Sources, sourceOf(route.Sources[i], seen))
		}
		for i := 0; i < len(route.Sinks); i++ {
			out.Sinks = append(out.Sinks, sinkOf(route.Sinks[i]))
		}
		return nil, out, nil
	}
}

func routeNamed(routes []config.ReleaseWatchRoute, name string) (config.ReleaseWatchRoute, bool) {
	for i := 0; i < len(routes); i++ {
		if routes[i].Name == name {
			return routes[i], true
		}
	}
	return config.ReleaseWatchRoute{}, false
}

// sourceOf counts the releases of src in seen. The last seen is the latest time; of two
// seen at one time, the greater id, so the answer does not depend on map order.
func sourceOf(src config.ReleaseWatchSource, seen map[string]string) WatchSource {
	out := WatchSource{Kind: releasewatch.SourceKind(src), Name: releasewatch.SourceName(src)}
	prefix := releasewatch.SeenPrefix(src)
	for id, at := range seen {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		out.Seen++
		if at > out.LastSeenAt || (at == out.LastSeenAt && id > out.LastSeen) {
			out.LastSeen, out.LastSeenAt = id, at
		}
	}
	return out
}

func sinkOf(sink config.ReleaseWatchSink) WatchSink {
	if sink.GitHubIssue != nil {
		return WatchSink{Kind: "github_issue", Repo: sink.GitHubIssue.Repo, Labels: sink.GitHubIssue.Labels}
	}
	return WatchSink{Kind: "ntfy"}
}
