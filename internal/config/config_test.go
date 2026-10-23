package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTemplateIsValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, Template))
	if err != nil {
		t.Fatal(err)
	}
	def := Default()
	if cfg.Provider.Timeout.Duration != 5*time.Minute || cfg.Defaults.MaxRounds != def.Defaults.MaxRounds ||
		cfg.Memory.MaxBytes != def.Memory.MaxBytes || cfg.Notify.Method != "none" {
		t.Fatalf("template values differ from defaults: %+v", cfg)
	}
}

func TestMinimalConfigGetsDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[provider]\nurl = \"http://localhost:8080\"\nmodel = \"m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Schedule != "daily" || cfg.Records.KeepDays != 30 || cfg.Search.APIKeyEnv != "KAGI_API_KEY" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	base := "[provider]\nurl = \"http://localhost:8080\"\nmodel = \"m\"\n"
	cases := map[string]struct{ content, want string }{
		"unknown key":    {base + "modle = \"x\"\n", "unknown settings: provider.modle"},
		"no url":         {"[provider]\nmodel = \"m\"\n", "provider.url is not set"},
		"bad url":        {"[provider]\nurl = \"localhost\"\nmodel = \"m\"\n", "http or https"},
		"no model":       {"[provider]\nurl = \"http://x\"\n", "provider.model"},
		"bad duration":   {base + "timeout = \"soon\"\n", "soon"},
		"bad notify":     {base + "[notify]\nmethod = \"pigeon\"\n", "notify.method"},
		"ntfy needs url": {base + "[notify]\nmethod = \"ntfy\"\n", "ntfy_url"},
		"email fields":   {base + "[notify]\nmethod = \"email\"\n", "notify.email"},
		"bad syntax":     {"[provider\n", "config.toml"},
	}
	for name, c := range cases {
		_, err := Load(writeConfig(t, c.content))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	_, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err == nil || !strings.Contains(err.Error(), "autobot init") {
		t.Errorf("missing file: %v", err)
	}
}

func TestAPIKey(t *testing.T) {
	t.Setenv("TEST_AUTOBOT_KEY", "from-env")
	key, err := Provider{APIKeyEnv: "TEST_AUTOBOT_KEY"}.APIKey(context.Background())
	if err != nil || key != "from-env" {
		t.Fatalf("env key %q %v", key, err)
	}
	key, err = Provider{APIKeyEnv: "TEST_AUTOBOT_KEY", KeyCommand: "echo '  from-command '"}.APIKey(context.Background())
	if err != nil || key != "from-command" {
		t.Fatalf("command key %q %v", key, err)
	}
	if _, err := (Provider{KeyCommand: "exit 3"}).APIKey(context.Background()); err == nil {
		t.Fatal("failing key_command: want error")
	}
	if _, err := (Provider{KeyCommand: "true"}).APIKey(context.Background()); err == nil {
		t.Fatal("empty key_command output: want error")
	}
}

func TestKeyCommandTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := (Provider{KeyCommand: "sleep 10 & sleep 10; echo k"}).APIKey(ctx); err == nil {
		t.Fatal("want error")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("key_command outlived its deadline")
	}
}
