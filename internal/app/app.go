// Package app wires autobot's parts together for each command.
package app

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jgabor/autobot/internal/config"
	"github.com/jgabor/autobot/internal/notify"
	"github.com/jgabor/autobot/internal/paths"
	"github.com/jgabor/autobot/internal/provider"
	"github.com/jgabor/autobot/internal/task"
	"github.com/jgabor/autobot/internal/tools"
)

//go:embed system.md
var DefaultSystem string

// App holds what every command needs. Tests replace the hooks.
type App struct {
	Paths paths.Paths
	Out   io.Writer
	Err   io.Writer
	Now   func() time.Time

	// NewProvider builds the model client.
	NewProvider func(c config.Provider, key string) provider.Provider
	// NewSearcher builds the web search client. It returns nil when search
	// is not configured.
	NewSearcher func(c config.Search) tools.Searcher
	// FetchClient backs web_fetch. Nil means the safe default client.
	FetchClient *http.Client
	// NewNotifier builds the notifier. It returns nil for "none".
	NewNotifier func(c config.Notify) notify.Notifier
	// Executable is the path written into the systemd unit.
	Executable string
	// Env reads environment variables.
	Env func(string) string
}

// New returns an App for the real environment.
func New(p paths.Paths, out, errOut io.Writer) *App {
	exe, err := os.Executable()
	if err != nil {
		exe = "autobot"
	}
	return &App{
		Paths: p,
		Out:   out,
		Err:   errOut,
		Now:   time.Now,
		NewProvider: func(c config.Provider, key string) provider.Provider {
			if c.Type == config.ProviderClaudeCLI {
				return &provider.ClaudeCLI{Command: c.Command, Timeout: c.Timeout.Duration}
			}
			return &provider.OpenAI{URL: c.URL, Key: key, Timeout: c.Timeout.Duration, Retries: 2, Backoff: 5 * time.Second}
		},
		NewSearcher: func(c config.Search) tools.Searcher {
			key := os.Getenv(c.APIKeyEnv)
			if key == "" {
				return nil
			}
			return &tools.Kagi{Key: key}
		},
		NewNotifier: func(c config.Notify) notify.Notifier { return notify.FromConfig(c, nil) },
		Executable:  exe,
		Env:         os.Getenv,
	}
}

func (a *App) printf(format string, args ...any) { fmt.Fprintf(a.Out, format, args...) }

func (a *App) tilde(p string) string { return paths.Tilde(p, a.Paths.Home) }

// LoadConfig reads config.toml.
func (a *App) LoadConfig() (config.Config, error) {
	return config.Load(a.Paths.ConfigFile())
}

// SystemPrompt returns system.md, or the built-in prompt when it is absent.
func (a *App) SystemPrompt() (string, error) {
	data, err := os.ReadFile(a.Paths.SystemFile())
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultSystem, nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(data)) == "" {
		return DefaultSystem, nil
	}
	return string(data), nil
}

func taskDefaults(c config.Config) task.Defaults {
	return task.Defaults{Schedule: c.Defaults.Schedule, MaxRounds: c.Defaults.MaxRounds}
}

// secrets lists values that must never be written to records.
func (a *App) secrets(c config.Config, key string) []string {
	var out []string
	if key != "" {
		out = append(out, key)
	}
	for _, name := range []string{c.Search.APIKeyEnv, c.Notify.Email.PasswordEnv} {
		if name != "" {
			if v := a.Env(name); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}
