package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// detachTemplate is a slot template's job with the sandbox fields given, a deadline and a
// command.
func detachTemplate(sandbox string) func(slotRun) string {
	return func(slotRun) string {
		return `{"name": "slot-1", "command": ["/usr/bin/true"], "max_runtime_seconds": 60, "sandbox": {` + sandbox + `}}`
	}
}

// withDetachStarter replaces the start of a detached run; the returned slice holds every
// command line it was given.
func withDetachStarter(t *testing.T, fail error) *[][]string {
	t.Helper()
	var started [][]string
	old := startDetached
	startDetached = func(_ context.Context, argv []string) ([]byte, error) {
		started = append(started, argv)
		if fail != nil {
			return []byte("Failed to start transient service unit"), fail
		}
		return nil, nil
	}
	t.Cleanup(func() { startDetached = old })
	return &started
}

// A detached run hands the user manager a service of its own, with the template's limits,
// the environment kept as written and every path absolute, and prints where to look.
func TestSlotRunDetachStartsAServiceWithTheTemplatesLimits(t *testing.T) {
	run := newSlotRun(t, detachTemplate(`"memory_max": "512M", "memory_high": "384M"`))
	started := withDetachStarter(t, nil)
	t.Chdir(filepath.Dir(run.template))
	args := slices.Clone(run.args)
	for i := 0; i < len(args); i++ {
		if args[i] == run.template {
			args[i] = filepath.Base(run.template)
		}
	}
	printed, err := captureSlot(t, append([]string{"run", "--detach"}, args[1:]...))
	if err != nil {
		t.Fatalf("slot run --detach = %v, want nil", err)
	}
	if len(*started) != 1 {
		t.Fatalf("started %d services, want 1", len(*started))
	}
	argv := (*started)[0]
	unit := slotServiceName(run.out)
	checkDetachServiceArgv(t, argv, unit, run)
	if want := "service\t" + unit + "\nout\t" + run.out + "\n"; printed != want {
		t.Fatalf("printed %q, want %q", printed, want)
	}
	if _, statErr := os.Lstat(run.out); !os.IsNotExist(statErr) {
		t.Fatalf("the caller made the output directory (%v): the runner makes it", statErr)
	}
}

func checkDetachServiceArgv(t *testing.T, argv []string, unit string, run slotRun) {
	t.Helper()
	head := []string{"systemd-run", "--user", "--quiet", "--collect", "--expand-environment=no", "--unit=" + unit, "-p", "MemoryMax=512M", "-p", "MemoryHigh=384M"}
	if !slices.Equal(argv[:len(head)], head) {
		t.Fatalf("command line starts %q, want %q", argv[:len(head)], head)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}
	split := slices.Index(argv, "--")
	if split < 0 || !slices.Equal(argv[split+1:split+4], []string{self, "slot", "run"}) {
		t.Fatalf("command line %q, want the runner as this executable's slot run after --", argv)
	}
	runner := argv[split+1:]
	if slices.Contains(runner, "--detach") {
		t.Fatalf("runner %q holds --detach: the service would start another service", runner)
	}
	if got := runner[slices.Index(runner, "--config")+1]; got != run.template {
		t.Fatalf("runner's --config = %q, want the absolute %q", got, run.template)
	}
	if !slices.Equal(runner[len(runner)-2:], []string{"--", slotTestUnit}) {
		t.Fatalf("runner ends %q, want -- and the unit", runner[len(runner)-2:])
	}
}

// A detached run hands the service each name in the job's env_allow that the caller has
// set, by name only, so the runner's job gets the same values as an attached run and no value
// stands in the command line. A name the caller left unset, and a variable the template does
// not allow, stay out.
func TestSlotRunDetachForwardsTheAllowedEnvironmentByName(t *testing.T) {
	cases := []struct {
		name, allow string
		want        []string
	}{
		{"set and unset", `"env_allow": ["EPITROP_DETACH_SET", "EPITROP_DETACH_UNSET"], `, []string{"--setenv=EPITROP_DETACH_SET"}},
		{"none allowed", ``, nil},
		{"set but empty", `"env_allow": ["EPITROP_DETACH_EMPTY"], `, []string{"--setenv=EPITROP_DETACH_EMPTY"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EPITROP_DETACH_SET", "value-not-in-argv")
			t.Setenv("EPITROP_DETACH_EMPTY", "")
			t.Setenv("EPITROP_DETACH_OTHER", "other-value")
			run := newSlotRun(t, detachTemplate(tc.allow+`"memory_max": "512M", "memory_high": "384M"`))
			started := withDetachStarter(t, nil)
			if _, err := captureSlot(t, append([]string{"run", "--detach"}, run.args[1:]...)); err != nil {
				t.Fatalf("slot run --detach = %v, want nil", err)
			}
			argv := (*started)[0]
			if got := forwardedEnv(argv); !slices.Equal(got, tc.want) {
				t.Fatalf("forwarded %q, want %q", got, tc.want)
			}
			if i := slices.IndexFunc(argv, func(arg string) bool {
				return strings.Contains(arg, "value-not-in-argv") || strings.Contains(arg, "other-value")
			}); i >= 0 {
				t.Fatalf("command line holds a value: %q", argv[i])
			}
		})
	}
}

// forwardedEnv is the --setenv arguments of a detached command line other than PATH.
func forwardedEnv(argv []string) []string {
	head := argv[:slices.Index(argv, "--")]
	var got []string
	for i := 0; i < len(head); i++ {
		if strings.HasPrefix(head[i], "--setenv=") && !strings.HasPrefix(head[i], "--setenv=PATH=") {
			got = append(got, head[i])
		}
	}
	return got
}

// A template that does not name both limits itself, or names one the loader refuses, starts
// nothing.
func TestSlotRunDetachRefusesATemplateWithoutItsOwnLimits(t *testing.T) {
	cases := []struct {
		name, sandbox, want string
	}{
		{"no sandbox", ``, "sets no sandbox.memory_max"},
		{"memory_max from the default", `"memory_high": "256M"`, "sets no sandbox.memory_max"},
		{"no memory_high", `"memory_max": "512M"`, "sets no sandbox.memory_high"},
		{"empty memory_high", `"memory_max": "512M", "memory_high": ""`, "sets no sandbox.memory_high"},
		{"memory_max no size", `"memory_max": "lots", "memory_high": "256M"`, "sandbox.memory_max: must be positive systemd size"},
		{"env_allow no list", `"memory_max": "512M", "memory_high": "256M", "env_allow": "GOPROXY"`, "sandbox.env_allow is no list"},
		{"env_allow entry no name", `"memory_max": "512M", "memory_high": "256M", "env_allow": ["GOPROXY", 7]`, "sandbox.env_allow/1 is no name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := detachTemplate(tc.sandbox)
			if tc.sandbox == "" {
				job = func(slotRun) string {
					return `{"name": "slot-1", "command": ["/usr/bin/true"], "max_runtime_seconds": 60}`
				}
			}
			run := newSlotRun(t, job)
			started := withDetachStarter(t, nil)
			_, err := captureSlot(t, append([]string{"run", "--detach"}, run.args[1:]...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("slot run --detach = %v, want a refusal naming %q", err, tc.want)
			}
			if len(*started) != 0 {
				t.Fatalf("started %q, want nothing", *started)
			}
		})
	}
}

// The limits hold at the loader's floor of 64M and are refused one step below it.
func TestSlotRunDetachTakesLimitsFromTheFloor(t *testing.T) {
	cases := []struct {
		max, high string
		ok        bool
	}{
		{"64M", "64M", true},
		{"65536K", "65536K", true},
		{"63M", "63M", false},
		{"64M", "65535K", false},
		{"128M", "129M", false},
		{"64G", "64G", true},
		{"65G", "64G", false},
	}
	for _, tc := range cases {
		t.Run(tc.max+"/"+tc.high, func(t *testing.T) {
			run := newSlotRun(t, detachTemplate(fmt.Sprintf(`"memory_max": %q, "memory_high": %q`, tc.max, tc.high)))
			started := withDetachStarter(t, nil)
			_, err := captureSlot(t, append([]string{"run", "--detach"}, run.args[1:]...))
			if tc.ok != (err == nil) {
				t.Fatalf("slot run --detach with %s/%s = %v, want ok=%v", tc.max, tc.high, err, tc.ok)
			}
			if !tc.ok {
				if len(*started) != 0 {
					t.Fatalf("started %q, want nothing", *started)
				}
				return
			}
			argv := (*started)[0]
			if !slices.Contains(argv, "MemoryMax="+tc.max) || !slices.Contains(argv, "MemoryHigh="+tc.high) {
				t.Fatalf("command line %q, want MemoryMax=%s and MemoryHigh=%s", argv, tc.max, tc.high)
			}
		})
	}
}

// A service the user manager refuses is the command's error, with what systemd-run said.
func TestSlotRunDetachNamesARefusedStart(t *testing.T) {
	run := newSlotRun(t, detachTemplate(`"memory_max": "512M", "memory_high": "384M"`))
	withDetachStarter(t, errors.New("exit status 1"))
	_, err := captureSlot(t, append([]string{"run", "--detach"}, run.args[1:]...))
	if err == nil || !strings.Contains(err.Error(), slotServiceName(run.out)) || !strings.Contains(err.Error(), "Failed to start transient service unit") {
		t.Fatalf("slot run --detach = %v, want the service and systemd-run's text", err)
	}
}

// Two output directories give two services; one directory gives one name.
func TestSlotServiceNameFollowsTheOutputDirectory(t *testing.T) {
	a, b := slotServiceName("/srv/runs/a"), slotServiceName("/srv/runs/b")
	if a == b || a != slotServiceName("/srv/runs/a") {
		t.Fatalf("names %q and %q, want distinct names, stable per directory", a, b)
	}
	if !strings.HasPrefix(a, "epitrop-slot-") || !strings.HasSuffix(a, ".service") || len(a) != len("epitrop-slot-")+12+len(".service") {
		t.Fatalf("name %q, want epitrop-slot-<12 hex>.service", a)
	}
}
