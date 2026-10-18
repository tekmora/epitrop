// Command epitropctl syncs the model data Epitrop routes on from a handful
// of local and remote sources into one snapshot file, and can render that
// snapshot as a table. See the catalog package for the record shape and
// docs/data-sync.md for installing epitropctl and for what each source does
// and does not know.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	golusorisotel "github.com/golusoris/golusoris/otel"
	"github.com/tekmora/epitrop/internal/egressproxy"
)

var version = ""

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		printUsage()
		return 1
	}

	switch os.Args[1] {
	case "__job-shim":
		return runJobShim(os.Args[1:])
	case egressproxy.RelayCommand:
		return egressproxy.RunRelay(os.Args[1:], os.Stderr)
	}

	opts := golusorisotel.DefaultOptions()
	opts.Service.Name = "epitrop"
	opts.Service.Version = appVersion()
	opts.Export.Logs = false

	initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
	providers, err := golusorisotel.New(initCtx, opts)
	initCancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry init: %v\n", err)
	} else if providers != nil {
		defer func() {
			sCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if sErr := providers.Shutdown(sCtx); sErr != nil {
				fmt.Fprintf(os.Stderr, "telemetry shutdown: %v\n", sErr)
			}
		}()
	}

	cmdErr := dispatchCommand(os.Args[1], os.Args[2:])
	if cmdErr != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", cmdErr)
		var exitCoder interface{ ExitCode() int }
		if errors.As(cmdErr, &exitCoder) {
			return exitCoder.ExitCode()
		}
		return 1
	}
	return 0
}

var plainCommands = map[string]func([]string) error{
	"sync":     runSync,
	"jobs":     runJobs,
	"watch":    runWatch,
	"units":    runUnits,
	"graph":    runGraph,
	"effects":  runEffects,
	"slot":     runSlot,
	"show":     runShow,
	"headroom": runHeadroom,
}

func dispatchCommand(cmd string, args []string) error {
	if fn, ok := plainCommands[cmd]; ok {
		return fn(args)
	}
	switch cmd {
	case "schema":
		return runSchema(os.Stdout)
	case "version":
		printVersion()
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func printUsage() {
	fmt.Println("epitropctl - model data sync for the Epitrop graph router")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  epitropctl sync [--sources=a,b] [--out=file] [--litellm-base=url] [--litellm-token-file=path] [--ollama=url]")
	fmt.Println("  epitropctl show [--in=file]")
	fmt.Println("  epitropctl headroom --max-age=duration [--in=file]")
	fmt.Println("  epitropctl schema")
	fmt.Println("  epitropctl version")
	fmt.Println("  epitropctl jobs start|status|stop|supervise --config=file [name]")
	fmt.Println("  epitropctl jobs history --config=file [--since=RFC3339] [name]")
	fmt.Println("  epitropctl watch run|seed --config=file [--once] [--interval=duration] [--github-base=url] [--huggingface-base=url] [--ntfy-base=url]")
	fmt.Println("  epitropctl units set|resume|show|note|inbox|preempt|handover|frontier|assign|release --config=file ...")
	fmt.Println("  epitropctl graph show|apply --config=file [--file=stage.json]")
	fmt.Println("  epitropctl effects list|resolve --config=file [--outcome=done|retry --by=name <task id> <action type>]")
	fmt.Println("  epitropctl slot run --config=template --base=rev --brief=file --workspace=dir --out=dir [--repo=dir] [--resume=file] [--decisions=file] [--detach] <unit id>")
	fmt.Println("  epitropctl slot resume --out=dir [--repo=dir] [unit id]")
}

func appVersion() string {
	if version != "" {
		return version
	}
	return "dev"
}

func printVersion() {
	fmt.Println(appVersion())
}
