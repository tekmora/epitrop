package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/egressproxy"
)

func TestLoadPositiveFullFile(t *testing.T) {
	tmp := t.TempDir()
	cfg, err := Load(testContext(t), writeConfig(t, fmt.Sprintf(`{
  "graph": {"max_depth": 24},
  "sandbox_input_roots": [%s],
  "budgets": {"default": {"tokens": 300000, "wall_clock_seconds": 7200}},
  "jobs": [{
    "name": "daily-catalog-sync",
    "command": ["/bin/echo", "catalog"],
    "log_path": "/var/log/epitrop/daily-catalog-sync.log",
    "schedule": "0 2 * * *",
    "router_alias": "cordana/auto",
    "max_runtime_seconds": 1800,
    "restart": {"policy": "never", "max_restarts": 0, "backoff_seconds": 0},
    "stop": {"signal": "TERM", "grace_seconds": 5},
    "sandbox": {"mode": "enforce", "workspace": %s, "network": "none", "memory_max": "2G", "memory_high": "1536M", "cpu_weight": 100, "tasks_max": 512}
  }],
  "watches": [{"name": "catalog-source-change", "trigger": "git:internal/sources", "router_alias": "cordana/coding"}],
  "router": {"aliases": ["cordana/auto", "cordana/coding"]},
  "admission": {"probe": {"timeout_seconds": 45, "interval_seconds": 3, "ready_threshold": 4}},
  "event_log": {
    "dir": ".epitrop",
    "signing_key_path": "/run/secrets/epitrop-eventlog-seed",
    "public_key": "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
    "lock_timeout_seconds": 7,
    "max_replay_records": 1234
  }
}`, jsonQuote(tmp), jsonQuote(tmp))))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if cfg.Graph.MaxDepth != 24 || cfg.Budgets.Default.Tokens != 300000 {
		t.Fatalf("Load() = %+v, want full file values", cfg)
	}
	assertFullFileJobValues(t, cfg.Jobs)
	if cfg.EventLog.LockTimeoutSeconds != 7 || cfg.EventLog.MaxReplayRecords != 1234 {
		t.Fatalf("Load() event_log = %+v, want full event log values", cfg.EventLog)
	}
}

func assertFullFileJobValues(t *testing.T, jobs []JobConfig) {
	t.Helper()
	if len(jobs) != 1 || jobs[0].RouterAlias != "cordana/auto" {
		t.Fatalf("Load() jobs = %+v, want one scheduled alias declaration", jobs)
	}
	if jobs[0].Command[1] != "catalog" || jobs[0].Restart.Policy != "never" {
		t.Fatalf("Load() jobs = %+v, want command and restart policy", jobs)
	}
	if jobs[0].Sandbox.MemoryHigh != "1536M" {
		t.Fatalf("Load() jobs[0].sandbox.memory_high = %q, want 1536M", jobs[0].Sandbox.MemoryHigh)
	}
	if jobs[0].MaxRuntimeSeconds != 1800 {
		t.Fatalf("Load() jobs[0].max_runtime_seconds = %d, want 1800", jobs[0].MaxRuntimeSeconds)
	}
}

func TestLoadPositiveMinimalFileDefaults(t *testing.T) {
	cfg, err := Load(testContext(t), writeConfig(t, "{}\n"))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("Load() = %+v, want defaults %+v", cfg, Default())
	}
}

func TestLoadNegativeValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr []string
	}{
		{"unknown top-level", `{"extra": true}`, []string{"/", "additional properties"}},
		{"unknown nested", `{"graph": {"extra": true}}`, []string{"/graph", "additional properties"}},
		{"wrong type", `{"graph": {"max_depth": "12"}}`, []string{"/graph/max_depth", "want integer"}},
		{"out of range", `{"graph": {"max_depth": 65}}`, []string{"/graph/max_depth", "maximum"}},
		{"bad alias", `{"router": {"aliases": ["public/alias"]}}`, []string{"/router/aliases/0", "pattern"}},
		{"bad event public key", `{"event_log": {"public_key": "xyz"}}`, []string{"/event_log/public_key", "pattern"}},
		{"negative platform budget", `{"budgets": {"platform": {"frontier_tokens": -1}}}`, []string{"/budgets/platform/frontier_tokens", "minimum"}},
		{"platform budget above 10^12", `{"budgets": {"platform": {"frontier_tokens": 1000000000001}}}`, []string{"/budgets/platform/frontier_tokens", "maximum"}},
		{"unknown platform budget key", `{"budgets": {"platform": {"tokens": 5}}}`, []string{"/budgets/platform", "additional properties"}},
		{"bad replay max", `{"event_log": {"max_replay_records": 1000001}}`, []string{"/event_log/max_replay_records", "maximum"}},
		{"git ref outside refs/", `{"event_log": {"git": {"ref": "heads/events"}}}`, []string{"/event_log/git/ref", "pattern"}},
		{"git ref with a space", `{"event_log": {"git": {"ref": "refs/epitrop/my events"}}}`, []string{"/event_log/git/ref", "pattern"}},
		{"git author with a bracket", `{"event_log": {"git": {"author_name": "a <b>"}}}`, []string{"/event_log/git/author_name", "pattern"}},
		{"git email with a line break", `{"event_log": {"git": {"author_email": "a@b\nc"}}}`, []string{"/event_log/git/author_email", "pattern"}},
		{"git timeout zero", `{"event_log": {"git": {"timeout_seconds": 0}}}`, []string{"/event_log/git/timeout_seconds", "minimum"}},
		{"git timeout above 300", `{"event_log": {"git": {"timeout_seconds": 301}}}`, []string{"/event_log/git/timeout_seconds", "maximum"}},
		{"unknown git key", `{"event_log": {"git": {"sign": true}}}`, []string{"/event_log/git", "additional properties"}},
		{"git enabled without author", `{"event_log": {"git": {"enabled": true, "author_email": "epitrop@example.invalid"}}}`, []string{"event_log/git", "author_name and author_email are required"}},
		{"git enabled without email", `{"event_log": {"git": {"enabled": true, "author_name": "epitrop"}}}`, []string{"event_log/git", "author_name and author_email are required"}},
		{"git compact not a boolean", `{"event_log": {"git": {"compact": "yes"}}}`, []string{"/event_log/git/compact", "boolean"}},
		{"git compact without enabled", `{"event_log": {"git": {"compact": true}}}`, []string{"event_log/git", "compact needs enabled"}},
		{"unknown server transport", `{"server": {"transport": "sse"}}`, []string{"/server/transport", "valid"}},
		{"unknown server key", `{"server": {"allow_writes": true}}`, []string{"/server", "additional properties"}},
		{"server allow_mutations not a boolean", `{"server": {"allow_mutations": "yes"}}`, []string{"/server/allow_mutations", "boolean"}},
		{"server listen on every interface", `{"server": {"listen": "0.0.0.0:8899"}}`, []string{"server/listen", "not loopback"}},
		{"server listen without host", `{"server": {"listen": ":8899"}}`, []string{"server/listen", "not loopback"}},
		{"server listen on a name", `{"server": {"listen": "example.com:8899"}}`, []string{"server/listen", "not loopback"}},
		{"server listen without port", `{"server": {"listen": "127.0.0.1"}}`, []string{"server/listen", "missing port"}},
		{"server listen port above range", `{"server": {"listen": "127.0.0.1:65536"}}`, []string{"server/listen", "not 0..65535"}},
		{"server listen port negative", `{"server": {"listen": "127.0.0.1:-1"}}`, []string{"server/listen", "not 0..65535"}},
		{"bad job restart policy", `{"jobs": [
  {"name": "bad-job", "command": ["/bin/true"], "log_path": "/tmp/bad.log", "schedule": "always", "restart": {"policy": "sometimes"}}
]}`, []string{"/jobs/0/restart/policy", "valid"}},
		{"bad job stop signal", `{"jobs": [
  {"name": "bad-job", "command": ["/bin/true"], "log_path": "/tmp/bad.log", "schedule": "always", "stop": {"signal": "KILL"}}
]}`, []string{"/jobs/0/stop/signal", "valid"}},
		{"job runtime above a year", `{"jobs": [
  {"name": "bad-job", "command": ["/bin/true"], "log_path": "/tmp/bad.log", "max_runtime_seconds": 31536001, "sandbox": {"mode": "off", "reason": "test"}}
]}`, []string{"/jobs/0/max_runtime_seconds", "maximum"}},
		{"job heartbeat above a day", `{"jobs": [
  {"name": "bad-job", "command": ["/bin/true"], "log_path": "/tmp/bad.log", "heartbeat": {"max_age_seconds": 86401}, "sandbox": {"mode": "off", "reason": "test"}}
]}`, []string{"/jobs/0/heartbeat/max_age_seconds", "maximum"}},
		{"negative job heartbeat", `{"jobs": [
  {"name": "bad-job", "command": ["/bin/true"], "log_path": "/tmp/bad.log", "heartbeat": {"max_age_seconds": -1}, "sandbox": {"mode": "off", "reason": "test"}}
]}`, []string{"/jobs/0/heartbeat/max_age_seconds", "minimum"}},
		{"unknown job heartbeat field", `{"jobs": [
  {"name": "bad-job", "command": ["/bin/true"], "log_path": "/tmp/bad.log", "heartbeat": {"interval": 5}, "sandbox": {"mode": "off", "reason": "test"}}
]}`, []string{"/jobs/0/heartbeat", "interval"}},
		{"negative job runtime", `{"jobs": [
  {"name": "bad-job", "command": ["/bin/true"], "log_path": "/tmp/bad.log", "max_runtime_seconds": -1, "sandbox": {"mode": "off", "reason": "test"}}
]}`, []string{"/jobs/0/max_runtime_seconds", "minimum"}},
		{"bad job soft memory limit", `{"jobs": [
  {"name": "bad-job", "command": ["/bin/true"], "log_path": "/tmp/bad.log", "sandbox": {"mode": "off", "reason": "legacy", "memory_high": "lots"}}
]}`, []string{"/jobs/0/sandbox/memory_high", "pattern"}},
		{"bad job sandbox mode", `{"jobs": [
  {"name": "bad-job", "command": ["/bin/true"], "log_path": "/tmp/bad.log", "sandbox": {"mode": "maybe"}}
]}`, []string{"/jobs/0/sandbox/mode", "valid"}},
		{"bad job sandbox env", `{"jobs": [
  {"name": "bad-job", "command": ["/bin/true"], "log_path": "/tmp/bad.log", "sandbox": {"mode": "off", "reason": "legacy", "env_allow": ["bad-name"]}}
]}`, []string{"/jobs/0/sandbox/env_allow/0", "pattern"}},
		{"duplicate job names", `{"jobs": [
  {"name": "repeat-job", "command": ["/bin/true"], "log_path": "/tmp/a.log", "schedule": "0 1 * * *", "router_alias": "cordana/auto", "sandbox": {"mode": "off", "reason": "test"}},
  {"name": "repeat-job", "command": ["/bin/true"], "log_path": "/tmp/b.log", "schedule": "0 2 * * *", "router_alias": "cordana/auto", "sandbox": {"mode": "off", "reason": "test"}}
]}`, []string{"jobs/1/name", "duplicate"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(testContext(t), writeConfig(t, tt.body))
			if err == nil {
				t.Fatal("Load() = nil error, want validation failure")
			}
			assertErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestLoadEventLogGit(t *testing.T) {
	for _, seconds := range []int{1, 300} {
		compact := seconds == 300
		body := fmt.Sprintf(`{"event_log": {"git": {"enabled": true, "ref": "refs/epitrop/log-2.x", "author_name": "epitrop", "author_email": "epitrop@example.invalid", "timeout_seconds": %d, "compact": %t}}}`, seconds, compact)
		cfg, err := Load(testContext(t), writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load(timeout %d) = %v, want nil", seconds, err)
		}
		want := EventLogGitConfig{Enabled: true, Ref: "refs/epitrop/log-2.x", AuthorName: "epitrop", AuthorEmail: "epitrop@example.invalid", TimeoutSeconds: seconds, Compact: compact}
		if cfg.EventLog.Git != want {
			t.Fatalf("Load() event_log.git = %+v, want %+v", cfg.EventLog.Git, want)
		}
	}
}

func TestLoadServerListensOnLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "127.0.0.1:65535", "127.255.0.1:8899", "[::1]:8899", "localhost:8899"} {
		cfg, err := Load(testContext(t), writeConfig(t, fmt.Sprintf(`{"server": {"transport": "http", "listen": %q}}`, addr)))
		if err != nil {
			t.Fatalf("Load(listen %s) = %v, want nil", addr, err)
		}
		if want := (ServerConfig{Transport: TransportHTTP, Listen: addr}); cfg.Server != want {
			t.Fatalf("Load() server = %+v, want %+v", cfg.Server, want)
		}
	}
}

func TestLoadServerAllowsMutationsOnlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{}`, false},
		{`{"server": {"allow_mutations": false}}`, false},
		{`{"server": {"allow_mutations": true}}`, true},
	} {
		cfg, err := Load(testContext(t), writeConfig(t, tc.body))
		if err != nil {
			t.Fatalf("Load(%s) = %v, want nil", tc.body, err)
		}
		if cfg.Server.AllowMutations != tc.want {
			t.Fatalf("Load(%s) server.allow_mutations = %v, want %v", tc.body, cfg.Server.AllowMutations, tc.want)
		}
	}
}

func TestLoadBoundaryMaxDepth(t *testing.T) {
	for _, depth := range []int{1, MaxGraphDepth} {
		body := `{"graph": {"max_depth": ` + jsonInt(depth) + `}}`
		if _, err := Load(testContext(t), writeConfig(t, body)); err != nil {
			t.Fatalf("Load(max_depth=%d) = %v, want nil", depth, err)
		}
	}
	for _, depth := range []int{0, MaxGraphDepth + 1} {
		body := `{"graph": {"max_depth": ` + jsonInt(depth) + `}}`
		if _, err := Load(testContext(t), writeConfig(t, body)); err == nil {
			t.Fatalf("Load(max_depth=%d) = nil error, want bounds failure", depth)
		}
	}
}

func TestLoadBoundaryArrays(t *testing.T) {
	empty := `{"jobs": [], "watches": []}`
	if _, err := Load(testContext(t), writeConfig(t, empty)); err != nil {
		t.Fatalf("Load(empty arrays) = %v, want nil", err)
	}
	maxBody := declarationsJSON(t, MaxJobs, MaxWatches)
	if _, err := Load(testContext(t), writeConfig(t, maxBody)); err != nil {
		t.Fatalf("Load(max arrays) = %v, want nil", err)
	}
	tooMany := declarationsJSON(t, MaxJobs+1, 0)
	if _, err := Load(testContext(t), writeConfig(t, tooMany)); err == nil {
		t.Fatal("Load(too many jobs) = nil error, want maxItems failure")
	}
}

func TestLoadDisablesEnvironmentOverrides(t *testing.T) {
	t.Setenv("APP_GRAPH_MAX_DEPTH", "64")
	t.Setenv("GRAPH", "64")
	cfg, err := Load(testContext(t), writeConfig(t, "{}\n"))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if cfg.Graph.MaxDepth != defaultGraphDepth {
		t.Fatalf("MaxDepth = %d, want env ignored default %d", cfg.Graph.MaxDepth, defaultGraphDepth)
	}
}

// ReadDocument gives the file as it is: no default is added and nothing is refused, so a
// document that Load would refuse can still be read, completed and then loaded.
func TestReadDocumentGivesTheFileUnvalidated(t *testing.T) {
	t.Setenv("APP_GRAPH_MAX_DEPTH", "64")
	path := writeConfig(t, `{"graph": {"max_depth": 7}, "jobs": [{"name": "slot", "command": ["/usr/bin/true"], "sandbox": {"network": "none"}}], "not_in_the_schema": true}`+"\n")
	if _, err := Load(testContext(t), path); err == nil {
		t.Fatal("Load(document with an unknown key) = nil, want it refused: the test needs a document Load refuses")
	}
	doc, err := ReadDocument(testContext(t), path)
	if err != nil {
		t.Fatalf("ReadDocument() = %v, want nil", err)
	}
	assertReadDocumentContent(t, doc)
}

func assertReadDocumentContent(t *testing.T, doc map[string]any) {
	t.Helper()
	graph, isGraph := doc["graph"].(map[string]any)
	jobs, isJobs := doc["jobs"].([]any)
	if !isGraph || !isJobs || len(doc) != 3 || len(graph) != 1 || fmt.Sprint(graph["max_depth"]) != "7" || doc["not_in_the_schema"] != true || len(jobs) != 1 {
		t.Fatalf("ReadDocument() = %v, want the three keys of the file and nothing else", doc)
	}
	assertReadDocumentJob(t, jobs)
}

func assertReadDocumentJob(t *testing.T, jobs []any) {
	t.Helper()
	job, isJob := jobs[0].(map[string]any)
	sandbox, isSandbox := job["sandbox"].(map[string]any)
	if !isJob || !isSandbox || len(job) != 3 || job["name"] != "slot" || len(sandbox) != 1 || sandbox["network"] != "none" {
		t.Fatalf("job = %v, want the job as written, without defaults", job)
	}
}

func TestReadDocumentRefusals(t *testing.T) {
	path := writeConfig(t, "{}\n")
	if _, err := ReadDocument(context.Background(), path); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("ReadDocument(no deadline) = %v, want it refused", err)
	}
	if _, err := ReadDocument(testContext(t), ""); err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("ReadDocument(no path) = %v, want it refused", err)
	}
	assertReadDocumentFileRefusals(t)
	if doc, err := ReadDocument(testContext(t), path); err != nil || len(doc) != 0 {
		t.Fatalf("ReadDocument(empty document) = %v, %v; want an empty document", doc, err)
	}
}

func assertReadDocumentFileRefusals(t *testing.T) {
	t.Helper()
	if _, err := ReadDocument(testContext(t), filepath.Join(t.TempDir(), "missing.json")); err == nil || !strings.Contains(err.Error(), "stat") {
		t.Fatalf("ReadDocument(missing file) = %v, want it refused", err)
	}
	if _, err := ReadDocument(testContext(t), writeConfig(t, "{not json")); err == nil || !strings.Contains(err.Error(), "load") {
		t.Fatalf("ReadDocument(no JSON) = %v, want it refused", err)
	}
}

func TestSchemaFileAgreesWithEmbedded(t *testing.T) {
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "config.schema.json"))
	if err != nil {
		t.Fatalf("ReadFile(docs schema) = %v, want nil", err)
	}
	if string(docs) != string(SchemaJSON()) {
		t.Fatal("docs/config.schema.json differs from embedded internal/config/config.schema.json")
	}
}

func TestConfigSchemaDefaultsAgreeWithGoTypes(t *testing.T) {
	doc := decodeSchema(t)
	def := Default()
	assertSchemaDefault(t, doc, def.Graph.MaxDepth, "graph", "max_depth")
	assertSchemaDefault(t, doc, def.Budgets.Default.Tokens, "budgets", "default", "tokens")
	assertSchemaDefault(t, doc, def.Budgets.Default.WallClockSeconds, "budgets", "default", "wall_clock_seconds")
	assertSchemaDefault(t, doc, def.Budgets.Platform.FrontierTokens, "budgets", "platform", "frontier_tokens")
	assertSchemaDefault(t, doc, def.Admission.Probe.TimeoutSeconds, "admission", "probe", "timeout_seconds")
	assertSchemaDefault(t, doc, def.Admission.Probe.IntervalSeconds, "admission", "probe", "interval_seconds")
	assertSchemaDefault(t, doc, def.Admission.Probe.ReadyThreshold, "admission", "probe", "ready_threshold")
	assertSchemaDefault(t, doc, def.EventLog.Dir, "event_log", "dir")
	assertSchemaDefault(t, doc, def.EventLog.SigningKeyPath, "event_log", "signing_key_path")
	assertSchemaDefault(t, doc, def.EventLog.PublicKey, "event_log", "public_key")
	assertSchemaDefault(t, doc, def.EventLog.LockTimeoutSeconds, "event_log", "lock_timeout_seconds")
	assertSchemaDefault(t, doc, def.EventLog.MaxReplayRecords, "event_log", "max_replay_records")
	assertSchemaDefault(t, doc, def.EventLog.Git.Enabled, "event_log", "git", "enabled")
	assertSchemaDefault(t, doc, def.EventLog.Git.Ref, "event_log", "git", "ref")
	assertSchemaDefault(t, doc, def.EventLog.Git.AuthorName, "event_log", "git", "author_name")
	assertSchemaDefault(t, doc, def.EventLog.Git.AuthorEmail, "event_log", "git", "author_email")
	assertSchemaDefault(t, doc, def.EventLog.Git.TimeoutSeconds, "event_log", "git", "timeout_seconds")
	assertSchemaDefault(t, doc, def.EventLog.Git.Compact, "event_log", "git", "compact")
	assertSchemaDefault(t, doc, def.Server.Transport, "server", "transport")
	assertSchemaDefault(t, doc, def.Server.Listen, "server", "listen")
	assertSchemaDefault(t, doc, def.Server.AllowMutations, "server", "allow_mutations")
	assertSchemaDefault(t, doc, def.Router.Aliases, "router", "aliases")
	assertSchemaDefault(t, doc, def.Jobs, "jobs")
	assertSchemaDefault(t, doc, def.Watches, "watches")
	assertSchemaDefault(t, doc, def.Broker.QueueCapacity, "broker", "queue_capacity")
	assertSchemaDefault(t, doc, def.Broker.DeadLetter.Capacity, "broker", "dead_letter", "capacity")
	assertSchemaDefault(t, doc, def.Broker.DeadLetter.Fallback, "broker", "dead_letter", "fallback")
	assertSchemaDefault(t, doc, def.Broker.DeadLetter.Classes, "broker", "dead_letter", "classes")
}

func TestLoadJobDefaults(t *testing.T) {
	workspace := t.TempDir()
	cfg, err := Load(testContext(t), writeConfig(t, fmt.Sprintf(`{"sandbox_input_roots": [%q], "jobs": [{
  "name": "daemon",
  "command": ["/bin/true"],
  "log_path": "/tmp/daemon.log",
  "sandbox": {"workspace": %q}
}]}`, workspace, workspace)))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	job := cfg.Jobs[0]
	assertJobDefaults(t, job)
}

func assertJobDefaults(t *testing.T, job JobConfig) {
	t.Helper()
	if job.Schedule != "always" || job.RouterAlias != "cordana/auto" || job.Restart.Policy != "never" || job.Stop.Signal != "TERM" {
		t.Fatalf("job defaults = %+v, want schedule, alias, restart and stop defaults", job)
	}
	assertJobSandboxDefaults(t, job.Sandbox)
	if job.MaxRuntimeSeconds != 0 {
		t.Fatalf("max_runtime_seconds default = %d, want 0: no deadline of its own", job.MaxRuntimeSeconds)
	}
}

func assertJobSandboxDefaults(t *testing.T, sb SandboxConfig) {
	t.Helper()
	if sb.Mode != "enforce" || sb.Network != "none" || sb.MemoryMax != "2G" {
		t.Fatalf("sandbox defaults = %+v, want enforce none 2G", sb)
	}
	if sb.MemoryHigh != "" {
		t.Fatalf("memory_high default = %q, want none", sb.MemoryHigh)
	}
}

// TestJobRuntimeBoundIsTheSchemaBound: the Go constant and the schema's maximum are one bound.
func TestJobRuntimeBoundIsTheSchemaBound(t *testing.T) {
	at := fmt.Sprintf(`{"jobs": [{"name": "edge", "command": ["/bin/true"], "log_path": "/tmp/edge.log", "max_runtime_seconds": %d, "sandbox": {"mode": "off", "reason": "test"}}]}`, MaxJobRuntimeSeconds)
	cfg, err := Load(testContext(t), writeConfig(t, at))
	if err != nil || cfg.Jobs[0].MaxRuntimeSeconds != MaxJobRuntimeSeconds {
		t.Fatalf("Load(runtime at the bound %d) = %v, want it accepted", MaxJobRuntimeSeconds, err)
	}
	above := fmt.Sprintf(`{"jobs": [{"name": "edge", "command": ["/bin/true"], "log_path": "/tmp/edge.log", "max_runtime_seconds": %d, "sandbox": {"mode": "off", "reason": "test"}}]}`, MaxJobRuntimeSeconds+1)
	if _, err = Load(testContext(t), writeConfig(t, above)); err == nil {
		t.Fatalf("Load(runtime above the bound) = nil, want the schema to refuse it")
	}
}

// TestJobHeartbeatBoundIsTheSchemaBound: the Go constant and the schema's maximum are one
// bound, and a heartbeat at it loads.
func TestJobHeartbeatBoundIsTheSchemaBound(t *testing.T) {
	body := `{"jobs": [{"name": "edge", "command": ["/bin/true"], "log_path": "/tmp/edge.log", "heartbeat": {"max_age_seconds": %d}, "sandbox": {"mode": "off", "reason": "test"}}]}`
	cfg, err := Load(testContext(t), writeConfig(t, fmt.Sprintf(body, MaxHeartbeatSeconds)))
	if err != nil || cfg.Jobs[0].Heartbeat.MaxAgeSeconds != MaxHeartbeatSeconds {
		t.Fatalf("Load(heartbeat at the bound %d) = %v, want it accepted", MaxHeartbeatSeconds, err)
	}
	if _, err = Load(testContext(t), writeConfig(t, fmt.Sprintf(body, MaxHeartbeatSeconds+1))); err == nil {
		t.Fatalf("Load(heartbeat above the bound) = nil, want the schema to refuse it")
	}
}

func TestExampleConfigValidates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the example uses Unix paths")
	}
	for _, name := range []string{"config.example.yaml", "cutover-1.example.yaml"} {
		if _, err := Load(testContext(t), filepath.Join("..", "..", "docs", name)); err != nil {
			t.Fatalf("Load(docs/%s) = %v, want nil", name, err)
		}
	}
}

type schemaNode struct {
	Default    any                   `json:"default"`
	Properties map[string]schemaNode `json:"properties"`
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(config) = %v, want nil", err)
	}
	return path
}

func assertErrorContains(t *testing.T, err error, parts []string) {
	t.Helper()
	text := err.Error()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Fatalf("error %q does not contain %q", text, part)
		}
	}
}

func jsonInt(v int) string {
	body, err := json.Marshal(v)
	if err != nil {
		return "0"
	}
	return string(body)
}

func declarationsJSON(t *testing.T, jobs int, watches int) string {
	t.Helper()
	doc := map[string]any{
		"jobs":    jobDeclarations(jobs),
		"watches": watchDeclarations(watches),
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal(declarations) = %v, want nil", err)
	}
	return string(body)
}

func jobDeclarations(count int) []map[string]any {
	out := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, map[string]any{
			"name":         "job-" + jsonInt(i),
			"command":      []string{"/bin/true"},
			"log_path":     "/tmp/job-" + jsonInt(i) + ".log",
			"schedule":     "always",
			"router_alias": defaultAlias,
			"sandbox":      map[string]any{"mode": "off", "reason": "test fixture"},
		})
	}
	return out
}

func watchDeclarations(count int) []map[string]any {
	out := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, map[string]any{
			"name":         "watch-" + jsonInt(i),
			"trigger":      "event",
			"router_alias": defaultAlias,
		})
	}
	return out
}

func decodeSchema(t *testing.T) schemaNode {
	t.Helper()
	var doc schemaNode
	if err := json.Unmarshal(SchemaJSON(), &doc); err != nil {
		t.Fatalf("Unmarshal(schema) = %v, want nil", err)
	}
	return doc
}

func assertSchemaDefault(t *testing.T, doc schemaNode, want any, path ...string) {
	t.Helper()
	got := schemaDefault(t, doc, path)
	if !reflect.DeepEqual(normalizeSchemaValue(got), normalizeSchemaValue(want)) {
		t.Fatalf("schema default %s = %#v, want %#v", strings.Join(path, "."), got, want)
	}
}

func schemaDefault(t *testing.T, doc schemaNode, path []string) any {
	t.Helper()
	node := doc
	for _, part := range path {
		next, ok := node.Properties[part]
		if !ok {
			t.Fatalf("schema path %s missing", strings.Join(path, "."))
		}
		node = next
	}
	return node.Default
}

func normalizeSchemaValue(v any) any {
	switch typed := v.(type) {
	case float64:
		return int(typed)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if ok {
				out = append(out, text)
			}
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case []JobConfig:
		return []string{}
	case []WatchConfig:
		return []string{}
	case []DeadLetterClass:
		return []string{}
	default:
		return typed
	}
}

// TestLoadPartialFileKeepsSiblingDefaults: a file that sets one key of a
// section keeps the schema defaults of every key it leaves out.
func TestLoadPartialFileKeepsSiblingDefaults(t *testing.T) {
	cfg, err := Load(testContext(t), writeConfig(t, `{"graph":{"max_depth":5},"admission":{"probe":{"timeout_seconds":9}},"budgets":{"default":{"tokens":7},"platform":{"frontier_tokens":1000000000000}}}`+"\n"))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	want := Default()
	want.Graph.MaxDepth = 5
	want.Admission.Probe.TimeoutSeconds = 9
	want.Budgets.Default.Tokens = 7
	want.Budgets.Platform.FrontierTokens = 1000000000000
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadRefusesReservedSandboxEnv(t *testing.T) {
	workspace := t.TempDir()
	reserved := []string{"PATH", "HOME", "TMPDIR", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "INVOCATION_ID"}
	for i := 0; i < len(reserved); i++ {
		body := fmt.Sprintf(`{"jobs": [{"name": "daemon", "command": ["/bin/true"], "log_path": "/tmp/daemon.log", "sandbox": {"workspace": %q, "env_allow": [%q]}}]}`, workspace, reserved[i])
		if _, err := Load(testContext(t), writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), reserved[i]+" is reserved") {
			t.Fatalf("Load(env_allow %s) = %v, want reserved-name error", reserved[i], err)
		}
	}
}

func TestLoadSandboxFailsClosedWithoutHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sandbox does not run on Windows")
	}
	workspace := t.TempDir()
	enforced := writeConfig(t, fmt.Sprintf(`{"jobs": [{"name": "daemon", "command": ["/bin/true"], "log_path": "/tmp/daemon.log", "sandbox": {"workspace": %q}}]}`, workspace))
	off := writeConfig(t, `{"jobs": [{"name": "daemon", "command": ["/bin/true"], "log_path": "/tmp/daemon.log", "sandbox": {"mode": "off", "reason": "test"}}]}`)
	t.Setenv("HOME", "")
	if _, err := Load(testContext(t), enforced); err == nil || !strings.Contains(err.Error(), "sandbox needs HOME") {
		t.Fatalf("Load(no HOME, enforced sandbox) = %v, want fail-closed HOME error", err)
	}
	if _, err := Load(testContext(t), off); err != nil {
		t.Fatalf("Load(no HOME, sandbox off) = %v, want nil", err)
	}
}

func TestLoadRefusesHomeReachedThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sandbox does not run on Windows")
	}
	home := t.TempDir()
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatalf("Symlink(home) = %v, want nil", err)
	}
	t.Setenv("HOME", link)
	body := fmt.Sprintf(`{"jobs": [{"name": "daemon", "command": ["/bin/true"], "log_path": "/tmp/daemon.log", "sandbox": {"workspace": %q}}]}`, home)
	if _, err := Load(testContext(t), writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "refuse HOME") {
		t.Fatalf("Load(workspace = HOME behind a symlink) = %v, want HOME refusal", err)
	}
}

func TestValidateSandboxRules(t *testing.T) {
	tooMany := make([]string, MaxSandboxEnvAllow+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("NAME_%d", i)
	}
	cases := []struct {
		name string
		edit func(*SandboxConfig)
		want string
	}{
		{"enforce", func(*SandboxConfig) {}, ""},
		{"off with reason", func(s *SandboxConfig) { s.Mode, s.Reason = "off", "legacy" }, ""},
		{"off without reason", func(s *SandboxConfig) { s.Mode, s.Reason = "off", "  " }, "reason: required"},
		{"enforce with reason", func(s *SandboxConfig) { s.Reason = "why" }, "reason: forbidden"},
		{"unknown mode", func(s *SandboxConfig) { s.Mode = "audit" }, "mode: unknown"},
		{"unknown network", func(s *SandboxConfig) { s.Network = "host" }, "network: unknown"},
		{"memory at floor", func(s *SandboxConfig) { s.MemoryMax = "64M" }, ""},
		{"memory below floor", func(s *SandboxConfig) { s.MemoryMax = "65535K" }, "memory_max: must be 64M..64G"},
		{"memory at ceiling", func(s *SandboxConfig) { s.MemoryMax = "64G" }, ""},
		{"memory above ceiling", func(s *SandboxConfig) { s.MemoryMax = "65537M" }, "memory_max: must be 64M..64G"},
		{"memory wraps int64", func(s *SandboxConfig) { s.MemoryMax = "17179869189G" }, "memory_max: is too large"},
		{"memory not a size", func(s *SandboxConfig) { s.MemoryMax = "2X" }, "memory_max: must be positive"},
		{"no soft limit", func(s *SandboxConfig) { s.MemoryHigh = "" }, ""},
		{"soft limit below the hard one", func(s *SandboxConfig) { s.MemoryHigh = "1536M" }, ""},
		{"soft limit equal to the hard one", func(s *SandboxConfig) { s.MemoryHigh = "2G" }, ""},
		{"soft limit one byte above the hard one", func(s *SandboxConfig) { s.MemoryHigh = "2147483649" }, "memory_high: must be 64M..memory_max"},
		{"soft limit at floor", func(s *SandboxConfig) { s.MemoryHigh = "64M" }, ""},
		{"soft limit below floor", func(s *SandboxConfig) { s.MemoryHigh = "65535K" }, "memory_high: must be 64M..memory_max"},
		{"soft limit not a size", func(s *SandboxConfig) { s.MemoryHigh = "1.5G" }, "memory_high: must be positive"},
		{"soft limit wraps int64", func(s *SandboxConfig) { s.MemoryHigh = "17179869189G" }, "memory_high: is too large"},
		{"cpu at floor", func(s *SandboxConfig) { s.CPUWeight = 1 }, ""},
		{"cpu below floor", func(s *SandboxConfig) { s.CPUWeight = -1 }, "cpu_weight"},
		{"cpu at ceiling", func(s *SandboxConfig) { s.CPUWeight = 10000 }, ""},
		{"cpu above ceiling", func(s *SandboxConfig) { s.CPUWeight = 10001 }, "cpu_weight"},
		{"tasks at floor", func(s *SandboxConfig) { s.TasksMax = 16 }, ""},
		{"tasks below floor", func(s *SandboxConfig) { s.TasksMax = 15 }, "tasks_max"},
		{"tasks at ceiling", func(s *SandboxConfig) { s.TasksMax = 32768 }, ""},
		{"tasks above ceiling", func(s *SandboxConfig) { s.TasksMax = 32769 }, "tasks_max"},
		{"env name invalid", func(s *SandboxConfig) { s.EnvAllow = []string{"lower"} }, "invalid name"},
		{"env names at limit", func(s *SandboxConfig) { s.EnvAllow = tooMany[:MaxSandboxEnvAllow] }, ""},
		{"env names over limit", func(s *SandboxConfig) { s.EnvAllow = tooMany }, "env_allow: exceeds"},
	}
	for _, tc := range cases {
		sandbox := SandboxConfig{Mode: "enforce", Network: "none", MemoryMax: "2G", CPUWeight: 100, TasksMax: 512}
		tc.edit(&sandbox)
		err := ValidateSandbox(sandbox)
		if tc.want == "" && err != nil {
			t.Fatalf("%s: ValidateSandbox() = %v, want nil", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: ValidateSandbox() = %v, want error containing %q", tc.name, err, tc.want)
		}
	}
}

func proxyDestinations(count int) []string {
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, fmt.Sprintf("gateway%d.example:443", i))
	}
	return out
}

func TestValidateSandboxProxyRules(t *testing.T) {
	cases := []struct {
		name string
		edit func(*SandboxConfig)
		want string
	}{
		{"one destination", func(*SandboxConfig) {}, ""},
		{"an address as destination", func(s *SandboxConfig) { s.Destinations = []string{"127.0.0.1:4000", "[::1]:4000"} }, ""},
		{"destinations at limit", func(s *SandboxConfig) { s.Destinations = proxyDestinations(egressproxy.MaxDestinations) }, ""},
		{"destinations over limit", func(s *SandboxConfig) { s.Destinations = proxyDestinations(egressproxy.MaxDestinations + 1) }, "destinations: network=proxy needs 1..16"},
		{"no destination", func(s *SandboxConfig) { s.Destinations = nil }, "destinations: network=proxy needs 1..16"},
		{"destinations without the proxy", func(s *SandboxConfig) { s.Network = "none" }, "destinations: forbidden unless network=proxy"},
		{"destinations with open egress", func(s *SandboxConfig) { s.Network = "egress" }, "destinations: forbidden unless network=proxy"},
		{"second spelling", func(s *SandboxConfig) { s.Destinations = []string{"Gateway.example:443"} }, `destinations/0: destination "Gateway.example:443": write it as "gateway.example:443"`},
		{"wildcard", func(s *SandboxConfig) { s.Destinations = []string{"gateway.example:443", "*.example:443"} }, "destinations/1: destination"},
		{"no port", func(s *SandboxConfig) { s.Destinations = []string{"gateway.example"} }, "want host:port"},
		{"named twice", func(s *SandboxConfig) { s.Destinations = []string{"a.example:443", "b.example:443", "a.example:443"} }, "destinations/2: a.example:443 is named twice"},
		{"same host, two ports", func(s *SandboxConfig) { s.Destinations = []string{"a.example:443", "a.example:8443"} }, ""},
		{"HTTPS_PROXY from outside", func(s *SandboxConfig) { s.EnvAllow = []string{"LANG", "HTTPS_PROXY"} }, "env_allow/1: HTTPS_PROXY is set by the sandbox"},
		{"HTTP_PROXY from outside", func(s *SandboxConfig) { s.EnvAllow = []string{"HTTP_PROXY"} }, "env_allow/0: HTTP_PROXY is set by the sandbox"},
		{"ALL_PROXY from outside", func(s *SandboxConfig) { s.EnvAllow = []string{"ALL_PROXY"} }, "env_allow/0: ALL_PROXY is set by the sandbox"},
		{"NO_PROXY from outside", func(s *SandboxConfig) { s.EnvAllow = []string{"NO_PROXY"} }, "env_allow/0: NO_PROXY is set by the sandbox"},
		{"GOMAXPROCS from outside", func(s *SandboxConfig) { s.EnvAllow = []string{"GOMAXPROCS"} }, "env_allow/0: GOMAXPROCS is set by the sandbox"},
		{"GOMAXPROCS without the proxy", func(s *SandboxConfig) { s.Network, s.Destinations, s.EnvAllow = "egress", nil, []string{"GOMAXPROCS"} }, ""},
		{"another name from outside", func(s *SandboxConfig) { s.EnvAllow = []string{"PROXY_TOKEN"} }, ""},
		{"HTTPS_PROXY without the proxy", func(s *SandboxConfig) { s.Network, s.Destinations, s.EnvAllow = "egress", nil, []string{"HTTPS_PROXY"} }, ""},
		{"tasks with room for the relay", func(s *SandboxConfig) { s.TasksMax = 16 + egressproxy.MaxRelayTasks }, ""},
		{"tasks without room for the relay", func(s *SandboxConfig) { s.TasksMax = 15 + egressproxy.MaxRelayTasks }, "tasks_max: network=proxy needs at least 32"},
		{"the same tasks without the proxy", func(s *SandboxConfig) { s.Network, s.Destinations, s.TasksMax = "none", nil, 16 }, ""},
	}
	for _, tc := range cases {
		sandbox := SandboxConfig{Mode: "enforce", Network: "proxy", Destinations: []string{"gateway.example:443"}, MemoryMax: "2G", CPUWeight: 100, TasksMax: 512}
		tc.edit(&sandbox)
		err := ValidateSandbox(sandbox)
		if tc.want == "" && err != nil {
			t.Errorf("%s: ValidateSandbox() = %v, want nil", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: ValidateSandbox() = %v, want error containing %q", tc.name, err, tc.want)
		}
	}
}

func proxyJobConfig(t *testing.T, sandbox string) string {
	t.Helper()
	workspace := t.TempDir()
	return writeConfig(t, fmt.Sprintf(`{"sandbox_input_roots": [%q], "jobs": [{"name": "slot", "command": ["/bin/true"], "log_path": "/tmp/slot.log", "sandbox": {"workspace": %q, %s}}]}`, workspace, workspace, sandbox))
}

// TestLoadProxySandbox: the file's schema and the Go rules accept and refuse the same
// declarations of a job that reaches its destinations through the proxy.
func TestLoadProxySandbox(t *testing.T) {
	cfg, err := Load(testContext(t), proxyJobConfig(t, `"network": "proxy", "destinations": ["gateway.example:443", "127.0.0.1:4000"]`))
	if err != nil {
		t.Fatalf("Load(proxy with two destinations) = %v, want nil", err)
	}
	if got := cfg.Jobs[0].Sandbox; got.Network != "proxy" || !slices.Equal(got.Destinations, []string{"gateway.example:443", "127.0.0.1:4000"}) {
		t.Fatalf("sandbox = %+v, want the proxy and both destinations in the declared order", got)
	}
	most, err := json.Marshal(proxyDestinations(egressproxy.MaxDestinations))
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	if _, err = Load(testContext(t), proxyJobConfig(t, `"network": "proxy", "destinations": `+string(most))); err != nil {
		t.Fatalf("Load(%d destinations) = %v, want nil", egressproxy.MaxDestinations, err)
	}
	refused := []struct {
		name    string
		sandbox string
	}{
		{"one destination over the limit", `"network": "proxy", "destinations": ` + strings.Replace(string(most), "[", `["one.example:443",`, 1)},
		{"proxy without destinations", `"network": "proxy"`},
		{"destinations without the proxy", `"network": "none", "destinations": ["gateway.example:443"]`},
		{"destination named twice", `"network": "proxy", "destinations": ["gateway.example:443", "gateway.example:443"]`},
		{"destination that is no text", `"network": "proxy", "destinations": [443]`},
		{"destination that is a URL", `"network": "proxy", "destinations": ["https://gateway.example:443"]`},
		{"proxy variable from outside", `"network": "proxy", "destinations": ["gateway.example:443"], "env_allow": ["HTTPS_PROXY"]`},
		{"no room for the relay", `"network": "proxy", "destinations": ["gateway.example:443"], "tasks_max": 31`},
	}
	for i := 0; i < len(refused); i++ {
		if _, err = Load(testContext(t), proxyJobConfig(t, refused[i].sandbox)); err == nil {
			t.Errorf("Load(%s) = nil, want it refused", refused[i].name)
		}
	}
}

// TestRelayLifeIsAboveEveryJobRuntime: the relay bounds itself from inside the sandbox; the
// supervisor's deadline and stop grace must always come first.
// A job that opens a connection every second of the longest runtime keeps its way out.
func TestProxyConnectionBoundCoversEveryJobRuntime(t *testing.T) {
	if egressproxy.MaxConnections <= MaxJobRuntimeSeconds {
		t.Fatalf("MaxConnections = %d, want it above the seconds of the longest job runtime (%d)", egressproxy.MaxConnections, MaxJobRuntimeSeconds)
	}
}

func TestRelayLifeIsAboveEveryJobRuntime(t *testing.T) {
	longest := time.Duration(MaxJobRuntimeSeconds+MaxStopGraceSeconds) * time.Second
	if egressproxy.MaxRelayLife <= longest {
		t.Fatalf("MaxRelayLife = %s, want it above the longest job runtime and stop grace (%s)", egressproxy.MaxRelayLife, longest)
	}
}

type sandboxPathWorld struct {
	root, events, keys, key, confDir, home, file, exeDir string
}

func newSandboxPathWorld(t *testing.T) sandboxPathWorld {
	t.Helper()
	// The loader resolves symlinks, so the world does too: its paths then read as the
	// loader's messages print them (on macOS the temporary directory is behind /var).
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temporary directory) = %v, want nil", err)
	}
	w := sandboxPathWorld{root: root, events: filepath.Join(root, "state", "events"), keys: filepath.Join(root, "keys"),
		confDir: filepath.Join(root, "conf"), home: filepath.Join(root, "home", "user"), file: filepath.Join(root, "file")}
	w.key = filepath.Join(w.keys, "seed")
	for _, dir := range []string{w.events, filepath.Join(w.events, "sub"), w.keys, w.confDir, filepath.Join(w.home, "jobs"), filepath.Join(root, "ws")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) = %v, want nil", dir, err)
		}
	}
	for _, file := range []string{w.key, w.file} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) = %v, want nil", file, err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v, want nil", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatalf("EvalSymlinks(executable) = %v, want nil", err)
	}
	w.exeDir = filepath.Dir(exe)
	t.Setenv("HOME", w.home)
	return w
}

// load loads a configuration whose roots are the world's workspace and the jobs directory
// below its HOME.
func (w sandboxPathWorld) load(t *testing.T, workspace string, inputs []string) error {
	t.Helper()
	return w.loadWithRoots(t, []string{filepath.Join(w.root, "ws"), filepath.Join(w.home, "jobs")}, workspace, inputs)
}

func (w sandboxPathWorld) loadWithRoots(t *testing.T, roots []string, workspace string, inputs []string) error {
	t.Helper()
	sandbox, err := json.Marshal(map[string]any{"workspace": workspace, "inputs": inputs})
	if err != nil {
		t.Fatalf("marshal sandbox: %v", err)
	}
	rootList, err := json.Marshal(roots)
	if err != nil {
		t.Fatalf("marshal roots: %v", err)
	}
	body := fmt.Sprintf(`{"sandbox_input_roots": %s, "event_log": {"dir": %q, "signing_key_path": %q}, "jobs": [{"name": "daemon", "command": ["/bin/true"], "log_path": "/tmp/daemon.log", "sandbox": %s}]}`, rootList, w.events, w.key, sandbox)
	path := filepath.Join(w.confDir, "config.json")
	if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(config) = %v, want nil", err)
	}
	_, err = Load(testContext(t), path)
	return err
}

func TestLoadRefusesSandboxPathsReachingTheControlPlane(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sandbox does not run on Windows")
	}
	w := newSandboxPathWorld(t)
	ws := filepath.Join(w.root, "ws")
	cases := []struct {
		name      string
		workspace string
		inputs    []string
		want      string
	}{
		{"own workspace", ws, []string{}, ""},
		{"workspace below HOME", filepath.Join(w.home, "jobs"), []string{}, ""},
		{"no workspace", "", []string{}, "workspace: required"},
		{"root", "/", []string{}, "refuse /"},
		{"relative", "ws", []string{}, "is not absolute"},
		{"missing", filepath.Join(w.root, "missing"), []string{}, "workspace:"},
		{"not a directory", w.file, []string{}, "is not a directory"},
		{"HOME", w.home, []string{}, "refuse HOME"},
		{"directory containing HOME", filepath.Dir(w.home), []string{}, "refuse HOME"},
		{"contains the event log", filepath.Dir(w.events), []string{}, "overlaps protected path"},
		{"inside the event log", filepath.Join(w.events, "sub"), []string{}, "overlaps protected path"},
		{"contains the signing key", w.keys, []string{}, "overlaps protected path"},
		{"contains the config", w.confDir, []string{}, "overlaps protected path"},
		{"contains the executable", w.exeDir, []string{}, "overlaps protected path"},
		{"input contains the signing key", ws, []string{w.keys}, "inputs/0: overlaps protected path"},
		{"input contains HOME", ws, []string{filepath.Dir(w.home)}, "inputs/0: refuse HOME"},
		{"input relative", ws, []string{"rel"}, "inputs/0:"},
	}
	for _, tc := range cases {
		err := w.load(t, tc.workspace, tc.inputs)
		if tc.want == "" && err != nil {
			t.Fatalf("%s: Load() = %v, want nil", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: Load() = %v, want error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestLoadSandboxFailsClosedWithoutExecutable(t *testing.T) {
	old := sandboxExecutable
	t.Cleanup(func() { sandboxExecutable = old })
	sandboxExecutable = func() (string, error) { return "", errors.New("no /proc") }
	workspace := t.TempDir()
	body := fmt.Sprintf(`{"jobs": [{"name": "daemon", "command": ["/bin/true"], "log_path": "/tmp/daemon.log", "sandbox": {"workspace": %q}}]}`, workspace)
	if _, err := Load(testContext(t), writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "sandbox needs the executable path") {
		t.Fatalf("Load(no executable path) = %v, want fail-closed error", err)
	}
}

func TestReleaseWatchPositive(t *testing.T) {
	stateDir := t.TempDir()
	body := fmt.Sprintf(`{
		"release_watch": {
			"state_dir": %s,
			"rate_limit_floor": 150,
			"max_actions_per_run": 25,
			"per_source_cap": 5,
			"routes": [{
				"name": "route-one",
				"sources": [
					{"github": "test-org/test-upstream"},
					{"huggingface_org": "TestHFOrg"}
				],
				"sinks": [
					{"github_issue": {"repo": "test-org/test-consumer", "labels": ["bug", "release"]}},
					{"ntfy": {"topic": "my-topic", "priority": 4}}
				]
			}]
		}
	}`, jsonQuote(stateDir))
	cfg, err := Load(testContext(t), writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	rw := cfg.ReleaseWatch
	if rw.StateDir != stateDir || rw.RateLimitFloor != 150 || rw.MaxActionsPerRun != 25 || rw.PerSourceCap != 5 {
		t.Fatalf("ReleaseWatch = %+v, want configured values", rw)
	}
	if len(rw.Routes) != 1 || rw.Routes[0].Name != "route-one" {
		t.Fatalf("Routes = %+v, want route-one", rw.Routes)
	}
	if len(rw.Routes[0].Sources) != 2 || len(rw.Routes[0].Sinks) != 2 {
		t.Fatalf("Route sources/sinks len mismatch: %+v", rw.Routes[0])
	}
}

func TestReleaseWatchNegative(t *testing.T) {
	st := jsonQuote(t.TempDir())
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "route with empty sinks",
			body:    fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "route-a", "sources": [{"github": "owner/repo"}], "sinks": []}]}}`, st),
			wantErr: "sinks",
		},
		{
			name:    "github_issue without repo",
			body:    fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "route-a", "sources": [{"github": "owner/repo"}], "sinks": [{"github_issue": {"repo": ""}}]}]}}`, st),
			wantErr: "repo",
		},
		{
			name:    "repo not owner/name",
			body:    fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "route-a", "sources": [{"github": "owner/repo"}], "sinks": [{"github_issue": {"repo": "no-slash-here"}}]}]}}`, st),
			wantErr: "pattern",
		},
		{
			name:    "sink repo of dot segments",
			body:    fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "route-a", "sources": [{"github": "owner/repo"}], "sinks": [{"github_issue": {"repo": "../.."}}]}]}}`, st),
			wantErr: "invalid repository",
		},
		{
			name:    "source repo name ..",
			body:    fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "route-a", "sources": [{"github": "owner/.."}], "sinks": [{"github_issue": {"repo": "owner/consumer"}}]}]}}`, st),
			wantErr: "invalid repository",
		},
		{
			name:    "source owner .",
			body:    fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "route-a", "sources": [{"github": "./repo"}], "sinks": [{"github_issue": {"repo": "owner/consumer"}}]}]}}`, st),
			wantErr: "invalid repository",
		},
		{
			name:    "huggingface org ..",
			body:    fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "route-a", "sources": [{"huggingface_org": ".."}], "sinks": [{"github_issue": {"repo": "owner/consumer"}}]}]}}`, st),
			wantErr: "invalid org",
		},
		{
			name:    "source with zero kinds",
			body:    fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "route-a", "sources": [{}], "sinks": [{"github_issue": {"repo": "owner/consumer"}}]}]}}`, st),
			wantErr: "exactly one of github or huggingface_org",
		},
		{
			name:    "source with two kinds",
			body:    fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "route-a", "sources": [{"github": "owner/repo", "huggingface_org": "SomeOrg"}], "sinks": [{"github_issue": {"repo": "owner/consumer"}}]}]}}`, st),
			wantErr: "exactly one of github or huggingface_org",
		},
		{
			name: "duplicate route names",
			body: fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [
				{"name": "route-dup", "sources": [{"github": "owner/repo"}], "sinks": [{"github_issue": {"repo": "owner/consumer"}}]},
				{"name": "route-dup", "sources": [{"github": "owner/other"}], "sinks": [{"github_issue": {"repo": "owner/consumer"}}]}
			]}}`, st),
			wantErr: "duplicate route name",
		},
		{
			name:    "state_dir relative when routes non-empty",
			body:    `{"release_watch": {"state_dir": "relative/state", "routes": [{"name": "route-a", "sources": [{"github": "owner/repo"}], "sinks": [{"github_issue": {"repo": "owner/consumer"}}]}]}}`,
			wantErr: "state_dir",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(testContext(t), writeConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load(%s) = %v, want error containing %q", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestReleaseWatchBoundaryNumericCaps(t *testing.T) {
	st := jsonQuote(t.TempDir())
	for _, maxActions := range []int{1, 100} {
		body := fmt.Sprintf(`{"release_watch": {"state_dir": %s, "max_actions_per_run": %d, "routes": [{"name": "r", "sources": [{"github": "a/b"}], "sinks": [{"github_issue": {"repo": "c/d"}}]}]}}`, st, maxActions)
		if _, err := Load(testContext(t), writeConfig(t, body)); err != nil {
			t.Fatalf("Load(max_actions_per_run=%d) = %v, want nil", maxActions, err)
		}
	}
	for _, maxActions := range []int{0, 101} {
		body := fmt.Sprintf(`{"release_watch": {"state_dir": %s, "max_actions_per_run": %d, "routes": [{"name": "r", "sources": [{"github": "a/b"}], "sinks": [{"github_issue": {"repo": "c/d"}}]}]}}`, st, maxActions)
		if _, err := Load(testContext(t), writeConfig(t, body)); err == nil {
			t.Fatalf("Load(max_actions_per_run=%d) = nil, want boundary error", maxActions)
		}
	}
	for _, perSource := range []int{1, 20} {
		body := fmt.Sprintf(`{"release_watch": {"state_dir": %s, "per_source_cap": %d, "routes": [{"name": "r", "sources": [{"github": "a/b"}], "sinks": [{"github_issue": {"repo": "c/d"}}]}]}}`, st, perSource)
		if _, err := Load(testContext(t), writeConfig(t, body)); err != nil {
			t.Fatalf("Load(per_source_cap=%d) = %v, want nil", perSource, err)
		}
	}
	for _, perSource := range []int{0, 21} {
		body := fmt.Sprintf(`{"release_watch": {"state_dir": %s, "per_source_cap": %d, "routes": [{"name": "r", "sources": [{"github": "a/b"}], "sinks": [{"github_issue": {"repo": "c/d"}}]}]}}`, st, perSource)
		if _, err := Load(testContext(t), writeConfig(t, body)); err == nil {
			t.Fatalf("Load(per_source_cap=%d) = nil, want boundary error", perSource)
		}
	}
}

func TestReleaseWatchBoundaryArrayCaps(t *testing.T) {
	st := jsonQuote(t.TempDir())
	labels8 := `["l1","l2","l3","l4","l5","l6","l7","l8"]`
	body8 := fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "r", "sources": [{"github": "a/b"}], "sinks": [{"github_issue": {"repo": "c/d", "labels": %s}}]}]}}`, st, labels8)
	if _, err := Load(testContext(t), writeConfig(t, body8)); err != nil {
		t.Fatalf("Load(labels=8) = %v, want nil", err)
	}
	labels9 := `["l1","l2","l3","l4","l5","l6","l7","l8","l9"]`
	body9 := fmt.Sprintf(`{"release_watch": {"state_dir": %s, "routes": [{"name": "r", "sources": [{"github": "a/b"}], "sinks": [{"github_issue": {"repo": "c/d", "labels": %s}}]}]}}`, st, labels9)
	if _, err := Load(testContext(t), writeConfig(t, body9)); err == nil {
		t.Fatal("Load(labels=9) = nil error, want error")
	}
	emptyRoutes := `{"release_watch": {"state_dir": "", "routes": []}}`
	if _, err := Load(testContext(t), writeConfig(t, emptyRoutes)); err != nil {
		t.Fatalf("Load(empty routes with empty state_dir) = %v, want nil", err)
	}
}

func TestLoadRefusesARouterAliasOutsideThePattern(t *testing.T) {
	bad := []string{"openai/gpt-4", "cordana/", "cordana/Auto", "cordana/9x"}
	for _, alias := range bad {
		t.Run("job_"+alias, func(t *testing.T) {
			body := fmt.Sprintf(`{"jobs":[{"name":"j","command":["/bin/true"],"log_path":"/tmp/j.log","router_alias":%q,"sandbox":{"mode":"off","reason":"test"}}]}`, alias)
			_, err := Load(testContext(t), writeConfig(t, body))
			if err == nil || !strings.Contains(err.Error(), "pattern") {
				t.Fatalf("Load(job router_alias %q) err = %v, want pattern failure", alias, err)
			}
		})
		t.Run("watch_"+alias, func(t *testing.T) {
			body := fmt.Sprintf(`{"watches":[{"name":"w","trigger":"event","router_alias":%q}]}`, alias)
			_, err := Load(testContext(t), writeConfig(t, body))
			if err == nil || !strings.Contains(err.Error(), "pattern") {
				t.Fatalf("Load(watch router_alias %q) err = %v, want pattern failure", alias, err)
			}
		})
	}
	t.Run("boundary_cordana_a", func(t *testing.T) {
		body := `{"jobs":[{"name":"j","command":["/bin/true"],"log_path":"/tmp/j.log","router_alias":"cordana/a","sandbox":{"mode":"off","reason":"test"}}],"watches":[{"name":"w","trigger":"event","router_alias":"cordana/a"}],"router":{"aliases":["cordana/a"]}}`
		if _, err := Load(testContext(t), writeConfig(t, body)); err != nil {
			t.Fatalf("Load(boundary cordana/a) = %v, want nil error", err)
		}
	})
}

func TestLoadRefusesARouterAliasNotInRouterAliases(t *testing.T) {
	t.Run("listed alias passes", func(t *testing.T) {
		body := `{"jobs":[{"name":"j","command":["/bin/true"],"log_path":"/tmp/j.log","router_alias":"cordana/coding","sandbox":{"mode":"off","reason":"test"}}],"watches":[{"name":"w","trigger":"event","router_alias":"cordana/coding"}],"router":{"aliases":["cordana/coding"]}}`
		if _, err := Load(testContext(t), writeConfig(t, body)); err != nil {
			t.Fatalf("Load() = %v, want nil", err)
		}
	})
	t.Run("unlisted job alias fails", func(t *testing.T) {
		body := `{"jobs":[{"name":"j","command":["/bin/true"],"log_path":"/tmp/j.log","router_alias":"cordana/coding","sandbox":{"mode":"off","reason":"test"}}],"router":{"aliases":["cordana/auto"]}}`
		_, err := Load(testContext(t), writeConfig(t, body))
		wantErr := `config: job "j" router_alias "cordana/coding" is not listed in router.aliases`
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("Load() = %v, want error %q", err, wantErr)
		}
	})
	t.Run("unlisted watch alias fails", func(t *testing.T) {
		body := `{"watches":[{"name":"w","trigger":"event","router_alias":"cordana/coding"}],"router":{"aliases":["cordana/auto"]}}`
		_, err := Load(testContext(t), writeConfig(t, body))
		wantErr := `config: watch "w" router_alias "cordana/coding" is not listed in router.aliases`
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("Load() = %v, want error %q", err, wantErr)
		}
	})
	t.Run("defaulted job alias with router.aliases missing cordana/auto fails", func(t *testing.T) {
		body := `{"jobs":[{"name":"j","command":["/bin/true"],"log_path":"/tmp/j.log","sandbox":{"mode":"off","reason":"test"}}],"router":{"aliases":["cordana/custom"]}}`
		_, err := Load(testContext(t), writeConfig(t, body))
		wantErr := `config: job "j" router_alias "cordana/auto" is not listed in router.aliases`
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("Load() = %v, want error %q", err, wantErr)
		}
	})
}

func TestValidateNtfySink(t *testing.T) {
	for _, p := range []int{1, 5} {
		sink := &NtfySink{Topic: "valid-topic_1.0", Priority: p}
		if err := validateNtfySink("route-a", 0, sink); err != nil {
			t.Fatalf("validateNtfySink(priority=%d) = %v, want nil", p, err)
		}
	}

	for _, p := range []int{0, 6} {
		sink := &NtfySink{Topic: "valid-topic", Priority: p}
		err := validateNtfySink("route-a", 0, sink)
		if err == nil || !strings.Contains(err.Error(), "must be 1..5") {
			t.Fatalf("validateNtfySink(priority=%d) = %v, want 'must be 1..5'", p, err)
		}
	}

	checkValidateNtfySinkTopicRefusals(t)
}

func checkValidateNtfySinkTopicRefusals(t *testing.T) {
	t.Helper()
	for _, topic := range []string{"", "   "} {
		sink := &NtfySink{Topic: topic, Priority: 3}
		err := validateNtfySink("route-a", 0, sink)
		if err == nil || !strings.Contains(err.Error(), "topic: required") {
			t.Fatalf("validateNtfySink(topic=%q) = %v, want 'topic: required'", topic, err)
		}
	}

	for _, topic := range []string{"topic with spaces", "topic/slash", "topic#hash", "topic@at"} {
		sink := &NtfySink{Topic: topic, Priority: 3}
		err := validateNtfySink("route-a", 0, sink)
		if err == nil || !strings.Contains(err.Error(), "invalid topic") {
			t.Fatalf("validateNtfySink(topic=%q) = %v, want 'invalid topic'", topic, err)
		}
	}
}

// Both limits are required, each within the bounds a sandbox's limits keep.
func TestCheckMemoryLimits(t *testing.T) {
	cases := []struct {
		max, high, want string
	}{
		{"64M", "64M", ""},
		{"2G", "1536M", ""},
		{"64G", "64G", ""},
		{"512M", "", "memory_high: is required"},
		{"", "256M", "memory_max: is required"},
		{"63M", "63M", "memory_max: must be 64M..64G"},
		{"65G", "64G", "memory_max: must be 64M..64G"},
		{"128M", "65535K", "memory_high: must be 64M..memory_max"},
		{"128M", "129M", "memory_high: must be 64M..memory_max"},
		{"128M", "lots", "memory_high: must be positive systemd size"},
	}
	for _, tc := range cases {
		err := CheckMemoryLimits(tc.max, tc.high)
		if tc.want == "" && err != nil {
			t.Errorf("CheckMemoryLimits(%q, %q) = %v, want nil", tc.max, tc.high, err)
		}
		if tc.want != "" && (err == nil || err.Error() != tc.want) {
			t.Errorf("CheckMemoryLimits(%q, %q) = %v, want %q", tc.max, tc.high, err, tc.want)
		}
	}
}

func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
