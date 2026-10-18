package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/supervisor"
)

func runJobShim(args []string) int {
	return supervisor.RunShim(args)
}

var newJobsSupervisor = supervisor.New

func runJobs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("jobs command required: start, status, stop, supervise or history")
	}
	switch args[0] {
	case "start", "status", "stop", "supervise", "history":
		return runJobsAction(args[0], args[1:])
	}
	return fmt.Errorf("unknown jobs command: %s", args[0])
}

func runJobsAction(action string, args []string) error {
	fs := flag.NewFlagSet("jobs "+action, flag.ContinueOnError)
	configPath := fs.String("config", "", "configuration file")
	pollSeconds := fs.Int("poll-interval-seconds", 1, "supervisor poll interval")
	sinceText := fs.String("since", "", "history: count records written at or after this RFC 3339 time")
	if err := fs.Parse(args); err != nil {
		return flagErr{err: err}
	}
	if *configPath == "" {
		return fmt.Errorf("jobs %s: --config is required", action)
	}
	name, err := jobsNameArg(action, fs.Args())
	if err != nil {
		return err
	}
	since, err := jobsSinceFlag(action, *sinceText)
	if err != nil {
		return err
	}
	loadCtx, cancelLoad := context.WithTimeout(context.Background(), jobsCommandTimeout)
	defer cancelLoad()
	cfg, err := config.Load(loadCtx, *configPath)
	if err != nil {
		return err
	}
	sup, err := newJobsSupervisor(cfg, supervisor.Options{PollInterval: time.Duration(*pollSeconds) * time.Second})
	if err != nil {
		return err
	}
	ctx, cancel := jobsActionContext(action)
	defer cancel()
	if action == "history" {
		return printJobsHistory(ctx, sup, name, since)
	}
	return runLoadedJobsAction(ctx, sup, action, name)
}

// jobsSinceFlag reads --since, which only jobs history takes; without it history counts
// the whole log.
func jobsSinceFlag(action string, text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	if action != "history" {
		return time.Time{}, fmt.Errorf("jobs %s: --since is a jobs history flag", action)
	}
	since, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("jobs history: --since: %w", err)
	}
	return since, nil
}

// jobsCommandTimeout bounds loading the configuration and every jobs command but supervise.
const jobsCommandTimeout = 30 * time.Second

// maxSuperviseRuntime bounds one supervise process, matching the supervisor's job runtime bound.
const maxSuperviseRuntime = 365 * 24 * time.Hour

// jobsActionContext is the context a jobs command runs under. Supervise runs until a stop
// signal (SIGTERM from its host unit, or an interrupt) or its runtime bound; the other
// commands end within jobsCommandTimeout.
func jobsActionContext(action string) (context.Context, context.CancelFunc) {
	if action != "supervise" {
		return context.WithTimeout(context.Background(), jobsCommandTimeout)
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	ctx, cancel := context.WithTimeout(sigCtx, maxSuperviseRuntime)
	return ctx, func() {
		cancel()
		stop()
	}
}

func jobsNameArg(action string, args []string) (string, error) {
	if action == "supervise" && len(args) == 0 {
		return "", nil
	}
	if len(args) > 1 {
		return "", fmt.Errorf("jobs %s: expected at most one name", action)
	}
	if len(args) == 1 {
		return args[0], nil
	}
	return "", nil
}

func runLoadedJobsAction(ctx context.Context, sup *supervisor.Supervisor, action string, name string) error {
	switch action {
	case "start":
		return sup.Start(ctx, name)
	case "stop":
		return sup.Stop(ctx, name)
	case "status":
		return printJobsStatus(ctx, sup, name)
	case "supervise":
		return superviseUntilStopped(ctx, sup)
	}
	return fmt.Errorf("unknown jobs command: %s", action)
}

// superviseUntilStopped supervises until ctx ends. A stop signal is the normal end and
// returns nil; reaching the runtime bound stays an error.
func superviseUntilStopped(ctx context.Context, sup *supervisor.Supervisor) error {
	err := sup.Supervise(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func printJobsStatus(ctx context.Context, sup *supervisor.Supervisor, name string) error {
	statuses, err := sup.Status(ctx, name)
	if err != nil {
		return err
	}
	for i := 0; i < len(statuses); i++ {
		fmt.Println(jobsStatusLine(statuses[i]))
	}
	return nil
}

// jobsStatusLine is one job's status row; a running job whose heartbeat is stale gets a
// last column naming its last beat.
func jobsStatusLine(st supervisor.JobStatus) string {
	line := st.Name + "\t" + string(st.State) + "\t" + strconv.Itoa(st.PID) + "\t" + st.Since + "\t" + strconv.Itoa(st.Restarts) + "\t" + st.Sandbox
	if st.Stale {
		line += "\tstale since " + st.LastBeat
	}
	return line
}

func printJobsHistory(ctx context.Context, sup *supervisor.Supervisor, name string, since time.Time) error {
	histories, err := sup.History(ctx, name, since)
	if err != nil {
		return err
	}
	for i := 0; i < len(histories); i++ {
		fmt.Println(jobsHistoryLine(histories[i]))
	}
	return nil
}

// jobsHistoryLine is one job's history row: name, then the started, exited, unclean,
// restarted, stale, refused and stopped counts, then the first and last counted times.
func jobsHistoryLine(h supervisor.JobHistory) string {
	counts := []int{h.Started, h.Exited, h.Unclean, h.Restarted, h.Stale, h.Refused, h.Stopped}
	fields := make([]string, 0, len(counts)+3)
	fields = append(fields, h.Name)
	for i := 0; i < len(counts); i++ {
		fields = append(fields, strconv.Itoa(counts[i]))
	}
	return strings.Join(append(fields, h.First, h.Last), "\t")
}
