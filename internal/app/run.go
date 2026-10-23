package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jgabor/autobot/internal/access"
	"github.com/jgabor/autobot/internal/agent"
	"github.com/jgabor/autobot/internal/config"
	"github.com/jgabor/autobot/internal/inbox"
	"github.com/jgabor/autobot/internal/lock"
	"github.com/jgabor/autobot/internal/memory"
	"github.com/jgabor/autobot/internal/provider"
	"github.com/jgabor/autobot/internal/record"
	"github.com/jgabor/autobot/internal/reflection"
	"github.com/jgabor/autobot/internal/schedule"
	"github.com/jgabor/autobot/internal/task"
	"github.com/jgabor/autobot/internal/tools"
	"github.com/pmezard/go-difflib/difflib"
)

// ErrFailed means at least one task failed. Details are already printed.
var ErrFailed = errors.New("one or more tasks failed")

// RunOptions select what to run.
type RunOptions struct {
	Tasks  []string
	Due    bool
	DryRun bool
}

// Run runs the named tasks, or every due task.
func (a *App) Run(ctx context.Context, o RunOptions) error {
	if o.Due == (len(o.Tasks) > 0) {
		return errors.New("name the tasks to run, or use --due")
	}
	if o.DryRun && o.Due {
		return errors.New("--dry-run works with named tasks, not --due")
	}
	cfg, err := a.LoadConfig()
	if err != nil {
		if o.Due {
			a.reportSetupError(err)
		}
		return err
	}
	if !o.DryRun {
		if err := os.MkdirAll(a.Paths.StateDir, 0o700); err != nil {
			return err
		}
		l, err := lock.Acquire(a.Paths.LockFile())
		if err != nil {
			return err
		}
		defer l.Release()
	}

	tasks, invalid, err := task.LoadAll(a.Paths.TasksDir(), taskDefaults(cfg))
	if err != nil {
		if o.Due {
			a.reportSetupError(err)
		}
		return err
	}
	state, err := schedule.LoadState(a.Paths.ScheduleFile())
	if err != nil {
		return err
	}
	now := a.Now()

	var selected []*task.Task
	var entries []inbox.Entry
	if o.Due {
		for _, t := range tasks {
			if t.Enabled && schedule.Due(t.Schedule, state.Tasks[t.ID].LastRun, now) {
				selected = append(selected, t)
			}
		}
		// Report a broken task file once per distinct error, not every check.
		for _, inv := range invalid {
			status := "invalid: " + inv.Err.Error()
			if state.Tasks[inv.ID].LastStatus != status {
				entries = append(entries, inbox.Entry{Task: inv.ID, Status: "invalid", Note: inv.Err.Error()})
				ts := state.Tasks[inv.ID]
				ts.LastStatus = status
				state.Tasks[inv.ID] = ts
			}
		}
	} else {
		byID := map[string]*task.Task{}
		for _, t := range tasks {
			byID[t.ID] = t
		}
		bad := map[string]error{}
		for _, inv := range invalid {
			bad[inv.ID] = inv.Err
		}
		for _, name := range o.Tasks {
			name = strings.TrimSuffix(name, ".md")
			if err := bad[name]; err != nil {
				return err
			}
			t := byID[name]
			if t == nil {
				return fmt.Errorf("no task named %q in %s (see `autobot check`, or create it with `autobot new %s`)", name, a.tilde(a.Paths.TasksDir()), name)
			}
			if !t.Enabled {
				fmt.Fprintf(a.Err, "note: %s is disabled; running it because it was named\n", t.ID)
			}
			selected = append(selected, t)
		}
	}
	if len(selected) == 0 && len(entries) == 0 {
		if o.Due {
			a.printf("Nothing is due.\n")
		}
		return nil
	}

	// The key is only needed when a task runs, not to report a broken file.
	var key string
	var keyErr error
	if len(selected) > 0 {
		key, keyErr = cfg.Provider.APIKey(ctx)
	}
	prov := a.NewProvider(cfg.Provider, key)
	redact := record.Redactor(a.secrets(cfg, key))

	failed := false
	for _, t := range selected {
		if ctx.Err() != nil {
			entries = append(entries, inbox.Entry{Task: t.ID, Status: "skipped", Note: "autobot was stopped before this task ran"})
			failed = true
			continue
		}
		prev := state.Tasks[t.ID]
		if !o.DryRun {
			// Count the run before it starts, so a task that crashes
			// autobot is not started again at every check.
			state.Tasks[t.ID] = schedule.TaskState{LastRun: now, LastStatus: "running"}
			if err := state.Save(a.Paths.ScheduleFile()); err != nil {
				fmt.Fprintf(a.Err, "could not save schedule state: %v\n", err)
			}
		}
		e, setupFailed := a.safeRunTask(ctx, cfg, prov, keyErr, t, o.DryRun, redact)
		failed = failed || e.Status != "completed"
		if o.DryRun {
			entries = append(entries, e)
			continue
		}
		if setupFailed {
			// The provider or its key failed, so the task did not really
			// run. Keep its last run time so the next check retries it, and
			// report the same failure only once while it lasts.
			status := "failed: " + e.Note
			if !o.Due || prev.LastStatus != status {
				entries = append(entries, e)
			}
			state.Tasks[t.ID] = schedule.TaskState{LastRun: prev.LastRun, LastStatus: status}
		} else {
			entries = append(entries, e)
			// The check's start time is what the schedule counts from, so long
			// tasks do not push later tasks back a little more every day.
			state.Tasks[t.ID] = schedule.TaskState{LastRun: now, LastStatus: e.Status}
		}
		if err := state.Save(a.Paths.ScheduleFile()); err != nil {
			fmt.Fprintf(a.Err, "could not save schedule state: %v\n", err)
		}
	}
	if o.DryRun {
		if failed {
			return ErrFailed
		}
		return nil
	}
	if err := state.Save(a.Paths.ScheduleFile()); err != nil {
		fmt.Fprintf(a.Err, "could not save schedule state: %v\n", err)
	}
	if _, err := record.Prune(a.Paths.RunsDir(), time.Duration(cfg.Records.KeepDays)*24*time.Hour, a.Now()); err != nil {
		fmt.Fprintf(a.Err, "could not prune old run records: %v\n", err)
	}
	if len(entries) == 0 {
		return ErrFailed
	}

	digestPath, _, err := inbox.WriteDigest(a.Paths.InboxDir(), now, entries, a.Paths.Home)
	if err != nil {
		fmt.Fprintf(a.Err, "could not write digest: %v\n", err)
	} else {
		a.printf("Digest: %s\n", a.tilde(digestPath))
	}
	a.notify(ctx, cfg, entries)
	if failed || inbox.Failed(entries) {
		return ErrFailed
	}
	return nil
}

// reportSetupError makes a broken setup visible in the inbox, since a timer
// run has nobody watching its output.
func (a *App) reportSetupError(err error) {
	if mkErr := os.MkdirAll(a.Paths.InboxDir(), 0o755); mkErr != nil {
		return
	}
	// The timer checks every 15 minutes; write the same error only once.
	note := strings.ReplaceAll(err.Error(), "\n", " ")
	if latest, rerr := os.ReadFile(filepath.Join(a.Paths.InboxDir(), "latest.md")); rerr == nil && strings.Contains(string(latest), note) {
		return
	}
	entries := []inbox.Entry{{Task: "autobot", Status: "failed", Note: err.Error()}}
	inbox.WriteDigest(a.Paths.InboxDir(), a.Now(), entries, a.Paths.Home)
}

func (a *App) notify(ctx context.Context, cfg config.Config, entries []inbox.Entry) {
	if a.NewNotifier == nil {
		return
	}
	n := a.NewNotifier(cfg.Notify)
	if n == nil || (cfg.Notify.On == "failure" && !inbox.Failed(entries)) {
		return
	}
	title := "autobot: " + inbox.Headline(entries)
	var lines []string
	for _, e := range entries {
		line := fmt.Sprintf("%s: %s", e.Task, e.Status)
		if e.Note != "" && e.Status != "completed" {
			line += " (" + firstLine(e.Note, 120) + ")"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", "Inbox: "+a.tilde(a.Paths.InboxDir()))
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := n.Send(nctx, title, strings.Join(lines, "\n")); err != nil {
		fmt.Fprintf(a.Err, "notification failed: %v\n", err)
	}
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

// safeRunTask runs one task and turns a panic into a failed entry, so a bug
// hit by one task does not stop the tasks after it.
func (a *App) safeRunTask(ctx context.Context, cfg config.Config, prov provider.Provider, keyErr error, t *task.Task, dryRun bool, redact record.Redactor) (e inbox.Entry, setupFailed bool) {
	defer func() {
		if r := recover(); r != nil {
			e = inbox.Entry{Task: t.ID, Status: "failed", Note: fmt.Sprintf("autobot hit an internal error: %v", r)}
			setupFailed = false
			fmt.Fprintf(a.Err, "%s: %s\n", t.ID, e.Note)
		}
	}()
	return a.runTask(ctx, cfg, prov, keyErr, t, dryRun, redact)
}

// runTask runs one task. setupFailed reports that the provider or its key
// failed before any tool ran, so the task did not really run and should be
// retried at the next check.
func (a *App) runTask(ctx context.Context, cfg config.Config, prov provider.Provider, keyErr error, t *task.Task, dryRun bool, redact record.Redactor) (entry inbox.Entry, setupFailed bool) {
	started := a.Now()
	entry = inbox.Entry{Task: t.ID, Status: "failed"}
	model := cfg.Provider.Model
	if t.Model != "" {
		model = t.Model
	}
	if dryRun {
		a.printf("==> %s (dry run, nothing will be written)\n", t.ID)
	} else {
		a.printf("==> %s\n", t.ID)
	}

	var rec *record.Run
	meta := record.Meta{Task: t.ID, Status: "failed", Started: started, Model: model}
	// fail ends a run that could not start, still leaving a record.
	fail := func(err error) inbox.Entry {
		entry.Note = redact.Apply(err.Error())
		fmt.Fprintf(a.Err, "%s: failed: %s\n", t.ID, entry.Note)
		if rec != nil {
			meta.Error = err.Error()
			meta.Finished = a.Now()
			meta.Reflection = "skipped"
			if ferr := rec.Finish(meta, a.report(meta, "")); ferr != nil {
				fmt.Fprintf(a.Err, "%s: could not write run record: %v\n", t.ID, ferr)
			}
		}
		return entry
	}
	if !dryRun {
		var err error
		if rec, err = record.Start(a.Paths.RunsDir(), t.ID, started, redact); err != nil {
			return fail(fmt.Errorf("start run record: %w", err)), false
		}
		entry.RunID = rec.ID
		meta.ID = rec.ID
	}
	if keyErr != nil {
		return fail(keyErr), true
	}

	outDir := a.Paths.OutputDir(t.ID)
	if !dryRun {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return fail(err), false
		}
	}
	policy, err := access.New(access.Options{
		Home:   a.Paths.Home,
		Output: outDir,
		Read:   t.Read,
		Write:  t.Write,
		// /proc exposes autobot's own environment, including API keys.
		Protected: []string{a.Paths.ConfigDir, a.Paths.StateDir, "/proc"},
	})
	if err != nil {
		return fail(err), false
	}
	var searcher tools.Searcher
	if t.Network && a.NewSearcher != nil {
		searcher = a.NewSearcher(cfg.Search)
	}
	toolset := tools.New(tools.Options{
		Policy:         policy,
		Home:           a.Paths.Home,
		Commands:       t.Commands,
		CommandTimeout: cfg.Defaults.CommandTimeout.Duration,
		Network:        t.Network,
		Searcher:       searcher,
		HTTPClient:     a.FetchClient,
		MaxResults:     cfg.Defaults.MaxResults,
		DryRun:         dryRun,
	})
	store := &memory.Store{Dir: a.Paths.MemoryDir(), MaxBytes: cfg.Memory.MaxBytes}
	if !dryRun {
		// Record hand edits before the run, so undo never mixes them up
		// with what this run's reflection changes.
		if _, err := os.Stat(a.Paths.MemoryDir()); err == nil {
			if err := store.Commit("manual edit"); err != nil {
				fmt.Fprintf(a.Err, "%s: could not record memory edits: %v\n", t.ID, err)
			}
		}
	}
	taskMem, sharedMem, err := store.Load(t.ID)
	if err != nil {
		return fail(fmt.Errorf("load memory: %w", err)), false
	}
	// Tasks with network access neither see nor change shared memory, so
	// facts one task read cannot leak out through another task's network
	// access, and text from the web cannot steer other tasks.
	if t.Network {
		sharedMem = nil
	}
	base, err := a.SystemPrompt()
	if err != nil {
		return fail(err), false
	}
	system := a.buildSystem(base, t, policy, taskMem, sharedMem, started)

	res, runErr := agent.Run(ctx, agent.Config{
		Provider:            prov,
		Model:               model,
		System:              system,
		Prompt:              t.Prompt,
		Tools:               toolset,
		MaxRounds:           t.MaxRounds,
		ContextWindow:       cfg.Provider.ContextWindow,
		MaxToolOutputTokens: cfg.Defaults.MaxToolOutputTokens,
		OnMessage: func(m provider.Message) {
			if rec != nil {
				rec.Message(m)
			}
		},
	})

	if dryRun {
		return a.finishDryRun(t, res, runErr, toolset), false
	}

	var perr *agent.ProviderError
	providerFailed := errors.As(runErr, &perr)
	// Only a provider failure before any tool ran means the task did not
	// really run. After tools ran, it has side effects and counts as a run.
	notStarted := providerFailed && len(res.ToolCalls) == 0
	meta.Status = "completed"
	meta.Rounds = res.Rounds
	meta.Tools = res.ToolCalls
	meta.PromptTokens, meta.CompletionTokens = res.Usage.PromptTokens, res.Usage.CompletionTokens
	for _, w := range toolset.Written() {
		meta.FilesWritten = append(meta.FilesWritten, a.tilde(w))
	}
	for _, m := range toolset.Moved() {
		meta.FilesMoved = append(meta.FilesMoved, a.tilde(m.From)+" -> "+a.tilde(m.To))
	}
	if runErr != nil {
		meta.Status = "failed"
		meta.Error = runErr.Error()
		entry.Note = redact.Apply(runErr.Error())
		fmt.Fprintf(a.Err, "%s: failed: %s\n", t.ID, entry.Note)
	} else {
		out, err := inbox.SaveOutput(a.Paths.InboxDir(), t.ID, started, redact.Apply(res.Final))
		if err != nil {
			meta.Status = "failed"
			meta.Error = "save output: " + err.Error()
			entry.Note = meta.Error
		} else {
			meta.Output = out
			entry.Output = out
			entry.Status = "completed"
		}
	}

	// Reflection learns from failures too, but not when the provider itself
	// is broken: there is nothing to learn and the request would fail again.
	if providerFailed || errors.Is(runErr, context.Canceled) {
		meta.Reflection = "skipped"
	} else {
		outcome, err := reflection.Run(ctx, prov, model, store, reflection.Input{
			TaskID: t.ID, Prompt: t.Prompt, Status: meta.Status, Final: res.Final, Error: meta.Error,
			Transcript: res.Messages, RunID: rec.ID, MaxBytes: cfg.Memory.MaxBytes, NoShared: t.Network,
		}, redact.Apply)
		if err != nil {
			meta.Reflection = err.Error()
			fmt.Fprintf(a.Err, "%s: %v\n", t.ID, err)
		} else {
			meta.Reflection = outcome.Summary
		}
	}
	meta.Finished = a.Now()
	if err := rec.Finish(meta, a.report(meta, res.Final)); err != nil {
		fmt.Fprintf(a.Err, "%s: could not write run record: %v\n", t.ID, err)
	}
	if entry.Status == "completed" {
		a.printf("%s: completed in %s, %d tool round(s). Output: %s\n", t.ID, meta.Finished.Sub(started).Round(time.Second), res.Rounds, a.tilde(entry.Output))
	}
	return entry, notStarted
}

func (a *App) finishDryRun(t *task.Task, res agent.Result, runErr error, toolset *tools.Set) inbox.Entry {
	entry := inbox.Entry{Task: t.ID, Status: "completed"}
	if runErr != nil {
		entry.Status = "failed"
		entry.Note = runErr.Error()
		fmt.Fprintf(a.Err, "%s: failed: %v\n", t.ID, runErr)
	} else {
		a.printf("\n--- final answer ---\n%s\n", res.Final)
	}
	if moves := toolset.Moved(); len(moves) > 0 {
		a.printf("\n--- moves that would be made (later tool calls in this dry run did not see them) ---\n")
		for _, m := range moves {
			a.printf("%s -> %s\n", a.tilde(m.From), a.tilde(m.To))
		}
	}
	changes := toolset.Changes()
	if len(changes) == 0 {
		a.printf("\n--- no files would be written ---\n")
		return entry
	}
	a.printf("\n--- files that would be written ---\n")
	for _, c := range changes {
		from := a.tilde(c.Path)
		var before []string
		if c.Existed {
			before = difflib.SplitLines(c.Before)
		} else {
			from = "/dev/null"
		}
		diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: before, B: difflib.SplitLines(c.After),
			FromFile: from, ToFile: a.tilde(c.Path), Context: 3,
		})
		a.printf("%s\n", diff)
	}
	return entry
}

func (a *App) buildSystem(base string, t *task.Task, policy *access.Policy, taskMem, sharedMem *memory.Doc, now time.Time) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(base))
	b.WriteString("\n\n## This run\n\n")
	fmt.Fprintf(&b, "- Date and time: %s\n", now.Format("Monday 2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "- Task: %s\n", t.ID)
	fmt.Fprintf(&b, "- Home folder: %s. Paths starting with ~ mean this folder.\n", a.Paths.Home)
	fmt.Fprintf(&b, "- Output folder: %s. You can always read and write here. Relative paths resolve here.\n", a.tilde(policy.Output()))
	list := func(items []string, none string) string {
		if len(items) == 0 {
			return none
		}
		return strings.Join(items, ", ")
	}
	fmt.Fprintf(&b, "- Read access: %s\n", list(policy.ReadPaths(), "only the output folder"))
	fmt.Fprintf(&b, "- Write access: %s\n", list(policy.WritePaths(), "only the output folder"))
	if t.Network {
		b.WriteString("- Network: web_search and web_fetch are available.\n")
	} else {
		b.WriteString("- Network: none.\n")
	}
	if len(t.Commands) > 0 {
		fmt.Fprintf(&b, "- Commands you may run with run_command: %s\n", strings.Join(t.Commands, "; "))
	}
	b.WriteString("- Your final answer is saved to the inbox automatically. Do not also write it to a file unless the task asks for one.\n")
	b.WriteString("\n## Memory from earlier runs\n\n")
	switch {
	case taskMem.Len() == 0 && (sharedMem == nil || sharedMem.Len() == 0):
		b.WriteString("Nothing yet. This may be the first run.\n")
	case sharedMem == nil:
		b.WriteString(taskMem.Render())
	default:
		b.WriteString(sharedMem.Render())
		b.WriteString("\n")
		b.WriteString(taskMem.Render())
	}
	return b.String()
}

func (a *App) report(m record.Meta, final string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", m.ID)
	fmt.Fprintf(&b, "- Task: %s\n- Status: %s\n- Started: %s\n- Duration: %s\n- Model: %s\n",
		m.Task, m.Status, m.Started.Format("2006-01-02 15:04:05 MST"), m.Finished.Sub(m.Started).Round(time.Second), m.Model)
	var tools []string
	for name, n := range m.Tools {
		tools = append(tools, fmt.Sprintf("%s %d", name, n))
	}
	sort.Strings(tools)
	if len(tools) == 0 {
		tools = []string{"none"}
	}
	fmt.Fprintf(&b, "- Tool rounds: %d\n- Tools used: %s\n", m.Rounds, strings.Join(tools, ", "))
	fmt.Fprintf(&b, "- Tokens: %d prompt, %d completion\n", m.PromptTokens, m.CompletionTokens)
	if m.Output != "" {
		fmt.Fprintf(&b, "- Output: %s\n", a.tilde(m.Output))
	}
	if len(m.FilesWritten) > 0 {
		fmt.Fprintf(&b, "- Files written: %s\n", strings.Join(m.FilesWritten, ", "))
	}
	if len(m.FilesMoved) > 0 {
		fmt.Fprintf(&b, "- Files moved: %d\n", len(m.FilesMoved))
	}
	if m.Reflection != "" {
		fmt.Fprintf(&b, "- Reflection: %s\n", m.Reflection)
	}
	if m.Error != "" {
		fmt.Fprintf(&b, "\n## Error\n\n%s\n", m.Error)
	}
	if final != "" {
		fmt.Fprintf(&b, "\n## Final answer\n\n%s\n", strings.TrimSpace(final))
	}
	if len(m.FilesMoved) > 0 {
		b.WriteString("\n## Files moved\n\n")
		for _, mv := range m.FilesMoved {
			fmt.Fprintf(&b, "- %s\n", mv)
		}
	}
	return b.String()
}
