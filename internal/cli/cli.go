// Package cli parses autobot's command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/jgabor/autobot/internal/app"
	"github.com/jgabor/autobot/internal/paths"
)

// Version is set at build time with -ldflags "-X github.com/jgabor/autobot/internal/cli.Version=...".
var Version = "dev"

const usage = `autobot runs recurring chores with an AI model.

Usage:
  autobot init                     Create config, a system prompt, an example task and the systemd timer
  autobot check [--offline]        Test config, provider, key and tool calling; list tasks
  autobot new <name>               Create a task from a template
  autobot run <task>... [--dry-run]
                                   Run tasks now. --dry-run writes nothing and shows intended writes as diffs
  autobot run --due                Run every task that is due (used by the timer)
  autobot log [-n N] [task]        List recent runs
  autobot show [run] [--full]      Show a run's report and transcript (default: the latest run)
  autobot memory diff [-n N]       Show the last memory changes
  autobot memory undo [--force]    Revert the last memory change
  autobot migrate [--from DIR]     Convert a v1 setup
  autobot version                  Print the version

Files:
  ~/.config/autobot/config.toml    provider, model and defaults
  ~/.config/autobot/tasks/*.md     one task per file
  ~/.local/state/autobot/inbox/    task outputs and digests
`

// Main runs one command and returns the exit code: 0 success, 1 failure,
// 2 usage error.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "autobot", Version)
		return 0
	}
	p, err := paths.FromEnv()
	if err != nil {
		fmt.Fprintln(stderr, "autobot:", err)
		return 1
	}
	a := app.New(p, stdout, stderr)
	if err := a.LoadEnvironment(); err != nil {
		fmt.Fprintf(stderr, "autobot: %s: %v\n", a.EnvironmentFile(), err)
		return 1
	}
	return Run(ctx, a, cmd, rest)
}

// Run dispatches a command on an App. Tests call it directly.
func Run(ctx context.Context, a *app.App, cmd string, args []string) int {
	err := dispatch(ctx, a, cmd, args)
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(a.Err, "autobot: %v\nRun `autobot help` for usage.\n", err)
		return 2
	case errors.Is(err, app.ErrFailed), errors.Is(err, app.ErrCheckFailed):
		return 1
	default:
		fmt.Fprintln(a.Err, "autobot:", err)
		return 1
	}
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// parse accepts flags before, between and after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usagef("%s: %v", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func dispatch(ctx context.Context, a *app.App, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	switch cmd {
	case "init":
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return usagef("init takes no arguments")
		}
		return a.Init()
	case "check":
		offline := fs.Bool("offline", false, "skip the provider request")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return usagef("check takes no arguments")
		}
		return a.Check(ctx, *offline)
	case "new":
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef("usage: autobot new <name>")
		}
		return a.NewTask(pos[0])
	case "run":
		due := fs.Bool("due", false, "run every due task")
		dry := fs.Bool("dry-run", false, "write nothing; show intended writes")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if *due == (len(pos) > 0) {
			return usagef("usage: autobot run <task>... [--dry-run], or autobot run --due")
		}
		if *due && *dry {
			return usagef("--dry-run works with named tasks, not --due")
		}
		return a.Run(ctx, app.RunOptions{Tasks: pos, Due: *due, DryRun: *dry})
	case "log":
		n := fs.Int("n", 20, "number of runs")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 1 || *n < 1 {
			return usagef("usage: autobot log [-n N] [task]")
		}
		taskID := ""
		if len(pos) == 1 {
			taskID = pos[0]
		}
		return a.Log(*n, taskID)
	case "show":
		full := fs.Bool("full", false, "show everything, including the system prompt")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 1 {
			return usagef("usage: autobot show [run] [--full]")
		}
		query := ""
		if len(pos) == 1 {
			query = pos[0]
		}
		return a.Show(query, *full)
	case "memory":
		if len(args) == 0 {
			return usagef("usage: autobot memory diff [-n N], or autobot memory undo [--force]")
		}
		sub := args[0]
		fs = flag.NewFlagSet("memory "+sub, flag.ContinueOnError)
		switch sub {
		case "diff":
			n := fs.Int("n", 1, "number of changes")
			pos, err := parse(fs, args[1:])
			if err != nil {
				return err
			}
			if len(pos) > 0 || *n < 1 {
				return usagef("usage: autobot memory diff [-n N]")
			}
			return a.MemoryDiff(*n)
		case "undo":
			force := fs.Bool("force", false, "also discard memory edits that are not committed")
			pos, err := parse(fs, args[1:])
			if err != nil {
				return err
			}
			if len(pos) > 0 {
				return usagef("usage: autobot memory undo [--force]")
			}
			return a.MemoryUndo(*force)
		}
		return usagef("unknown memory command %q", sub)
	case "migrate":
		from := fs.String("from", "", "v1 config folder (default: ~/.config/autobot)")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return usagef("usage: autobot migrate [--from DIR]")
		}
		return a.Migrate(*from)
	}
	return usagef("unknown command %q", cmd)
}
