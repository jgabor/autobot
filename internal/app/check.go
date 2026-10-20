package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"

	"github.com/jgabor/autobot/internal/config"
	"github.com/jgabor/autobot/internal/paths"
	"github.com/jgabor/autobot/internal/provider"
	"github.com/jgabor/autobot/internal/schedule"
	"github.com/jgabor/autobot/internal/task"
)

// ErrCheckFailed means at least one check failed.
var ErrCheckFailed = errors.New("some checks failed")

// LingerDir is where systemd records lingering users. Tests replace it.
var LingerDir = "/var/lib/systemd/linger"

type checker struct {
	a      *App
	failed bool
}

func (c *checker) ok(format string, args ...any) {
	c.a.printf("ok    "+format+"\n", args...)
}

func (c *checker) warn(format string, args ...any) {
	c.a.printf("warn  "+format+"\n", args...)
}

func (c *checker) fail(format string, args ...any) {
	c.failed = true
	c.a.printf("FAIL  "+format+"\n", args...)
}

// Check tests the setup. With offline, it skips the provider request.
func (a *App) Check(ctx context.Context, offline bool) error {
	c := &checker{a: a}
	cfg, err := a.LoadConfig()
	if err != nil {
		c.fail("config: %v", err)
		return ErrCheckFailed
	}
	c.ok("config: %s", a.tilde(a.Paths.ConfigFile()))

	if _, err := os.Stat(a.Paths.SystemFile()); err == nil {
		c.ok("system prompt: %s", a.tilde(a.Paths.SystemFile()))
	} else {
		c.ok("system prompt: built-in (no %s)", a.tilde(a.Paths.SystemFile()))
	}
	if err := os.MkdirAll(a.Paths.StateDir, 0o700); err != nil {
		c.fail("state folder %s: %v", a.tilde(a.Paths.StateDir), err)
	} else {
		c.ok("state folder: %s", a.tilde(a.Paths.StateDir))
	}

	network := a.checkTasks(c, cfg)

	key, err := cfg.Provider.APIKey(ctx)
	switch {
	case err != nil:
		c.fail("API key: %v", err)
	case cfg.Provider.KeyCommand != "":
		c.ok("API key: from key_command")
	case key == "" && cfg.Provider.APIKeyEnv != "":
		c.warn("API key: %s is empty. That is fine for local servers that need no key", cfg.Provider.APIKeyEnv)
	case key != "":
		c.ok("API key: from %s", cfg.Provider.APIKeyEnv)
	}

	if offline {
		c.warn("provider: not tested (--offline)")
	} else if err == nil {
		a.checkProvider(ctx, c, cfg, key)
	}

	if network {
		if a.Env(cfg.Search.APIKeyEnv) == "" {
			c.warn("web search: a task has network access but %s is empty, so web_search will fail. web_fetch still works", cfg.Search.APIKeyEnv)
		} else {
			c.ok("web search: %s key from %s", cfg.Search.Provider, cfg.Search.APIKeyEnv)
		}
	}

	switch cfg.Notify.Method {
	case "none":
		c.ok("notifications: off")
	case "notify-send":
		if _, err := exec.LookPath("notify-send"); err != nil {
			c.fail("notifications: notify-send is not installed")
		} else {
			c.ok("notifications: notify-send")
		}
	case "email":
		if cfg.Notify.Email.Username != "" && a.Env(cfg.Notify.Email.PasswordEnv) == "" {
			c.fail("notifications: email password variable %q is empty", cfg.Notify.Email.PasswordEnv)
		} else {
			c.ok("notifications: email to %v", cfg.Notify.Email.To)
		}
	default:
		c.ok("notifications: %s", cfg.Notify.Method)
	}

	timer := filepath.Join(a.Paths.SystemdDir(), "autobot.timer")
	if _, err := os.Stat(timer); err != nil {
		c.warn("timer: no %s, so nothing runs on a schedule. Run `autobot init` to create it", a.tilde(timer))
	} else {
		c.ok("timer: %s (enable it with `systemctl --user enable --now autobot.timer`)", a.tilde(timer))
	}
	if u, err := user.Current(); err == nil {
		if _, err := os.Stat(filepath.Join(LingerDir, u.Username)); err != nil {
			c.warn("linger: off, so the timer stops when you log out. Turn it on with `loginctl enable-linger %s`", u.Username)
		} else {
			c.ok("linger: on")
		}
	}

	if c.failed {
		return ErrCheckFailed
	}
	return nil
}

// checkTasks reports each task and returns whether any task uses the network.
func (a *App) checkTasks(c *checker, cfg config.Config) bool {
	tasks, invalid, err := task.LoadAll(a.Paths.TasksDir(), taskDefaults(cfg))
	if err != nil {
		c.fail("tasks: %v", err)
		return false
	}
	if len(tasks) == 0 && len(invalid) == 0 {
		c.fail("tasks: none in %s. Create one with `autobot new <name>`", a.tilde(a.Paths.TasksDir()))
		return false
	}
	state, err := schedule.LoadState(a.Paths.ScheduleFile())
	if err != nil {
		c.warn("schedule state: %v", err)
	}
	now := a.Now()
	network := false
	for _, t := range tasks {
		network = network || t.Network
		if !t.Enabled {
			c.warn("task %s: disabled", t.ID)
			continue
		}
		next := "due now"
		if at := schedule.NextRun(t.Schedule, state.Tasks[t.ID].LastRun, now); at.After(now) {
			next = "next run " + at.Format("2006-01-02 15:04")
		}
		c.ok("task %s: %s, %s", t.ID, t.Schedule, next)
		for _, p := range append(append([]string{}, t.Read...), t.Write...) {
			full := paths.Expand(p, a.Paths.Home)
			if _, err := os.Stat(full); err != nil {
				if _, perr := os.Stat(filepath.Dir(full)); perr != nil {
					c.warn("task %s: %s does not exist", t.ID, p)
				}
			}
		}
	}
	for _, inv := range invalid {
		c.fail("task %s: %v", inv.ID, inv.Err)
	}
	return network
}

func (a *App) checkProvider(ctx context.Context, c *checker, cfg config.Config, key string) {
	p := a.NewProvider(cfg.Provider, key)
	ctx, cancel := context.WithTimeout(ctx, cfg.Provider.Timeout.Duration)
	defer cancel()
	ping := provider.Tool{Type: "function", Function: provider.ToolFunction{
		Name:        "ping",
		Description: "Confirms that tool calling works.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"message": map[string]any{"type": "string"},
		}, "required": []string{"message"}},
	}}
	start := time.Now()
	resp, err := p.Chat(ctx, provider.Request{
		Model: cfg.Provider.Model,
		Messages: []provider.Message{
			{Role: "system", Content: "You are testing tool support. Always use the tool you are given."},
			{Role: "user", Content: `Call the ping tool with message "hello". Do not reply with text.`},
		},
		Tools: []provider.Tool{ping},
	})
	if err != nil {
		c.fail("provider: %v. Check provider.url, provider.model and the API key", err)
		return
	}
	took := time.Since(start).Round(100 * time.Millisecond)
	c.ok("provider: %s answered in %s", cfg.Provider.Model, took)
	for _, tc := range resp.Message.ToolCalls {
		if tc.Function.Name == "ping" {
			c.ok("tool calls: %s called the test tool", cfg.Provider.Model)
			return
		}
	}
	c.fail("tool calls: %s replied without calling the test tool. autobot needs a model and server with tool calling support%s",
		cfg.Provider.Model, replyHint(resp.Message.Content))
}

func replyHint(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf(" (it said: %q)", firstLine(s, 80))
}
