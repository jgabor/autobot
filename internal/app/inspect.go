package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jgabor/autobot/internal/lock"
	"github.com/jgabor/autobot/internal/memory"
	"github.com/jgabor/autobot/internal/record"
)

// Log lists recent runs, optionally for one task.
func (a *App) Log(n int, taskID string) error {
	runs, err := record.List(a.Paths.RunsDir())
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATUS\tDURATION\tROUNDS\tDETAIL")
	shown := 0
	for _, m := range runs {
		if shown >= n {
			break
		}
		if taskID != "" && m.Task != taskID {
			continue
		}
		shown++
		dur := "-"
		if !m.Finished.IsZero() {
			dur = m.Finished.Sub(m.Started).Round(time.Second).String()
		}
		detail := ""
		switch {
		case m.Error != "":
			detail = firstLine(m.Error, 70)
		case m.Output != "":
			detail = a.tilde(m.Output)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", m.ID, m.Status, dur, m.Rounds, detail)
	}
	if shown == 0 {
		a.printf("No runs recorded yet. Run a task with `autobot run <task>`.\n")
		return nil
	}
	return tw.Flush()
}

// Show prints a run's report and transcript. Long messages are clipped
// unless full is set.
func (a *App) Show(query string, full bool) error {
	m, err := record.Find(a.Paths.RunsDir(), query)
	if err != nil {
		return err
	}
	dir := filepath.Join(a.Paths.RunsDir(), m.ID)
	if report, err := os.ReadFile(filepath.Join(dir, "report.md")); err == nil {
		a.printf("%s\n", strings.TrimSpace(string(report)))
	} else {
		a.printf("# %s\n\n(no report: the run did not finish)\n", m.ID)
	}
	msgs, times, err := record.Transcript(dir)
	if err != nil {
		return fmt.Errorf("read transcript: %w", err)
	}
	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if !full && len(s) > 1500 {
			return s[:1500] + fmt.Sprintf("\n[... %d more characters; use --full]", len(s)-1500)
		}
		return s
	}
	a.printf("\n## Transcript\n")
	for i, msg := range msgs {
		at := times[i].Format("15:04:05")
		switch msg.Role {
		case "system":
			if full {
				a.printf("\n[%s] system:\n%s\n", at, clip(msg.Content))
			} else {
				a.printf("\n[%s] system prompt (%d characters; use --full to see it)\n", at, len(msg.Content))
			}
		case "user":
			a.printf("\n[%s] task:\n%s\n", at, clip(msg.Content))
		case "assistant":
			if c := strings.TrimSpace(msg.Content); c != "" {
				a.printf("\n[%s] model:\n%s\n", at, clip(c))
			}
			for _, tc := range msg.ToolCalls {
				a.printf("\n[%s] call %s %s\n", at, tc.Function.Name, clip(tc.Function.Arguments))
			}
		case "tool":
			a.printf("\n[%s] result of %s:\n%s\n", at, msg.Name, clip(msg.Content))
		}
	}
	return nil
}

func (a *App) memoryStore(maxBytes int) *memory.Store {
	return &memory.Store{Dir: a.Paths.MemoryDir(), MaxBytes: maxBytes}
}

// MemoryDiff prints the last n memory changes.
func (a *App) MemoryDiff(n int) error {
	if _, err := os.Stat(a.Paths.MemoryDir()); err != nil {
		a.printf("No memory yet. It is written after the first task run.\n")
		return nil
	}
	diff, err := a.memoryStore(0).Diff(n)
	if err == memory.ErrNoHistory {
		a.printf("No memory changes recorded yet.\n")
		return nil
	}
	if err != nil {
		return err
	}
	a.printf("%s", diff)
	return nil
}

// MemoryUndo reverts the last memory change.
func (a *App) MemoryUndo(force bool) error {
	if err := os.MkdirAll(a.Paths.StateDir, 0o700); err != nil {
		return err
	}
	l, err := lock.Acquire(a.Paths.LockFile())
	if err != nil {
		return err
	}
	defer l.Release()
	msg, err := a.memoryStore(0).Undo(force)
	if err == memory.ErrDirty {
		return fmt.Errorf("%w. Run `autobot memory undo --force` to discard them as well", err)
	}
	if err != nil {
		return err
	}
	a.printf("Reverted: %s\n", msg)
	return nil
}
