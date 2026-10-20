package migrate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jgabor/autobot/internal/config"
	"github.com/jgabor/autobot/internal/memory"
	"github.com/jgabor/autobot/internal/paths"
	"github.com/jgabor/autobot/internal/task"
)

func TestGuessPathsOnV1Examples(t *testing.T) {
	cases := []struct {
		file        string
		read, write []string
	}{
		{"1-review-notes.md", []string{"~/Documents/Notes"}, []string{"~/Documents/Notes/next-actions.md"}},
		{"2-audit-user-services.md", []string{"~/.config/systemd/user"}, []string{"~/.config/systemd/user/audit.md"}},
		{"3-review-downloads.md", []string{"~/Downloads"}, []string{"~/Downloads/filing-plan.md"}},
	}
	for _, c := range cases {
		data, err := os.ReadFile(filepath.Join("testdata/v1", c.file))
		if err != nil {
			t.Fatal(err)
		}
		read, write := GuessPaths(string(data), "/home/u")
		if !reflect.DeepEqual(read, c.read) || !reflect.DeepEqual(write, c.write) {
			t.Errorf("%s: read %q write %q; want %q %q", c.file, read, write, c.read, c.write)
		}
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{"review-notes": "review-notes", "Review Notes!": "review-notes", "shared": "task-shared", "___": "task-___", "--": "task"}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func setupV1(t *testing.T) (paths.Paths, Options) {
	t.Helper()
	home := t.TempDir()
	p := paths.Paths{
		Home:       home,
		ConfigHome: filepath.Join(home, ".config"),
		ConfigDir:  filepath.Join(home, ".config/autobot"),
		StateDir:   filepath.Join(home, ".local/state/autobot"),
	}
	os.MkdirAll(p.ConfigDir, 0o755)
	os.MkdirAll(p.StateDir, 0o755)
	entries, _ := os.ReadDir("testdata/v1")
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join("testdata/v1", e.Name()))
		os.WriteFile(filepath.Join(p.ConfigDir, e.Name()), data, 0o644)
	}
	os.WriteFile(filepath.Join(p.ConfigDir, "0-system.md"), []byte("v1 system prompt"), 0o644)
	os.WriteFile(filepath.Join(p.ConfigDir, "environment"), []byte("AUTOBOT_PROVIDER=https://opencode.example/v1\nAUTOBOT_MODEL=\"Qwen/Qwen3.5-9B\"\nAUTOBOT_TIMEOUT=120\nOPENCODE_API_KEY=secret\n"), 0o600)
	os.WriteFile(filepath.Join(p.StateDir, "state.md"), []byte(`# autobot state

Version: 1
Last updated: 2026-10-01T10:00:00+02:00
Run count: 12

## Important locations

- Notes are in ~/Documents/Notes

## Useful procedures

## Recurring gotchas

- Glob does not support braces

## Durable insights

## Open follow-ups

- Check the audit report

## Next-run improvements

- Read fewer files
- No improvements recorded yet.
`), 0o644)
	var log []string
	o := Options{Paths: p, Env: func(string) string { return "" }, Log: func(f string, a ...any) { log = append(log, f) }}
	return p, o
}

func TestRunMigratesEverything(t *testing.T) {
	p, o := setupV1(t)
	res, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Tasks, []string{"review-notes", "audit-user-services", "review-downloads"}) || !res.System || !res.Config || !res.Memory {
		t.Fatalf("result: %+v", res)
	}
	tk, err := task.Load(filepath.Join(p.TasksDir(), "review-notes.md"), task.Defaults{Schedule: "daily", MaxRounds: 20})
	if err != nil {
		t.Fatal(err)
	}
	if tk.Enabled || tk.Schedule.String() != "hourly" || !reflect.DeepEqual(tk.Read, []string{"~/Documents/Notes"}) ||
		!strings.Contains(tk.Prompt, "Review a notes directory") {
		t.Fatalf("migrated task: %+v", tk)
	}
	raw, _ := os.ReadFile(tk.Path)
	if strings.Count(string(raw), "# review") != 3 {
		t.Errorf("guessed fields must be marked for review:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(p.ConfigDir, "1-review-notes.md")); err == nil {
		t.Error("v1 task file was not moved")
	}
	if data, _ := os.ReadFile(p.SystemFile()); string(data) != "v1 system prompt" {
		t.Errorf("system.md: %q", data)
	}
	cfg, err := config.Load(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.URL != "https://opencode.example/v1" || cfg.Provider.Model != "Qwen/Qwen3.5-9B" ||
		cfg.Provider.APIKeyEnv != "OPENCODE_API_KEY" || cfg.Provider.Timeout.Seconds() != 120 {
		t.Errorf("config: %+v", cfg.Provider)
	}
	if data, _ := os.ReadFile(p.ConfigFile()); strings.Contains(string(data), "secret") {
		t.Error("API key copied into config.toml")
	}
	shared, _ := os.ReadFile(filepath.Join(p.MemoryDir(), "shared.md"))
	doc := memory.Parse(string(shared), "", "s")
	text := doc.Render()
	for _, want := range []string{"## Locations\n\n- [s-1] Notes are in ~/Documents/Notes", "Glob does not support braces", "Read fewer files", "## Follow-ups\n\n- [s-"} {
		if !strings.Contains(text, want) {
			t.Errorf("shared memory missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Run count") || strings.Contains(text, "No improvements recorded") {
		t.Errorf("v1 bookkeeping copied:\n%s", text)
	}
	store := &memory.Store{Dir: p.MemoryDir()}
	if diff, err := store.Diff(1); err != nil || !strings.Contains(diff, "import v1 state.md") {
		t.Errorf("memory import not committed: %v\n%s", err, diff)
	}

	// A second run finds nothing new and changes nothing.
	again, err := Run(o)
	if err != nil || again.Anything {
		t.Fatalf("second run: %+v %v", again, err)
	}
}

func TestRunNeverOverwrites(t *testing.T) {
	p, o := setupV1(t)
	os.MkdirAll(p.TasksDir(), 0o755)
	os.WriteFile(filepath.Join(p.TasksDir(), "review-notes.md"), []byte("mine"), 0o644)
	os.WriteFile(p.ConfigFile(), []byte("mine"), 0o600)
	os.WriteFile(p.SystemFile(), []byte("mine"), 0o644)
	res, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Config || res.System {
		t.Fatalf("result: %+v", res)
	}
	for _, f := range []string{filepath.Join(p.TasksDir(), "review-notes.md"), p.ConfigFile(), p.SystemFile()} {
		if data, _ := os.ReadFile(f); string(data) != "mine" {
			t.Errorf("%s overwritten", f)
		}
	}
	if _, err := os.Stat(filepath.Join(p.ConfigDir, "1-review-notes.md")); err != nil {
		t.Error("skipped v1 file must stay in place")
	}
}

func TestMigrateFromOtherFolderReadsItsEnvironment(t *testing.T) {
	p, o := setupV1(t)
	old := filepath.Join(p.Home, "old-autobot")
	os.Rename(p.ConfigDir, old)
	os.MkdirAll(p.ConfigDir, 0o755)
	o.From = old
	res, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Config || len(res.Tasks) != 3 {
		t.Fatalf("result: %+v", res)
	}
	cfg, err := config.Load(p.ConfigFile())
	if err != nil || cfg.Provider.Model != "Qwen/Qwen3.5-9B" {
		t.Fatalf("config from --from folder: %+v %v", cfg.Provider, err)
	}
}
