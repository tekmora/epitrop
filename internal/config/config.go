package config

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	coreconfig "github.com/golusoris/golusoris/core/config"
	golusorisschema "github.com/golusoris/golusoris/jsonschema"
	"github.com/tekmora/epitrop/internal/broker"
	"github.com/tekmora/epitrop/internal/egressproxy"
)

const (
	MaxGraphDepth           = 64
	MaxJobs                 = 64
	MaxJobArgs              = 32
	MaxWatches              = 64
	MaxRouterAliases        = 32
	MaxReplayRecords        = 1000000
	MaxJobRestarts          = 32
	MaxRestartBackoff       = 3600
	MaxStopGraceSeconds     = 300
	MaxJobRuntimeSeconds    = 365 * 24 * 3600
	MaxHeartbeatSeconds     = 24 * 3600
	MaxSandboxInputs        = 32
	MaxSandboxReadOnly      = 32
	MaxSandboxInputRoots    = 32
	MaxSandboxEnvAllow      = 64
	MaxReleaseWatchRoutes   = 32
	MaxReleaseWatchSources  = 16
	MaxReleaseWatchSinks    = 4
	MaxReleaseWatchLabels   = 8
	MinMaxActionsPerRun     = 1
	MaxMaxActionsPerRun     = 100
	MinPerSourceCap         = 1
	MaxPerSourceCap         = 20
	DefaultRateLimitFloor   = 100
	DefaultMaxActionsPerRun = 12
	DefaultPerSourceCap     = 3
	DefaultNtfyPriority     = 3
	maxConfigKeyDepth       = 8
	schemaResourceID        = "config.schema.json"
	defaultGraphDepth       = 12
	defaultTokens           = 200000
	defaultWallClockSec     = 3600
	defaultAlias            = "cordana/auto"
	defaultTimeoutSec       = 30
	defaultIntervalSec      = 5
	defaultReadyCount       = 2
	defaultEventLogDir      = ".epitrop"
	defaultLockSeconds      = 5
	defaultReplayMax        = 100000
	defaultEventLogRef      = "refs/epitrop/events"
	defaultGitSeconds       = 30
	defaultServerListen     = "127.0.0.1:8899"
	defaultBrokerQueue      = 64
	defaultFallbackClass    = "real-failure"
	defaultJobSchedule      = "always"
	defaultRestart          = "never"
	defaultStopSignal       = "TERM"
	defaultSandboxMode      = "enforce"
	defaultSandboxNet       = "none"
	defaultMemoryMax        = "2G"
	defaultCPUWeight        = 100
	defaultTasksMax         = 512
	minMemoryMaxBytes       = 64 * 1024 * 1024
	maxMemoryMaxBytes       = 64 * 1024 * 1024 * 1024
	minCPUWeight            = 1
	maxCPUWeight            = 10000
	minTasksMax             = 16
	maxTasksMax             = 32768
)

//go:embed config.schema.json
var schemaJSON []byte

var (
	sandboxEnvName   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	repoPattern      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	routeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	hfOrgPattern     = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	ntfyTopicPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// reservedSandboxEnv are names the sandbox sets (PATH, HOME, TMPDIR) or removes before the
// job starts (the systemd bus variables and INVOCATION_ID); allowlisting one would be
// overridden silently, so config refuses it.
var reservedSandboxEnv = []string{"PATH", "HOME", "TMPDIR", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "INVOCATION_ID"}

// proxySandboxEnv are the names a program reads its proxy from. With network=proxy the
// sandbox points them at the job's own proxy; a value from outside would send the job's
// programs somewhere the sandbox has no route to, or past the proxy for the hosts it
// names, so config refuses them in that mode. GOMAXPROCS is the relay's: the sandbox sets
// it for the relay and removes it before the job's command starts, so a value from
// outside would be dropped silently. A job sets its own in its command.
var proxySandboxEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "GOMAXPROCS"}

// Config is the Epitrop process configuration loaded from a schema-checked
// file.
type Config struct {
	Graph        GraphConfig        `koanf:"graph" json:"graph"`
	Budgets      BudgetsConfig      `koanf:"budgets" json:"budgets"`
	Jobs         []JobConfig        `koanf:"jobs" json:"jobs"`
	Watches      []WatchConfig      `koanf:"watches" json:"watches"`
	Router       RouterConfig       `koanf:"router" json:"router"`
	Admission    AdmissionConfig    `koanf:"admission" json:"admission"`
	EventLog     EventLogConfig     `koanf:"event_log" json:"event_log"`
	ReleaseWatch ReleaseWatchConfig `koanf:"release_watch" json:"release_watch"`
	Server       ServerConfig       `koanf:"server" json:"server"`
	Broker       BrokerConfig       `koanf:"broker" json:"broker"`
	// SandboxInputRoots are the directories a sandboxed job's workspace and inputs must lie
	// in once symlinks are resolved. With none, no job runs sandboxed.
	SandboxInputRoots []string `koanf:"sandbox_input_roots" json:"sandbox_input_roots"`
}

// BrokerConfig sizes the in-process broker: the queue of each subscriber, and the
// dead-letter class set failed attempts are sorted into.
type BrokerConfig struct {
	QueueCapacity int              `koanf:"queue_capacity" json:"queue_capacity"`
	DeadLetter    DeadLetterConfig `koanf:"dead_letter" json:"dead_letter"`
}

// DeadLetterConfig declares the dead-letter classes. Classes lists the classes with
// signatures, in match order; Fallback names the class that takes every failure none of
// them matched, and grants no retry.
type DeadLetterConfig struct {
	Capacity int               `koanf:"capacity" json:"capacity"`
	Fallback string            `koanf:"fallback" json:"fallback"`
	Classes  []DeadLetterClass `koanf:"classes" json:"classes"`
}

type DeadLetterClass struct {
	Name       string   `koanf:"name" json:"name"`
	Signatures []string `koanf:"signatures" json:"signatures"`
	MaxRetries int      `koanf:"max_retries" json:"max_retries"`
}

// Specs returns the class set for broker.NewDeadLetters: the declared classes in order, then
// the fallback class.
func (c DeadLetterConfig) Specs() []broker.ClassSpec {
	specs := make([]broker.ClassSpec, 0, len(c.Classes)+1)
	for _, class := range c.Classes {
		specs = append(specs, broker.ClassSpec{Name: class.Name, Signatures: class.Signatures, MaxRetries: class.MaxRetries})
	}
	return append(specs, broker.ClassSpec{Name: c.Fallback})
}

type ReleaseWatchConfig struct {
	StateDir         string              `koanf:"state_dir" json:"state_dir"`
	RateLimitFloor   int                 `koanf:"rate_limit_floor" json:"rate_limit_floor"`
	MaxActionsPerRun int                 `koanf:"max_actions_per_run" json:"max_actions_per_run"`
	PerSourceCap     int                 `koanf:"per_source_cap" json:"per_source_cap"`
	Routes           []ReleaseWatchRoute `koanf:"routes" json:"routes"`
}

type ReleaseWatchRoute struct {
	Name    string               `koanf:"name" json:"name"`
	Sources []ReleaseWatchSource `koanf:"sources" json:"sources"`
	Sinks   []ReleaseWatchSink   `koanf:"sinks" json:"sinks"`
}

type ReleaseWatchSource struct {
	GitHub         string `koanf:"github" json:"github,omitempty"`
	HuggingFaceOrg string `koanf:"huggingface_org" json:"huggingface_org,omitempty"`
}

type ReleaseWatchSink struct {
	GitHubIssue *GitHubIssueSink `koanf:"github_issue" json:"github_issue,omitempty"`
	Ntfy        *NtfySink        `koanf:"ntfy" json:"ntfy,omitempty"`
}

type GitHubIssueSink struct {
	Repo   string   `koanf:"repo" json:"repo"`
	Labels []string `koanf:"labels" json:"labels"`
}

type NtfySink struct {
	Topic    string `koanf:"topic" json:"topic"`
	Priority int    `koanf:"priority" json:"priority"`
}

type GraphConfig struct {
	MaxDepth int `koanf:"max_depth" json:"max_depth"`
}

type BudgetsConfig struct {
	Default  TaskBudget     `koanf:"default" json:"default"`
	Platform PlatformBudget `koanf:"platform" json:"platform"`
}

// PlatformBudget is the platform budget layer (#6): what the whole platform may
// spend, read against the Praetor efficiency ledger. 0 turns the check off.
type PlatformBudget struct {
	FrontierTokens int `koanf:"frontier_tokens" json:"frontier_tokens"`
}

type TaskBudget struct {
	Tokens           int `koanf:"tokens" json:"tokens"`
	WallClockSeconds int `koanf:"wall_clock_seconds" json:"wall_clock_seconds"`
}

type JobConfig struct {
	Name        string           `koanf:"name" json:"name"`
	Command     []string         `koanf:"command" json:"command"`
	LogPath     string           `koanf:"log_path" json:"log_path"`
	Schedule    string           `koanf:"schedule" json:"schedule"`
	OnEvent     *JobEventTrigger `koanf:"on_event" json:"on_event,omitempty"`
	RouterAlias string           `koanf:"router_alias" json:"router_alias"`
	// MaxRuntimeSeconds is the job's deadline: when it has run this long it is stopped like
	// `jobs stop` stops it and its exit is recorded with reason "deadline". 0 means the
	// longest runtime any job may have (MaxJobRuntimeSeconds).
	MaxRuntimeSeconds int              `koanf:"max_runtime_seconds" json:"max_runtime_seconds"`
	Restart           JobRestartConfig `koanf:"restart" json:"restart"`
	Stop              JobStopConfig    `koanf:"stop" json:"stop"`
	Heartbeat         JobHeartbeat     `koanf:"heartbeat" json:"heartbeat"`
	Sandbox           SandboxConfig    `koanf:"sandbox" json:"sandbox"`
}

// JobHeartbeat says how old a running job's last sign of life may be. A write to the job's
// log is a beat, and so is its start. A job silent for longer than MaxAgeSeconds is hung:
// the supervisor records job.stale and ends it the way `jobs stop` would, and the restart
// policy decides what follows. 0 turns the check off.
type JobHeartbeat struct {
	MaxAgeSeconds int `koanf:"max_age_seconds" json:"max_age_seconds"`
}

type JobRestartConfig struct {
	Policy         string `koanf:"policy" json:"policy"`
	MaxRestarts    int    `koanf:"max_restarts" json:"max_restarts"`
	BackoffSeconds int    `koanf:"backoff_seconds" json:"backoff_seconds"`
}

type JobStopConfig struct {
	Signal       string `koanf:"signal" json:"signal"`
	GraceSeconds int    `koanf:"grace_seconds" json:"grace_seconds"`
}

type SandboxConfig struct {
	Mode      string   `koanf:"mode" json:"mode"`
	Reason    string   `koanf:"reason" json:"reason"`
	Workspace string   `koanf:"workspace" json:"workspace"`
	Inputs    []string `koanf:"inputs" json:"inputs"`
	// ReadOnly are paths inside the workspace, relative to it, that the job may read and not
	// write: a working copy's hooks, for one. Each has to exist and stay inside the workspace
	// when its symlinks are resolved, at load and again at every start.
	ReadOnly []string `koanf:"read_only" json:"read_only"`
	EnvAllow []string `koanf:"env_allow" json:"env_allow"`
	// Network is what the job reaches: "none" nothing, "egress" the internet without the
	// host's loopback, "proxy" the Destinations and nothing else.
	Network string `koanf:"network" json:"network"`
	// Destinations are the "host:port" values a job with network=proxy may connect to,
	// each in the one spelling egressproxy.Canonical accepts. A name never leads to a
	// loopback or link-local address; such a destination is written as an address.
	Destinations []string `koanf:"destinations" json:"destinations"`
	MemoryMax    string   `koanf:"memory_max" json:"memory_max"`
	// MemoryHigh is the soft limit: above it the kernel throttles the job and reclaims its
	// memory before the hard limit kills it. Empty means none. It never exceeds MemoryMax.
	MemoryHigh string `koanf:"memory_high" json:"memory_high"`
	CPUWeight  int    `koanf:"cpu_weight" json:"cpu_weight"`
	TasksMax   int    `koanf:"tasks_max" json:"tasks_max"`
	// Home is the client home the shim makes in the workspace before each start; nil for
	// none.
	Home *SandboxHome `koanf:"home" json:"home,omitempty"`
}

type WatchConfig struct {
	Name        string `koanf:"name" json:"name"`
	Trigger     string `koanf:"trigger" json:"trigger"`
	RouterAlias string `koanf:"router_alias" json:"router_alias"`
}

type RouterConfig struct {
	Aliases []string `koanf:"aliases" json:"aliases"`
}

type AdmissionConfig struct {
	Probe ProbeConfig `koanf:"probe" json:"probe"`
}

type ProbeConfig struct {
	TimeoutSeconds  int `koanf:"timeout_seconds" json:"timeout_seconds"`
	IntervalSeconds int `koanf:"interval_seconds" json:"interval_seconds"`
	ReadyThreshold  int `koanf:"ready_threshold" json:"ready_threshold"`
}

type EventLogConfig struct {
	Dir                string `koanf:"dir" json:"dir"`
	SigningKeyPath     string `koanf:"signing_key_path" json:"signing_key_path"`
	PublicKey          string `koanf:"public_key" json:"public_key"`
	LockTimeoutSeconds int    `koanf:"lock_timeout_seconds" json:"lock_timeout_seconds"`
	MaxReplayRecords   int    `koanf:"max_replay_records" json:"max_replay_records"`
	// Git commits the log at checkpoints; see EventLogGitConfig.
	Git EventLogGitConfig `koanf:"git" json:"git"`
}

// EventLogGitConfig makes the control plane commit the log's events/ and checkpoints/ to the
// git repository the log directory lies in, at every checkpoint, onto Ref and as its own
// author. Nothing checks Ref out; the operator's index, working tree and branch stay as
// they are.
type EventLogGitConfig struct {
	Enabled        bool   `koanf:"enabled" json:"enabled"`
	Ref            string `koanf:"ref" json:"ref"`
	AuthorName     string `koanf:"author_name" json:"author_name"`
	AuthorEmail    string `koanf:"author_email" json:"author_email"`
	TimeoutSeconds int    `koanf:"timeout_seconds" json:"timeout_seconds"`
	// Compact moves the log's base forward after each commit and removes the segments the
	// commit holds; see eventlog.Writer.Compact. It needs Enabled.
	Compact bool `koanf:"compact" json:"compact"`
}

// Server transports.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// ServerConfig is how epitrop-server serves its tools: on stdin and stdout, or over HTTP
// on Listen. Listen must name a loopback host; port 0 picks a free port.
type ServerConfig struct {
	Transport string `koanf:"transport" json:"transport"`
	Listen    string `koanf:"listen" json:"listen"`
	// AllowMutations adds the tools that start and stop jobs and record unit status.
	AllowMutations bool `koanf:"allow_mutations" json:"allow_mutations"`
}

// SchemaJSON returns the embedded JSON Schema document used by Load.
func SchemaJSON() []byte {
	return append([]byte(nil), schemaJSON...)
}

// Default returns the Go defaults mirrored by config.schema.json.
func Default() Config {
	return Config{
		Graph: GraphConfig{MaxDepth: defaultGraphDepth},
		Budgets: BudgetsConfig{Default: TaskBudget{
			Tokens:           defaultTokens,
			WallClockSeconds: defaultWallClockSec,
		}},
		Jobs:    []JobConfig{},
		Watches: []WatchConfig{},
		Router:  RouterConfig{Aliases: []string{defaultAlias}},
		Admission: AdmissionConfig{Probe: ProbeConfig{
			TimeoutSeconds:  defaultTimeoutSec,
			IntervalSeconds: defaultIntervalSec,
			ReadyThreshold:  defaultReadyCount,
		}},
		EventLog: EventLogConfig{
			Dir:                defaultEventLogDir,
			SigningKeyPath:     "",
			PublicKey:          "",
			LockTimeoutSeconds: defaultLockSeconds,
			MaxReplayRecords:   defaultReplayMax,
			Git:                EventLogGitConfig{Ref: defaultEventLogRef, TimeoutSeconds: defaultGitSeconds},
		},
		ReleaseWatch: ReleaseWatchConfig{
			StateDir:         "",
			RateLimitFloor:   DefaultRateLimitFloor,
			MaxActionsPerRun: DefaultMaxActionsPerRun,
			PerSourceCap:     DefaultPerSourceCap,
			Routes:           []ReleaseWatchRoute{},
		},
		Server: ServerConfig{Transport: TransportStdio, Listen: defaultServerListen},
		Broker: BrokerConfig{
			QueueCapacity: defaultBrokerQueue,
			DeadLetter:    DeadLetterConfig{Capacity: defaultBrokerQueue, Fallback: defaultFallbackClass, Classes: []DeadLetterClass{}},
		},
		SandboxInputRoots: []string{},
	}
}

// Load reads path through golusoris/core/config, validates the merged document
// against the embedded schema, then decodes into typed config.
func Load(ctx context.Context, path string) (Config, error) {
	if err := readyContext(ctx); err != nil {
		return Config{}, err
	}
	if path == "" {
		return Config{}, errors.New("config: path is required")
	}
	if _, err := os.Stat(path); err != nil {
		return Config{}, fmt.Errorf("config: stat %s: %w", path, err)
	}
	loaded, err := coreconfig.New(coreconfig.Options{
		Delimiter: ".",
		Files:     []string{path},
		Watch:     false,
		// Environment values arrive as strings the schema would refuse or the
		// weak decoder would coerce, so only the file is read
		// (TestLoadDisablesEnvironmentOverrides).
		DisableEnv: true,
	})
	if err != nil {
		return Config{}, fmt.Errorf("config: load %s: %w", path, err)
	}
	if err := validateFlat(loaded.All()); err != nil {
		return Config{}, err
	}
	cfg := Default()
	if err = loaded.Unmarshal("", &cfg); err != nil {
		return Config{}, err
	}
	if err = validateLoaded(&cfg, path); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateLoaded applies the defaults to a decoded document and runs the checks that need
// the typed config; path is the file the sandbox paths resolve against.
func validateLoaded(cfg *Config, path string) error {
	applyJobDefaults(cfg.Jobs)
	applyReleaseWatchDefaults(&cfg.ReleaseWatch)
	if err := validateNames(*cfg); err != nil {
		return err
	}
	for i := 0; i < len(cfg.Jobs); i++ {
		if err := ValidateJobTrigger(cfg.Jobs[i]); err != nil {
			return err
		}
	}
	if err := validateRouterAliases(*cfg); err != nil {
		return err
	}
	if err := validateReleaseWatch(cfg); err != nil {
		return err
	}
	if err := validateEventLogGit(cfg.EventLog.Git); err != nil {
		return err
	}
	if err := ValidateListen(cfg.Server.Listen); err != nil {
		return err
	}
	if err := validateBroker(cfg.Broker); err != nil {
		return err
	}
	return validateJobSandboxes(cfg, path)
}

// ReadDocument reads a configuration file as a document. It validates nothing and applies
// no default: it is for a caller that completes the document and writes it out before it is
// loaded, and Load is what validates the result.
func ReadDocument(ctx context.Context, path string) (map[string]any, error) {
	if err := readyContext(ctx); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("config: path is required")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("config: stat %s: %w", path, err)
	}
	loaded, err := coreconfig.New(coreconfig.Options{Delimiter: ".", Files: []string{path}, Watch: false, DisableEnv: true})
	if err != nil {
		return nil, fmt.Errorf("config: load %s: %w", path, err)
	}
	return nestedDocument(loaded.All())
}

func applyJobDefaults(jobs []JobConfig) {
	for i := 0; i < len(jobs); i++ {
		if jobs[i].Schedule == "" {
			jobs[i].Schedule = defaultJobSchedule
		}
		if jobs[i].RouterAlias == "" {
			jobs[i].RouterAlias = defaultAlias
		}
		if jobs[i].Restart.Policy == "" {
			jobs[i].Restart.Policy = defaultRestart
		}
		if jobs[i].Stop.Signal == "" {
			jobs[i].Stop.Signal = defaultStopSignal
		}
		applySandboxDefaults(&jobs[i].Sandbox)
	}
}

func applySandboxDefaults(sandbox *SandboxConfig) {
	if sandbox.Mode == "" {
		sandbox.Mode = defaultSandboxMode
	}
	if sandbox.Network == "" {
		sandbox.Network = defaultSandboxNet
	}
	if sandbox.MemoryMax == "" {
		sandbox.MemoryMax = defaultMemoryMax
	}
	if sandbox.CPUWeight == 0 {
		sandbox.CPUWeight = defaultCPUWeight
	}
	if sandbox.TasksMax == 0 {
		sandbox.TasksMax = defaultTasksMax
	}
}

func NormalizeSandbox(sandbox SandboxConfig) SandboxConfig {
	applySandboxDefaults(&sandbox)
	return sandbox
}

func ValidateSandbox(sandbox SandboxConfig) error {
	applySandboxDefaults(&sandbox)
	if err := validateSandboxMode(sandbox); err != nil {
		return err
	}
	if err := validateSandboxLimits(sandbox); err != nil {
		return err
	}
	if err := validateSandboxEnv(sandbox.EnvAllow); err != nil {
		return err
	}
	if err := validateSandboxReadOnly(sandbox); err != nil {
		return err
	}
	return validateSandboxHome(sandbox)
}

func readyContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("config: nil context")
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("config: Load requires a context deadline")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("config: context: %w", err)
	}
	return nil
}

func validateFlat(flat map[string]any) error {
	doc, err := nestedDocument(flat)
	if err != nil {
		return err
	}
	schema, err := golusorisschema.Compile(schemaResourceID, schemaJSON)
	if err != nil {
		return fmt.Errorf("config: compile schema: %w", err)
	}
	if err = schema.ValidateValue(doc); err != nil {
		return fmt.Errorf("config: validate %s: %w", validationPath(err), err)
	}
	return nil
}

func nestedDocument(flat map[string]any) (map[string]any, error) {
	doc := map[string]any{}
	if len(flat) == 0 {
		return doc, nil
	}
	for key, value := range flat {
		parts := strings.Split(key, ".")
		if err := insertPath(doc, parts, value); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

func insertPath(root map[string]any, parts []string, value any) error {
	if len(parts) == 0 || len(parts) > maxConfigKeyDepth {
		return fmt.Errorf("config: invalid key path depth %d", len(parts))
	}
	cursor := root
	last := len(parts) - 1
	for i := 0; i < last; i++ {
		next, ok := cursor[parts[i]]
		if !ok {
			child := map[string]any{}
			cursor[parts[i]] = child
			cursor = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("config: key path conflict at %s", strings.Join(parts[:i+1], "."))
		}
		cursor = child
	}
	cursor[parts[last]] = value
	return nil
}

func validateNames(cfg Config) error {
	if err := validateJobNames(cfg.Jobs); err != nil {
		return err
	}
	return validateWatchNames(cfg.Watches)
}

func validateJobSandboxes(cfg *Config, configPath string) error {
	if !anySandboxEnforced(cfg.Jobs) {
		for i := 0; i < len(cfg.Jobs); i++ {
			if err := validateJobSandbox(&cfg.Jobs[i], protectedPaths{}); err != nil {
				return fmt.Errorf("config: job %q sandbox.%w", cfg.Jobs[i].Name, err)
			}
		}
		return nil
	}
	guard, err := sandboxPathGuard(*cfg, configPath)
	if err != nil {
		return err
	}
	for i := 0; i < len(cfg.Jobs); i++ {
		if err = validateJobSandbox(&cfg.Jobs[i], guard); err != nil {
			return fmt.Errorf("config: job %q sandbox.%w", cfg.Jobs[i].Name, err)
		}
	}
	return nil
}

func validateJobSandbox(job *JobConfig, guard protectedPaths) error {
	applySandboxDefaults(&job.Sandbox)
	if err := validateSandboxSettings(*job); err != nil {
		return err
	}
	if job.Sandbox.Mode == "off" {
		return nil
	}
	workspace, err := resolveSandboxWorkspace(job.Sandbox.Workspace)
	if err != nil {
		return err
	}
	if err = guard.check("workspace", workspace); err != nil {
		return err
	}
	inputs, err := resolveSandboxInputs(job.Sandbox.Inputs, guard)
	if err != nil {
		return err
	}
	job.Sandbox.Workspace, job.Sandbox.Inputs = workspace, inputs
	if _, err = ResolveSandboxReadOnly(job.Sandbox); err != nil {
		return err
	}
	return resolveSandboxHome(&job.Sandbox, guard)
}

// validateSandboxSettings checks the sandbox settings that need no path resolved.
func validateSandboxSettings(job JobConfig) error {
	if err := validateSandboxMode(job.Sandbox); err != nil {
		return err
	}
	if err := validateSandboxLimits(job.Sandbox); err != nil {
		return err
	}
	if err := validateSandboxEnv(job.Sandbox.EnvAllow); err != nil {
		return err
	}
	if err := validateSandboxReadOnly(job.Sandbox); err != nil {
		return err
	}
	if err := validateSandboxHome(job.Sandbox); err != nil {
		return err
	}
	// A bound login has to outlast the run, and only a deadline says how long that is.
	if job.Sandbox.Home != nil && job.MaxRuntimeSeconds == 0 {
		return fmt.Errorf("home: needs the job's max_runtime_seconds")
	}
	return nil
}

func validateSandboxMode(sandbox SandboxConfig) error {
	switch sandbox.Mode {
	case "enforce":
		if sandbox.Reason != "" {
			return fmt.Errorf("reason: forbidden unless mode=off")
		}
	case "off":
		if strings.TrimSpace(sandbox.Reason) == "" {
			return fmt.Errorf("reason: required when mode=off")
		}
	default:
		return fmt.Errorf("mode: unknown value %q", sandbox.Mode)
	}
	return validateSandboxNetwork(sandbox)
}

func validateSandboxNetwork(sandbox SandboxConfig) error {
	switch sandbox.Network {
	case "none", "egress":
		if len(sandbox.Destinations) > 0 {
			return fmt.Errorf("destinations: forbidden unless network=proxy")
		}
		return nil
	case "proxy":
		return validateSandboxProxy(sandbox)
	default:
		return fmt.Errorf("network: unknown value %q", sandbox.Network)
	}
}

// validateSandboxProxy checks what network=proxy needs: declared destinations, no proxy
// variable from outside, and a task limit with room for the relay that runs in front of the
// job and counts against it.
func validateSandboxProxy(sandbox SandboxConfig) error {
	if err := validateSandboxDestinations(sandbox.Destinations); err != nil {
		return err
	}
	for i := 0; i < len(sandbox.EnvAllow); i++ {
		if slices.Contains(proxySandboxEnv, sandbox.EnvAllow[i]) {
			return fmt.Errorf("env_allow/%d: %s is set by the sandbox when network=proxy", i, sandbox.EnvAllow[i])
		}
	}
	if least := minTasksMax + egressproxy.MaxRelayTasks; sandbox.TasksMax < least {
		return fmt.Errorf("tasks_max: network=proxy needs at least %d, the relay in front of the job takes up to %d", least, egressproxy.MaxRelayTasks)
	}
	return nil
}

// validateSandboxDestinations wants at least one destination, each in its one spelling and
// named once.
func validateSandboxDestinations(destinations []string) error {
	if len(destinations) == 0 || len(destinations) > egressproxy.MaxDestinations {
		return fmt.Errorf("destinations: network=proxy needs 1..%d", egressproxy.MaxDestinations)
	}
	for i := 0; i < len(destinations); i++ {
		if err := egressproxy.Canonical(destinations[i]); err != nil {
			return fmt.Errorf("destinations/%d: %w", i, err)
		}
		if slices.Contains(destinations[:i], destinations[i]) {
			return fmt.Errorf("destinations/%d: %s is named twice", i, destinations[i])
		}
	}
	return nil
}

func validateSandboxLimits(sandbox SandboxConfig) error {
	bytes, err := parseMemoryMax(sandbox.MemoryMax)
	if err != nil {
		return fmt.Errorf("memory_max: %w", err)
	}
	if bytes < minMemoryMaxBytes || bytes > maxMemoryMaxBytes {
		return fmt.Errorf("memory_max: must be 64M..64G")
	}
	if err = validateMemoryHigh(sandbox.MemoryHigh, bytes); err != nil {
		return err
	}
	if sandbox.CPUWeight < minCPUWeight || sandbox.CPUWeight > maxCPUWeight {
		return fmt.Errorf("cpu_weight: must be 1..10000")
	}
	if sandbox.TasksMax < minTasksMax || sandbox.TasksMax > maxTasksMax {
		return fmt.Errorf("tasks_max: must be 16..32768")
	}
	return nil
}

// CheckMemoryLimits checks a hard and a soft memory limit as a sandbox's memory_max and
// memory_high are checked: the hard limit within 64M..64G, the soft one within 64M and the
// hard limit. Both are required here; a sandbox may leave its soft limit out.
func CheckMemoryLimits(memoryMax string, memoryHigh string) error {
	if memoryHigh == "" {
		return errors.New("memory_high: is required")
	}
	return validateSandboxLimits(SandboxConfig{MemoryMax: memoryMax, MemoryHigh: memoryHigh, CPUWeight: minCPUWeight, TasksMax: minTasksMax})
}

// validateMemoryHigh checks the soft limit against the hard one. A soft limit above the hard
// limit would never act; one below the floor of the hard limit would throttle every job.
func validateMemoryHigh(value string, maxBytes int64) error {
	if value == "" {
		return nil
	}
	high, err := parseMemoryMax(value)
	if err != nil {
		return fmt.Errorf("memory_high: %w", err)
	}
	if high < minMemoryMaxBytes || high > maxBytes {
		return fmt.Errorf("memory_high: must be 64M..memory_max")
	}
	return nil
}

func validateSandboxEnv(names []string) error {
	if len(names) > MaxSandboxEnvAllow {
		return fmt.Errorf("env_allow: exceeds %d", MaxSandboxEnvAllow)
	}
	for i := 0; i < len(names); i++ {
		if !sandboxEnvName.MatchString(names[i]) {
			return fmt.Errorf("env_allow/%d: invalid name %q", i, names[i])
		}
		for j := 0; j < len(reservedSandboxEnv); j++ {
			if names[i] == reservedSandboxEnv[j] {
				return fmt.Errorf("env_allow/%d: %s is reserved by the sandbox", i, names[i])
			}
		}
	}
	return nil
}

func resolveSandboxWorkspace(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("workspace: required when mode=enforce")
	}
	resolved, err := resolveExistingAbs(path)
	if err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("workspace: stat %s: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace: %s is not a directory", resolved)
	}
	return resolved, nil
}

func resolveSandboxInputs(paths []string, guard protectedPaths) ([]string, error) {
	if len(paths) > MaxSandboxInputs {
		return nil, fmt.Errorf("inputs: exceeds %d", MaxSandboxInputs)
	}
	out := make([]string, 0, len(paths))
	for i := 0; i < len(paths); i++ {
		resolved, err := resolveExistingAbs(paths[i])
		if err != nil {
			return nil, fmt.Errorf("inputs/%d: %w", i, err)
		}
		if err = guard.check(fmt.Sprintf("inputs/%d", i), resolved); err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

type protectedPaths struct {
	paths []string
	home  string
	// roots are the resolved sandbox_input_roots: the only directories a workspace or an
	// input may lie in.
	roots []string
}

// loginLocations are the places below HOME where shells, forges, clouds and coding clients
// keep their logins. They are the second barrier behind the roots: a workspace or input
// that overlaps one is refused, unless a root names that very location, which then opens
// that location alone. A root that merely contains one opens none of it.
var loginLocations = []string{
	".ssh", ".gnupg", ".netrc", ".git-credentials", ".config/git/credentials", ".config/gh",
	".docker", ".aws", ".azure", ".config/gcloud", ".kube", ".password-store", ".pki",
	".local/share/keyrings", ".claude", ".claude.json", ".codex", ".gemini", ".config/opencode",
	".local/share/opencode",
}

func anySandboxEnforced(jobs []JobConfig) bool {
	for i := 0; i < len(jobs); i++ {
		if jobs[i].Sandbox.Mode != "off" {
			return true
		}
	}
	return false
}

// sandboxPathGuard fails closed when HOME cannot be resolved: without it the guard could
// not refuse $HOME as a workspace.
func sandboxPathGuard(cfg Config, configPath string) (protectedPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return protectedPaths{}, fmt.Errorf("config: sandbox needs HOME to guard it: %w", err)
	}
	if home, err = filepath.EvalSymlinks(home); err != nil {
		return protectedPaths{}, fmt.Errorf("config: sandbox resolve HOME: %w", err)
	}
	paths, err := protectedPathList(cfg, configPath)
	if err != nil {
		return protectedPaths{}, err
	}
	roots, err := resolveInputRoots(cfg.SandboxInputRoots, home)
	if err != nil {
		return protectedPaths{}, err
	}
	return protectedPaths{paths: paths, home: home, roots: roots}, nil
}

// resolveInputRoots resolves each root's symlinks. A root has to exist, and neither / nor a
// directory containing HOME is one: either would open the user's whole home directory.
func resolveInputRoots(roots []string, home string) ([]string, error) {
	if len(roots) > MaxSandboxInputRoots {
		return nil, fmt.Errorf("config: sandbox_input_roots: exceeds %d", MaxSandboxInputRoots)
	}
	out := make([]string, 0, len(roots))
	for i := 0; i < len(roots); i++ {
		resolved, err := resolveExistingAbs(roots[i])
		if err != nil {
			return nil, fmt.Errorf("config: sandbox_input_roots/%d: %w", i, err)
		}
		if pathContains(resolved, home) {
			return nil, fmt.Errorf("config: sandbox_input_roots/%d: refuse %s, it is / or contains HOME %s", i, resolved, home)
		}
		out = append(out, resolved)
	}
	return out, nil
}

// sandboxExecutable finds the running Epitrop binary. The supervisor re-executes it as
// each job's shim, so a job that could write it would run its own code outside the sandbox
// at the next start.
var sandboxExecutable = os.Executable

func protectedPathList(cfg Config, configPath string) ([]string, error) {
	executable, err := sandboxExecutable()
	if err != nil {
		return nil, fmt.Errorf("config: sandbox needs the executable path to guard it: %w", err)
	}
	candidates := []string{cfg.EventLog.Dir, cfg.EventLog.SigningKeyPath, configPath, executable}
	out := make([]string, 0, len(candidates))
	for i := 0; i < len(candidates); i++ {
		if candidates[i] == "" {
			continue
		}
		resolved, err := resolvePossiblyMissingAbs(candidates[i])
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

func (guard protectedPaths) check(field string, path string) error {
	if path == "/" {
		return fmt.Errorf("%s: refuse /", field)
	}
	// A directory containing HOME would hand the job the user's whole home directory.
	if guard.home != "" && pathContains(path, guard.home) {
		return fmt.Errorf("%s: refuse HOME %s or a directory containing it", field, guard.home)
	}
	for i := 0; i < len(guard.paths); i++ {
		if pathOverlaps(path, guard.paths[i]) {
			return fmt.Errorf("%s: overlaps protected path %s", field, guard.paths[i])
		}
	}
	if !guard.inRoot(path) {
		return fmt.Errorf("%s: %s lies in no sandbox_input_roots entry", field, path)
	}
	return guard.checkLogins(field, path)
}

func (guard protectedPaths) inRoot(path string) bool {
	for i := 0; i < len(guard.roots); i++ {
		if pathContains(guard.roots[i], path) {
			return true
		}
	}
	return false
}

// checkLogins refuses a path that overlaps a login location, unless the path lies in that
// location and a root names exactly it.
func (guard protectedPaths) checkLogins(field string, path string) error {
	for i := 0; i < len(loginLocations); i++ {
		login := filepath.Join(guard.home, filepath.FromSlash(loginLocations[i]))
		if !pathOverlaps(path, login) {
			continue
		}
		if !pathContains(login, path) || !slices.ContainsFunc(guard.roots, func(root string) bool { return samePath(root, login) }) {
			return fmt.Errorf("%s: %s overlaps the login location %s, which no root names", field, path, login)
		}
	}
	return nil
}

func resolveExistingAbs(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is not absolute", path)
	}
	return filepath.EvalSymlinks(path)
}

func resolvePossiblyMissingAbs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("config: abs %s: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	return filepath.Clean(abs), nil
}

func pathOverlaps(a string, b string) bool {
	return samePath(a, b) || pathContains(a, b) || pathContains(b, a)
}

func samePath(a string, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func pathContains(parent string, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child || parent == "/" {
		return true
	}
	return strings.HasPrefix(child, parent+string(os.PathSeparator))
}

func parseMemoryMax(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("is required")
	}
	unit := value[len(value)-1:]
	digits := value
	multiplier := int64(1)
	switch unit {
	case "K", "M", "G", "T":
		digits = value[:len(value)-1]
		multiplier = memoryUnitMultiplier(unit)
	}
	number, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("must be positive systemd size")
	}
	if number > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("is too large")
	}
	return number * multiplier, nil
}

func memoryUnitMultiplier(unit string) int64 {
	switch unit {
	case "K":
		return 1024
	case "M":
		return 1024 * 1024
	case "G":
		return 1024 * 1024 * 1024
	case "T":
		return 1024 * 1024 * 1024 * 1024
	}
	return 1
}

func validateJobNames(jobs []JobConfig) error {
	seen := make(map[string]struct{}, len(jobs))
	for i, job := range jobs {
		if _, ok := seen[job.Name]; ok {
			return fmt.Errorf("config: jobs/%d/name: duplicate name %q", i, job.Name)
		}
		seen[job.Name] = struct{}{}
	}
	return nil
}

func validateWatchNames(watches []WatchConfig) error {
	seen := make(map[string]struct{}, len(watches))
	for i, watch := range watches {
		if _, ok := seen[watch.Name]; ok {
			return fmt.Errorf("config: watches/%d/name: duplicate name %q", i, watch.Name)
		}
		seen[watch.Name] = struct{}{}
	}
	return nil
}

// validateRouterAliases refuses a job or watch whose router alias is not listed in router.aliases.
func validateRouterAliases(cfg Config) error {
	known := make(map[string]struct{}, len(cfg.Router.Aliases))
	for _, alias := range cfg.Router.Aliases {
		known[alias] = struct{}{}
	}
	for _, job := range cfg.Jobs {
		if _, ok := known[job.RouterAlias]; !ok {
			return fmt.Errorf("config: job %q router_alias %q is not listed in router.aliases", job.Name, job.RouterAlias)
		}
	}
	for _, watch := range cfg.Watches {
		if watch.RouterAlias == "" {
			continue
		}
		if _, ok := known[watch.RouterAlias]; !ok {
			return fmt.Errorf("config: watch %q router_alias %q is not listed in router.aliases", watch.Name, watch.RouterAlias)
		}
	}
	return nil
}

func validationPath(err error) string {
	text := err.Error()
	start := strings.Index(text, "'/")
	if start < 0 {
		return "/"
	}
	rest := text[start+1:]
	end := strings.Index(rest, "'")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func applyReleaseWatchDefaults(rw *ReleaseWatchConfig) {
	if rw.RateLimitFloor == 0 {
		rw.RateLimitFloor = DefaultRateLimitFloor
	}
	if rw.MaxActionsPerRun == 0 {
		rw.MaxActionsPerRun = DefaultMaxActionsPerRun
	}
	if rw.PerSourceCap == 0 {
		rw.PerSourceCap = DefaultPerSourceCap
	}
	for i := 0; i < len(rw.Routes); i++ {
		for j := 0; j < len(rw.Routes[i].Sinks); j++ {
			if rw.Routes[i].Sinks[j].Ntfy != nil && rw.Routes[i].Sinks[j].Ntfy.Priority == 0 {
				rw.Routes[i].Sinks[j].Ntfy.Priority = DefaultNtfyPriority
			}
		}
	}
}

// validateEventLogGit checks what the schema cannot: an enabled commit needs its author.
func validateEventLogGit(g EventLogGitConfig) error {
	if g.Enabled && (strings.TrimSpace(g.AuthorName) == "" || strings.TrimSpace(g.AuthorEmail) == "") {
		return errors.New("config: event_log/git: author_name and author_email are required when enabled")
	}
	if g.Compact && !g.Enabled {
		return errors.New("config: event_log/git: compact needs enabled: a segment leaves events/ only once a commit holds it")
	}
	return nil
}

// ValidateListen refuses an address whose host is not loopback: the server has no
// authentication, so it serves only this host.
func ValidateListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: server/listen: %w", err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("config: server/listen: port %q is not 0..65535", port)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("config: server/listen: host %q is not loopback", host)
	}
	return nil
}

// validateBroker builds the broker and the class set the block declares, so the config
// refuses exactly what the broker would.
func validateBroker(b BrokerConfig) error {
	if _, err := broker.New(b.QueueCapacity); err != nil {
		return fmt.Errorf("config: broker/queue_capacity: %w", err)
	}
	dl := b.DeadLetter
	if _, err := broker.NewDeadLetters(dl.Specs(), dl.Fallback, dl.Capacity); err != nil {
		return fmt.Errorf("config: broker/dead_letter: %w", err)
	}
	return nil
}

func validateReleaseWatch(cfg *Config) error {
	rw := &cfg.ReleaseWatch
	applyReleaseWatchDefaults(rw)
	if len(rw.Routes) == 0 {
		return nil
	}
	if rw.StateDir == "" || !filepath.IsAbs(rw.StateDir) {
		return fmt.Errorf("config: release_watch/state_dir: must be an absolute path: %q", rw.StateDir)
	}
	if rw.MaxActionsPerRun < MinMaxActionsPerRun || rw.MaxActionsPerRun > MaxMaxActionsPerRun {
		return fmt.Errorf("config: release_watch/max_actions_per_run: must be %d..%d", MinMaxActionsPerRun, MaxMaxActionsPerRun)
	}
	if rw.PerSourceCap < MinPerSourceCap || rw.PerSourceCap > MaxPerSourceCap {
		return fmt.Errorf("config: release_watch/per_source_cap: must be %d..%d", MinPerSourceCap, MaxPerSourceCap)
	}
	if rw.RateLimitFloor < 0 {
		return fmt.Errorf("config: release_watch/rate_limit_floor: must be non-negative")
	}
	if len(rw.Routes) > MaxReleaseWatchRoutes {
		return fmt.Errorf("config: release_watch/routes: exceeds %d", MaxReleaseWatchRoutes)
	}
	return validateReleaseWatchRoutes(rw.Routes)
}

func validateReleaseWatchRoutes(routes []ReleaseWatchRoute) error {
	seenRoutes := make(map[string]struct{}, len(routes))
	for i := 0; i < len(routes); i++ {
		route := &routes[i]
		if !routeNamePattern.MatchString(route.Name) {
			return fmt.Errorf("config: release_watch/routes/%d/name: invalid name %q", i, route.Name)
		}
		if _, ok := seenRoutes[route.Name]; ok {
			return fmt.Errorf("config: release_watch/routes/%d/name: duplicate route name %q", i, route.Name)
		}
		seenRoutes[route.Name] = struct{}{}
		if err := validateRouteSources(route.Name, route.Sources); err != nil {
			return err
		}
		if err := validateRouteSinks(route.Name, route.Sinks); err != nil {
			return err
		}
	}
	return nil
}

func validateRouteSources(routeName string, sources []ReleaseWatchSource) error {
	if len(sources) == 0 {
		return fmt.Errorf("config: release_watch route %q sources: must not be empty", routeName)
	}
	if len(sources) > MaxReleaseWatchSources {
		return fmt.Errorf("config: release_watch route %q sources: exceeds %d", routeName, MaxReleaseWatchSources)
	}
	for i := 0; i < len(sources); i++ {
		if err := validateRouteSource(routeName, i, sources[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateRouteSource(routeName string, index int, src ReleaseWatchSource) error {
	hasGitHub := src.GitHub != ""
	hasHF := src.HuggingFaceOrg != ""
	if hasGitHub == hasHF {
		return fmt.Errorf("config: release_watch route %q sources/%d: must specify exactly one of github or huggingface_org", routeName, index)
	}
	if hasGitHub && !validRepository(src.GitHub) {
		return fmt.Errorf("config: release_watch route %q sources/%d/github: invalid repository %q (must be owner/name)", routeName, index, src.GitHub)
	}
	if hasHF && (!hfOrgPattern.MatchString(src.HuggingFaceOrg) || isDotSegment(src.HuggingFaceOrg)) {
		return fmt.Errorf("config: release_watch route %q sources/%d/huggingface_org: invalid org %q", routeName, index, src.HuggingFaceOrg)
	}
	return nil
}

// validRepository accepts owner/name. "." and ".." match the character class but would
// turn the API path the token is sent to into a different endpoint.
func validRepository(repo string) bool {
	if !repoPattern.MatchString(repo) {
		return false
	}
	owner, name, _ := strings.Cut(repo, "/")
	return !isDotSegment(owner) && !isDotSegment(name)
}

func isDotSegment(part string) bool {
	return part == "." || part == ".."
}

func validateRouteSinks(routeName string, sinks []ReleaseWatchSink) error {
	if len(sinks) == 0 {
		return fmt.Errorf("config: release_watch route %q sinks: must not be empty", routeName)
	}
	if len(sinks) > MaxReleaseWatchSinks {
		return fmt.Errorf("config: release_watch route %q sinks: exceeds %d", routeName, MaxReleaseWatchSinks)
	}
	for i := 0; i < len(sinks); i++ {
		if err := validateRouteSink(routeName, i, sinks[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateRouteSink(routeName string, index int, sink ReleaseWatchSink) error {
	hasGH := sink.GitHubIssue != nil
	hasNtfy := sink.Ntfy != nil
	if hasGH == hasNtfy {
		return fmt.Errorf("config: release_watch route %q sinks/%d: must specify exactly one of github_issue or ntfy", routeName, index)
	}
	if hasGH {
		return validateGitHubIssueSink(routeName, index, sink.GitHubIssue)
	}
	return validateNtfySink(routeName, index, sink.Ntfy)
}

func validateGitHubIssueSink(routeName string, index int, sink *GitHubIssueSink) error {
	if strings.TrimSpace(sink.Repo) == "" {
		return fmt.Errorf("config: release_watch route %q sinks/%d/github_issue/repo: required", routeName, index)
	}
	if !validRepository(sink.Repo) {
		return fmt.Errorf("config: release_watch route %q sinks/%d/github_issue/repo: invalid repository %q (must be owner/name)", routeName, index, sink.Repo)
	}
	if len(sink.Labels) > MaxReleaseWatchLabels {
		return fmt.Errorf("config: release_watch route %q sinks/%d/github_issue/labels: exceeds %d", routeName, index, MaxReleaseWatchLabels)
	}
	return nil
}

func validateNtfySink(routeName string, index int, sink *NtfySink) error {
	if strings.TrimSpace(sink.Topic) == "" {
		return fmt.Errorf("config: release_watch route %q sinks/%d/ntfy/topic: required", routeName, index)
	}
	if !ntfyTopicPattern.MatchString(sink.Topic) {
		return fmt.Errorf("config: release_watch route %q sinks/%d/ntfy/topic: invalid topic %q", routeName, index, sink.Topic)
	}
	if sink.Priority < 1 || sink.Priority > 5 {
		return fmt.Errorf("config: release_watch route %q sinks/%d/ntfy/priority: must be 1..5", routeName, index)
	}
	return nil
}
