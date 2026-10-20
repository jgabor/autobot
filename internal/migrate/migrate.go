// Package migrate converts a v1 autobot setup (numbered task files, one
// state.md) to the v2 layout.
package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jgabor/autobot/internal/config"
	"github.com/jgabor/autobot/internal/memory"
	"github.com/jgabor/autobot/internal/paths"
	"github.com/jgabor/autobot/internal/task"
)

// Options say where the v1 files are and where v2 files go.
type Options struct {
	Paths paths.Paths
	// From is the v1 config folder. It defaults to the v2 config folder,
	// since v1 used the same location.
	From string
	// V1State is the v1 state.md. It defaults to the v2 state folder's state.md.
	V1State string
	// Env reads variables that v1 took from the environment.
	Env func(string) string
	// Log receives one line per action.
	Log func(format string, args ...any)
}

var v1Task = regexp.MustCompile(`^([1-9][0-9]*)-(.+)\.md$`)

// Result counts what was migrated.
type Result struct {
	Tasks    []string
	Skipped  []string
	System   bool
	Config   bool
	Memory   bool
	Anything bool
}

// Run migrates. Existing v2 files are never overwritten.
func Run(o Options) (Result, error) {
	var res Result
	if o.From == "" {
		o.From = o.Paths.ConfigDir
	}
	if o.V1State == "" {
		o.V1State = filepath.Join(o.Paths.StateDir, "state.md")
	}
	if o.Env == nil {
		o.Env = os.Getenv
	}
	home := o.Paths.Home
	tilde := func(p string) string { return paths.Tilde(p, home) }

	entries, err := os.ReadDir(o.From)
	if err != nil {
		return res, fmt.Errorf("read %s: %w", tilde(o.From), err)
	}
	type v1 struct {
		num  int
		name string
		path string
	}
	var found []v1
	for _, e := range entries {
		m := v1Task.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		found = append(found, v1{n, m[2], filepath.Join(o.From, e.Name())})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].num < found[j].num })

	if err := os.MkdirAll(o.Paths.TasksDir(), 0o755); err != nil {
		return res, err
	}
	for _, f := range found {
		id := Slug(f.name)
		target := filepath.Join(o.Paths.TasksDir(), id+".md")
		if _, err := os.Lstat(target); err == nil {
			o.Log("skipped %s: %s already exists", filepath.Base(f.path), tilde(target))
			res.Skipped = append(res.Skipped, f.path)
			continue
		}
		data, err := os.ReadFile(f.path)
		if err != nil {
			return res, err
		}
		read, write := GuessPaths(string(data), home)
		content := Header(filepath.Base(f.path), read, write) + strings.TrimSpace(string(data)) + "\n"
		if _, err := task.Parse(target, []byte(content), task.Defaults{Schedule: "hourly", MaxRounds: 20}); err != nil {
			return res, fmt.Errorf("converted %s does not parse: %w", filepath.Base(f.path), err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return res, err
		}
		if err := os.Remove(f.path); err != nil {
			return res, err
		}
		o.Log("moved %s to %s (disabled until reviewed)", filepath.Base(f.path), tilde(target))
		for _, p := range read {
			o.Log("  guessed read:  %s", p)
		}
		for _, p := range write {
			o.Log("  guessed write: %s", p)
		}
		if len(read) == 0 && len(write) == 0 {
			o.Log("  no paths found in the task text; add read and write by hand")
		}
		res.Tasks = append(res.Tasks, id)
		res.Anything = true
	}

	oldSystem := filepath.Join(o.From, "0-system.md")
	if _, err := os.Stat(oldSystem); err == nil {
		if _, err := os.Lstat(o.Paths.SystemFile()); err == nil {
			o.Log("skipped 0-system.md: %s already exists", tilde(o.Paths.SystemFile()))
		} else {
			if err := os.Rename(oldSystem, o.Paths.SystemFile()); err != nil {
				return res, err
			}
			o.Log("moved 0-system.md to %s", tilde(o.Paths.SystemFile()))
			res.System, res.Anything = true, true
		}
	}

	if ok, err := writeConfig(o, tilde); err != nil {
		return res, err
	} else if ok {
		res.Config, res.Anything = true, true
	}

	if ok, err := importState(o, tilde); err != nil {
		return res, err
	} else if ok {
		res.Memory, res.Anything = true, true
	}
	return res, nil
}

// Slug turns a v1 task name into a valid v2 task name.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if s == "" {
		return "task"
	}
	if task.ValidID(s) != nil {
		s = "task-" + s
	}
	return s
}

// Header builds a disabled v2 header with guessed paths marked for review.
func Header(from string, read, write []string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "# Migrated from %s. Review read and write, then set enabled: true.\n", from)
	b.WriteString("schedule: hourly\n")
	b.WriteString("enabled: false # review\n")
	list := func(name string, items []string) {
		if len(items) == 0 {
			fmt.Fprintf(&b, "%s: [] # review\n", name)
			return
		}
		fmt.Fprintf(&b, "%s: # review\n", name)
		for _, p := range items {
			fmt.Fprintf(&b, "  - %q\n", p)
		}
	}
	list("read", read)
	list("write", write)
	b.WriteString("---\n")
	return b.String()
}

var (
	pathRe     = regexp.MustCompile("(~/[^\\s`'\"),;]*|/(?:home|etc|var|opt|srv|mnt|media|usr|data|tmp)/[^\\s`'\"),;]*)")
	sentenceRe = regexp.MustCompile(`[.!?]+(\s+|$)`)
	writeRe    = regexp.MustCompile(`(?i)\b(write|save|append|create|output)\b`)
)

// GuessPaths finds paths in a v1 task's text. A path in a sentence that
// talks about writing is a write path; any other path is a read path, cut
// back to the folder before its first glob character.
func GuessPaths(text, home string) (read, write []string) {
	seen := map[string]bool{}
	add := func(list *[]string, kind, p string) {
		if !seen[kind+p] {
			seen[kind+p] = true
			*list = append(*list, p)
		}
	}
	start := 0
	sentences := []string{}
	for _, loc := range sentenceRe.FindAllStringIndex(text, -1) {
		sentences = append(sentences, text[start:loc[1]])
		start = loc[1]
	}
	if start < len(text) {
		sentences = append(sentences, text[start:])
	}
	for _, s := range sentences {
		isWrite := writeRe.MatchString(s)
		for _, p := range pathRe.FindAllString(s, -1) {
			p = strings.TrimRight(p, ".:")
			globbed := hasGlob(p)
			if p == "~" || p == "~/" {
				continue
			}
			if i := strings.IndexAny(p, "*?["); i >= 0 {
				// Keep the folder that holds the first glob segment.
				p = p[:i]
				p = p[:max(strings.LastIndex(p, "/"), 0)]
			}
			p = strings.TrimRight(p, "/")
			if p == "" || p == "~" {
				continue
			}
			if isWrite && !globbed {
				add(&write, "w", p)
			} else {
				add(&read, "r", p)
			}
		}
	}
	return read, write
}

func hasGlob(p string) bool { return strings.ContainsAny(p, "*?[") }

func writeConfig(o Options, tilde func(string) string) (bool, error) {
	cfgPath := o.Paths.ConfigFile()
	if _, err := os.Lstat(cfgPath); err == nil {
		return false, nil
	}
	vars := map[string]string{}
	// v1 kept settings in an environment file next to its tasks.
	envFile := filepath.Join(o.From, "environment")
	if _, err := os.Stat(envFile); err != nil {
		envFile = filepath.Join(o.Paths.ConfigDir, "environment")
	}
	if data, err := os.ReadFile(envFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimPrefix(strings.TrimSpace(line), "export ")
			if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(k, "#") {
				vars[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	get := func(k string) string {
		if v := o.Env(k); v != "" {
			return v
		}
		return vars[k]
	}
	url, model := get("AUTOBOT_PROVIDER"), get("AUTOBOT_MODEL")
	if url == "" && model == "" {
		return false, nil
	}
	text := config.Template
	replace := func(old, new string) { text = strings.Replace(text, old, new, 1) }
	if url != "" {
		replace(`url = "http://localhost:11434/v1"`, fmt.Sprintf("url = %q", url))
	}
	if model != "" {
		replace(`model = "qwen3.5:9b"`, fmt.Sprintf("model = %q", model))
	}
	if strings.Contains(url, "opencode") {
		replace(`api_key_env = "OPENAI_API_KEY"`, `api_key_env = "OPENCODE_API_KEY"`)
	}
	if t := get("AUTOBOT_TIMEOUT"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			replace(`timeout = "5m"`, fmt.Sprintf(`timeout = "%ds"`, n))
		}
	}
	if r := get("AUTOBOT_MAX_TOOL_ROUNDS"); r != "" {
		if n, err := strconv.Atoi(r); err == nil && n > 0 {
			replace(`max_rounds = 20`, fmt.Sprintf(`max_rounds = %d`, n))
		}
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(cfgPath, []byte(text), 0o600); err != nil {
		return false, err
	}
	keys := filepath.Join(o.Paths.ConfigDir, "environment")
	if envFile != keys {
		o.Log("wrote %s from the v1 AUTOBOT_* settings. Copy your API keys from %s to %s", tilde(cfgPath), tilde(envFile), tilde(keys))
	} else {
		o.Log("wrote %s from the v1 AUTOBOT_* settings. API keys stay in %s", tilde(cfgPath), tilde(keys))
	}
	return true, nil
}

var v1Sections = map[string]string{
	"important locations":   "Locations",
	"useful procedures":     "Procedures",
	"recurring gotchas":     "Gotchas",
	"durable insights":      "Insights",
	"open follow-ups":       "Follow-ups",
	"next-run improvements": "Gotchas",
}

func importState(o Options, tilde func(string) string) (bool, error) {
	data, err := os.ReadFile(o.V1State)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	target := filepath.Join(o.Paths.MemoryDir(), memory.SharedName+".md")
	if _, err := os.Lstat(target); err == nil {
		o.Log("skipped %s: %s already exists", tilde(o.V1State), tilde(target))
		return false, nil
	}
	doc := memory.New("Shared memory", "s")
	section := -1
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(trimmed, "## "); ok {
			section = -1
			if v2, ok := v1Sections[strings.ToLower(strings.TrimSpace(name))]; ok {
				for i, s := range doc.Sections {
					if s.Name == v2 {
						section = i
					}
				}
			}
			continue
		}
		text := strings.TrimSpace(strings.TrimLeft(trimmed, "-*+ "))
		if section < 0 || text == "" || text == "No improvements recorded yet." {
			continue
		}
		doc.Sections[section].Items = append(doc.Sections[section].Items, memory.Item{Text: text})
	}
	// Reparse to assign IDs.
	doc = memory.Parse(doc.Render(), "Shared memory", "s")
	store := &memory.Store{Dir: o.Paths.MemoryDir()}
	if err := store.Write(memory.SharedName, doc, "import v1 state.md"); err != nil {
		return false, err
	}
	o.Log("copied %s into %s (%d items)", tilde(o.V1State), tilde(target), doc.Len())
	if n := len(doc.Render()); n > config.Default().Memory.MaxBytes {
		o.Log("  it is %d bytes, over the default memory.max_bytes; reflection will not add to it until it shrinks", n)
	}
	return true, nil
}
