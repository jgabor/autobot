// Package record writes and reads run records: a transcript and a report per
// task run.
package record

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jgabor/autobot/internal/provider"
)

// TimeFormat names run folders and inbox files. It sorts by time.
const TimeFormat = "2006-01-02T150405"

// Meta is the structured summary of a run, stored as meta.json.
type Meta struct {
	ID               string         `json:"id"`
	Task             string         `json:"task"`
	Status           string         `json:"status"`
	Error            string         `json:"error,omitempty"`
	Started          time.Time      `json:"started"`
	Finished         time.Time      `json:"finished"`
	Model            string         `json:"model"`
	Rounds           int            `json:"rounds"`
	Tools            map[string]int `json:"tools,omitempty"`
	PromptTokens     int            `json:"prompt_tokens"`
	CompletionTokens int            `json:"completion_tokens"`
	Output           string         `json:"output,omitempty"`
	FilesWritten     []string       `json:"files_written,omitempty"`
	FilesMoved       []string       `json:"files_moved,omitempty"`
	Reflection       string         `json:"reflection,omitempty"`
}

// Redactor replaces secrets before anything is written.
type Redactor []string

// Apply replaces every secret in s.
func (r Redactor) Apply(s string) string {
	for _, secret := range r {
		if len(secret) >= 4 {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

// Run is a run record being written.
type Run struct {
	ID     string
	Dir    string
	f      *os.File
	w      *bufio.Writer
	redact Redactor
}

type event struct {
	Time       time.Time           `json:"time"`
	Role       string              `json:"role"`
	Content    string              `json:"content,omitempty"`
	ToolCalls  []provider.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	Name       string              `json:"name,omitempty"`
}

// Start creates the run folder.
func Start(runsDir, taskID string, started time.Time, redact Redactor) (*Run, error) {
	base := started.Format(TimeFormat) + "-" + taskID
	id := base
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return nil, err
	}
	var dir string
	for n := 2; ; n++ {
		dir = filepath.Join(runsDir, id)
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
	// A first meta.json lets `autobot log <task>` find a run that was killed.
	data, _ := json.MarshalIndent(Meta{ID: id, Task: taskID, Status: "incomplete", Started: started}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "transcript.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &Run{ID: id, Dir: dir, f: f, w: bufio.NewWriter(f), redact: redact}, nil
}

// Message appends one message to the transcript.
func (r *Run) Message(m provider.Message) {
	e := event{Time: time.Now(), Role: m.Role, Content: r.redact.Apply(m.Content), ToolCallID: m.ToolCallID, Name: m.Name}
	for _, tc := range m.ToolCalls {
		tc.Function.Arguments = r.redact.Apply(tc.Function.Arguments)
		e.ToolCalls = append(e.ToolCalls, tc)
	}
	data, _ := json.Marshal(e)
	r.w.Write(data)
	r.w.WriteByte('\n')
}

// Finish writes meta.json and report.md and closes the transcript.
func (r *Run) Finish(m Meta, report string) error {
	m.Error = r.redact.Apply(m.Error)
	errs := []error{r.w.Flush(), r.f.Close()}
	data, err := json.MarshalIndent(m, "", "  ")
	errs = append(errs, err)
	if err == nil {
		errs = append(errs, os.WriteFile(filepath.Join(r.Dir, "meta.json"), append(data, '\n'), 0o600))
	}
	errs = append(errs, os.WriteFile(filepath.Join(r.Dir, "report.md"), []byte(r.redact.Apply(report)), 0o600))
	return errors.Join(errs...)
}

// List returns all runs, newest first.
func List(runsDir string) ([]Meta, error) {
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(runsDir, e.Name(), "meta.json"))
		if err != nil {
			// A run that is still going, or was killed, has no meta.json.
			out = append(out, Meta{ID: e.Name(), Status: "incomplete"})
			continue
		}
		var m Meta
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		m.ID = e.Name()
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// Find resolves a run by full ID, unique ID prefix, or task name (latest run).
func Find(runsDir, query string) (Meta, error) {
	runs, err := List(runsDir)
	if err != nil {
		return Meta{}, err
	}
	if len(runs) == 0 {
		return Meta{}, errors.New("no runs recorded yet (run `autobot run <task>`)")
	}
	if query == "" || query == "last" {
		return runs[0], nil
	}
	var prefixed []Meta
	for _, m := range runs {
		if m.ID == query {
			return m, nil
		}
		if strings.HasPrefix(m.ID, query) {
			prefixed = append(prefixed, m)
		}
	}
	if len(prefixed) == 1 {
		return prefixed[0], nil
	}
	for _, m := range runs {
		if m.Task == query {
			return m, nil
		}
	}
	if len(prefixed) > 1 {
		return Meta{}, fmt.Errorf("%q matches %d runs; give more of the run ID", query, len(prefixed))
	}
	return Meta{}, fmt.Errorf("no run matches %q (see `autobot log`)", query)
}

// Transcript reads a run's messages.
func Transcript(dir string) ([]provider.Message, []time.Time, error) {
	f, err := os.Open(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var msgs []provider.Message
	var times []time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		msgs = append(msgs, provider.Message{Role: e.Role, Content: e.Content, ToolCalls: e.ToolCalls, ToolCallID: e.ToolCallID, Name: e.Name})
		times = append(times, e.Time)
	}
	return msgs, times, sc.Err()
}

// Prune deletes run folders started before now minus keep. It returns how
// many were removed.
func Prune(runsDir string, keep time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-keep)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) < len(TimeFormat) {
			continue
		}
		started, err := time.ParseInLocation(TimeFormat, e.Name()[:len(TimeFormat)], now.Location())
		if err != nil || !started.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(runsDir, e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
