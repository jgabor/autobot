// Package config loads autobot's config.toml.
package config

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

//go:embed template.toml
var Template string

// Config is the parsed config.toml.
type Config struct {
	Provider Provider `toml:"provider"`
	Defaults Defaults `toml:"defaults"`
	Memory   Memory   `toml:"memory"`
	Records  Records  `toml:"records"`
	Notify   Notify   `toml:"notify"`
	Search   Search   `toml:"search"`
}

type Provider struct {
	URL           string   `toml:"url"`
	Model         string   `toml:"model"`
	APIKeyEnv     string   `toml:"api_key_env"`
	KeyCommand    string   `toml:"key_command"`
	Timeout       Duration `toml:"timeout"`
	ContextTokens int      `toml:"context_tokens"`
}

type Defaults struct {
	Schedule            string   `toml:"schedule"`
	MaxRounds           int      `toml:"max_rounds"`
	MaxToolOutputTokens int      `toml:"max_tool_output_tokens"`
	MaxResults          int      `toml:"max_results"`
	CommandTimeout      Duration `toml:"command_timeout"`
}

type Memory struct {
	MaxBytes int `toml:"max_bytes"`
}

type Records struct {
	KeepDays int `toml:"keep_days"`
}

type Notify struct {
	Method  string `toml:"method"`
	On      string `toml:"on"`
	NtfyURL string `toml:"ntfy_url"`
	Email   Email  `toml:"email"`
}

type Email struct {
	Host        string   `toml:"host"`
	Port        int      `toml:"port"`
	Username    string   `toml:"username"`
	PasswordEnv string   `toml:"password_env"`
	From        string   `toml:"from"`
	To          []string `toml:"to"`
}

type Search struct {
	Provider  string `toml:"provider"`
	APIKeyEnv string `toml:"api_key_env"`
}

// Duration is a time.Duration written as a string such as "5m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Default returns the configuration used for any value config.toml leaves out.
func Default() Config {
	return Config{
		Provider: Provider{
			APIKeyEnv:     "OPENAI_API_KEY",
			Timeout:       Duration{5 * time.Minute},
			ContextTokens: 32000,
		},
		Defaults: Defaults{
			Schedule:            "daily",
			MaxRounds:           20,
			MaxToolOutputTokens: 3000,
			MaxResults:          500,
			CommandTimeout:      Duration{30 * time.Second},
		},
		Memory:  Memory{MaxBytes: 8000},
		Records: Records{KeepDays: 30},
		Notify:  Notify{Method: "none", On: "always"},
		Search:  Search{Provider: "kagi", APIKeyEnv: "KAGI_API_KEY"},
	}
}

// Load reads and validates a config file.
func Load(path string) (Config, error) {
	cfg := Default()
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, fmt.Errorf("no config at %s (run `autobot init`)", path)
		}
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		sort.Strings(keys)
		return cfg, fmt.Errorf("%s: unknown settings: %s", path, strings.Join(keys, ", "))
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks values that would otherwise fail later at run time.
func (c Config) Validate() error {
	var errs []error
	u, err := url.Parse(c.Provider.URL)
	switch {
	case c.Provider.URL == "":
		errs = append(errs, errors.New("provider.url is not set"))
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		errs = append(errs, fmt.Errorf("provider.url must be an http or https URL, got %q", c.Provider.URL))
	}
	if c.Provider.Model == "" {
		errs = append(errs, errors.New("provider.model is not set"))
	}
	if c.Provider.Timeout.Duration <= 0 {
		errs = append(errs, errors.New("provider.timeout must be positive"))
	}
	if c.Provider.ContextTokens < 1000 {
		errs = append(errs, errors.New("provider.context_tokens must be at least 1000"))
	}
	if c.Defaults.MaxRounds < 1 {
		errs = append(errs, errors.New("defaults.max_rounds must be positive"))
	}
	if c.Defaults.MaxToolOutputTokens < 100 {
		errs = append(errs, errors.New("defaults.max_tool_output_tokens must be at least 100"))
	}
	if c.Defaults.MaxResults < 1 {
		errs = append(errs, errors.New("defaults.max_results must be positive"))
	}
	if c.Defaults.CommandTimeout.Duration <= 0 {
		errs = append(errs, errors.New("defaults.command_timeout must be positive"))
	}
	if c.Memory.MaxBytes < 500 {
		errs = append(errs, errors.New("memory.max_bytes must be at least 500"))
	}
	if c.Records.KeepDays < 1 {
		errs = append(errs, errors.New("records.keep_days must be positive"))
	}
	if !slices.Contains([]string{"none", "notify-send", "ntfy", "email"}, c.Notify.Method) {
		errs = append(errs, fmt.Errorf("notify.method must be none, notify-send, ntfy or email, got %q", c.Notify.Method))
	}
	if c.Notify.On != "always" && c.Notify.On != "failure" {
		errs = append(errs, fmt.Errorf("notify.on must be always or failure, got %q", c.Notify.On))
	}
	if c.Notify.Method == "ntfy" && c.Notify.NtfyURL == "" {
		errs = append(errs, errors.New("notify.ntfy_url is required when notify.method is ntfy"))
	}
	if c.Notify.Method == "email" {
		e := c.Notify.Email
		if e.Host == "" || e.Port == 0 || e.From == "" || len(e.To) == 0 {
			errs = append(errs, errors.New("notify.email needs host, port, from and to when notify.method is email"))
		}
	}
	if c.Search.Provider != "kagi" {
		errs = append(errs, fmt.Errorf("search.provider must be kagi, got %q", c.Search.Provider))
	}
	return errors.Join(errs...)
}

// APIKey returns the provider key from key_command or api_key_env. An empty
// key is valid: local servers often need none.
func (p Provider) APIKey(ctx context.Context) (string, error) {
	if p.KeyCommand != "" {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", p.KeyCommand)
		// Kill the whole process group on timeout, and stop waiting for
		// output soon after, so a stuck child cannot hold the run lock.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 2 * time.Second
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("provider.key_command failed: %w", err)
		}
		key := strings.TrimSpace(string(out))
		if key == "" {
			return "", errors.New("provider.key_command printed nothing")
		}
		return key, nil
	}
	if p.APIKeyEnv == "" {
		return "", nil
	}
	return os.Getenv(p.APIKeyEnv), nil
}
