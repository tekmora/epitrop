// Command epitrop-server serves the run record over the Model Context Protocol, on stdio
// or over HTTP on a loopback address. It serves the read-only tools, and with
// server.allow_mutations the tools that start and stop jobs and record unit status; see
// internal/mcp. Like epitropctl it answers to the job shim and the egress relay, which the
// supervisor runs as this executable.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/broker"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/egressproxy"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/jobfailure"
	"github.com/tekmora/epitrop/internal/logsetup"
	"github.com/tekmora/epitrop/internal/mcp"
	"github.com/tekmora/epitrop/internal/releasewatch"
	"github.com/tekmora/epitrop/internal/supervisor"
)

// version is set at link time.
var version string

const (
	configLoadTimeout = 30 * time.Second
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
	// watchInterval is how often the HTTP transport replays the event log for readiness
	// and the event stream.
	watchInterval = 2 * time.Second
)

// newSupervisor and newEffectsLedger open the supervisor and the effect ledger; tests
// replace them.
var (
	newSupervisor    = supervisor.New
	newEffectsLedger = func(cfg config.Config) (mcp.EffectRunner, error) { return logsetup.EffectsLedger(cfg) }
)

// jobShimCommand is the argument the supervisor starts its job shim with.
const jobShimCommand = "__job-shim"

func main() {
	if code, ok := helper(os.Args[1:]); ok {
		os.Exit(code)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := serve(ctx, os.Args[1:], os.Stdin, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// helper runs the job shim or the egress relay when args name one, and reports whether
// they did. The supervisor starts both as this executable.
func helper(args []string) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case jobShimCommand:
		return supervisor.RunShim(args), true
	case egressproxy.RelayCommand:
		return egressproxy.RunRelay(args, os.Stderr), true
	}
	return 0, false
}

// serve loads the config named by args and serves the tools on the configured transport
// until the client disconnects or ctx ends. On stdio nothing but protocol frames is
// written to out; on HTTP out gets the address served.
func serve(ctx context.Context, args []string, in io.ReadCloser, out io.WriteCloser) error {
	cfg, src, err := load(args)
	if err != nil {
		return err
	}
	w, err := openFailures(cfg)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	go w.Run(ctx, watchInterval, logger)
	if cfg.Server.Transport == config.TransportHTTP {
		return serveHTTP(ctx, cfg.Server.Listen, src, out)
	}
	err = mcp.NewServer(appVersion(), src).Run(ctx, &sdkmcp.IOTransport{Reader: in, Writer: out})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("epitrop-server: %w", err)
	}
	return nil
}

// serveHTTP serves the tools, probes and event stream on addr until ctx ends, then stops
// within shutdownTimeout. Request contexts end with ctx, so open streams close, and
// connections that have sent no request are closed, so neither holds the stop.
func serveHTTP(ctx context.Context, addr string, src mcp.Sources, out io.Writer) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("epitrop-server: %w", err)
	}
	h := mcp.NewHTTP(appVersion(), src, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	idle := newNewConns()
	srv := &http.Server{
		Handler:           h.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ConnState:         idle.track,
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	go h.Watch(ctx, watchInterval)
	if _, err := fmt.Fprintf(out, "epitrop-server: serving http://%s%s\n", ln.Addr(), mcp.PathMCP); err != nil {
		return errors.Join(fmt.Errorf("epitrop-server: %w", err), srv.Close())
	}
	select {
	case err := <-served:
		return fmt.Errorf("epitrop-server: %w", err)
	case <-ctx.Done():
	}
	// Closing the silent connections first lets Shutdown return once the requests in
	// flight are done; connections accepted until Shutdown closes the listener are
	// closed as they arrive.
	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	idle.closeAll()
	if err := srv.Shutdown(stopCtx); err != nil {
		return errors.Join(fmt.Errorf("epitrop-server: shutdown: %w", err), srv.Close(), idle.closeErr())
	}
	return idle.closeErr()
}

// load parses args, loads the config and opens what the tools read.
func load(args []string) (config.Config, mcp.Sources, error) {
	fs := flag.NewFlagSet("epitrop-server", flag.ContinueOnError)
	configPath := fs.String("config", "", "configuration file")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, mcp.Sources{}, err
	}
	if *configPath == "" {
		return config.Config{}, mcp.Sources{}, errors.New("epitrop-server: --config is required")
	}
	if fs.NArg() > 0 {
		return config.Config{}, mcp.Sources{}, fmt.Errorf("epitrop-server: unexpected argument %q", fs.Arg(0))
	}
	ctx, cancel := context.WithTimeout(context.Background(), configLoadTimeout)
	defer cancel()
	cfg, err := config.Load(ctx, *configPath)
	if err != nil {
		return config.Config{}, mcp.Sources{}, err
	}
	src, err := sources(cfg)
	if err != nil {
		return config.Config{}, mcp.Sources{}, err
	}
	return cfg, src, nil
}

const tailCapacity = 256

// sources opens what the tools read, and with server.allow_mutations what they change and
// the effect ledger that runs each call at most once.
func sources(cfg config.Config) (mcp.Sources, error) {
	unitsSvc, err := logsetup.UnitsService(cfg)
	if err != nil {
		return mcp.Sources{}, err
	}
	jobs, err := openJobs(cfg)
	if err != nil {
		return mcp.Sources{}, err
	}
	tail, err := openTail(cfg)
	if err != nil {
		return mcp.Sources{}, err
	}
	src := mcp.Sources{
		Jobs:    jobs,
		Units:   unitsSvc,
		Watches: releasewatch.NewWatches(cfg.ReleaseWatch),
		Events:  tail,
	}
	if !cfg.Server.AllowMutations {
		return src, nil
	}
	ledger, err := newEffectsLedger(cfg)
	if err != nil {
		return mcp.Sources{}, err
	}
	src.Mutate = &mcp.Mutators{Jobs: jobs, Units: unitsSvc, Effects: ledger}
	return src, nil
}

func openTail(cfg config.Config) (*broker.Tail, error) {
	signer, clk, err := logsetup.Signer(cfg)
	if err != nil {
		return nil, err
	}
	limits := logsetup.Limits(cfg, clk)
	pub := signer.PublicKey()
	b, err := broker.New(tailCapacity)
	if err != nil {
		return nil, fmt.Errorf("epitrop-server: %w", err)
	}
	tail, err := broker.NewTail(b,
		func(ctx context.Context, after uint64) ([]eventlog.Record, error) {
			return eventlog.ReplayAfter(ctx, cfg.EventLog.Dir, pub, limits, after)
		},
		func(ctx context.Context) (uint64, error) {
			return eventlog.HeadSeq(ctx, cfg.EventLog.Dir, pub)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("epitrop-server: %w", err)
	}
	return tail, nil
}

// openFailures opens the job failure watcher that routes failed job exits to dead-letter classes.
func openFailures(cfg config.Config) (*jobfailure.Watcher, error) {
	dl, err := broker.NewDeadLetters(cfg.Broker.DeadLetter.Specs(), cfg.Broker.DeadLetter.Fallback, cfg.Broker.DeadLetter.Capacity)
	if err != nil {
		return nil, fmt.Errorf("epitrop-server: %w", err)
	}
	signer, clk, err := logsetup.Signer(cfg)
	if err != nil {
		return nil, err
	}
	limits := logsetup.Limits(cfg, clk)
	pub := signer.PublicKey()

	replay := func(ctx context.Context, after uint64) ([]eventlog.Record, error) {
		return eventlog.ReplayAfter(ctx, cfg.EventLog.Dir, pub, limits, after)
	}
	head := func(ctx context.Context) (uint64, error) {
		return eventlog.HeadSeq(ctx, cfg.EventLog.Dir, pub)
	}
	appendFn := func(ctx context.Context, check func(context.Context) (eventlog.Event, error)) (eventlog.Record, error) {
		writer, wErr := eventlog.Open(cfg.EventLog.Dir, signer, limits)
		if wErr != nil {
			return eventlog.Record{}, fmt.Errorf("epitrop-server: open event log: %w", wErr)
		}
		return writer.AppendChecked(ctx, check)
	}
	jobLogs := make(map[string]string, len(cfg.Jobs))
	for _, j := range cfg.Jobs {
		jobLogs[j.Name] = j.LogPath
	}
	output := func(_ context.Context, job string) (string, error) {
		path := jobLogs[job]
		if path == "" {
			return "", nil
		}
		return readLogTail(path)
	}

	w, err := jobfailure.New(dl, replay, head, appendFn, output)
	if err != nil {
		return nil, fmt.Errorf("epitrop-server: %w", err)
	}
	return w, nil
}

// readLogTail reads at most the last MaxOutputBytes of path.
func readLogTail(path string) (tail string, err error) {
	// #nosec G304 -- path comes from declared job configuration.
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("epitrop-server: open %s: %w", path, err)
	}
	defer func() {
		if cErr := f.Close(); cErr != nil {
			err = errors.Join(err, fmt.Errorf("epitrop-server: close %s: %w", path, cErr))
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("epitrop-server: stat %s: %w", path, err)
	}
	size := info.Size()
	var offset int64
	if size > jobfailure.MaxOutputBytes {
		offset = size - jobfailure.MaxOutputBytes
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", fmt.Errorf("epitrop-server: seek %s: %w", path, err)
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, jobfailure.MaxOutputBytes))
	if err != nil {
		return "", fmt.Errorf("epitrop-server: read %s: %w", path, err)
	}
	return string(b), nil
}

// jobService reads the jobs' state and, for the mutating tools, starts and stops them.
type jobService interface {
	mcp.JobLister
	mcp.JobControl
}

// openJobs opens the supervisor. It starts a job through the shim this executable answers
// to. Where the platform has no supervisor, the jobs tools answer with the reason instead
// of the server refusing to start; any other error does.
func openJobs(cfg config.Config) (jobService, error) {
	sup, err := newSupervisor(cfg, supervisor.Options{})
	if errors.Is(err, supervisor.ErrNotSupported) {
		return unavailableJobs{err: err}, nil
	}
	if err != nil {
		return nil, err
	}
	return sup, nil
}

// unavailableJobs answers every request with why jobs cannot be read or run.
type unavailableJobs struct{ err error }

func (u unavailableJobs) Status(context.Context, string) ([]supervisor.JobStatus, error) {
	return nil, u.err
}

func (u unavailableJobs) Start(context.Context, string) error { return u.err }

func (u unavailableJobs) Stop(context.Context, string) error { return u.err }

func appVersion() string {
	if version != "" {
		return version
	}
	return "dev"
}
