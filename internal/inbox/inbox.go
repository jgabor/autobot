// Package inbox saves task outputs and run digests where the user reads them.
package inbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jgabor/autobot/internal/paths"
	"github.com/jgabor/autobot/internal/record"
)

// SaveOutput writes a task's final answer to inbox/<task>/<time>.md.
func SaveOutput(dir, taskID string, started time.Time, content string) (string, error) {
	taskDir := filepath.Join(dir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return "", err
	}
	base := started.Format(record.TimeFormat)
	path := filepath.Join(taskDir, base+".md")
	for n := 2; ; n++ {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			break
		}
		path = filepath.Join(taskDir, fmt.Sprintf("%s-%d.md", base, n))
	}
	return path, os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0o644)
}

// Entry is one task's line in a digest.
type Entry struct {
	Task   string
	Status string
	Output string
	RunID  string
	Note   string
}

// Failed reports whether any entry failed.
func Failed(entries []Entry) bool {
	for _, e := range entries {
		if e.Status != "completed" {
			return true
		}
	}
	return false
}

// Headline summarizes entries in one line, for notifications.
func Headline(entries []Entry) string {
	failed := 0
	for _, e := range entries {
		if e.Status != "completed" {
			failed++
		}
	}
	s := fmt.Sprintf("%d task(s) ran", len(entries))
	if failed > 0 {
		s += fmt.Sprintf(", %d failed", failed)
	}
	return s
}

// WriteDigest writes inbox/digests/<time>.md and copies it to inbox/latest.md.
func WriteDigest(dir string, now time.Time, entries []Entry, home string) (string, string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# autobot digest, %s\n\n%s.\n\n", now.Format("2006-01-02 15:04"), Headline(entries))
	b.WriteString("| Task | Status | Output | Run |\n|---|---|---|---|\n")
	for _, e := range entries {
		out := "none"
		if e.Output != "" {
			rel, err := filepath.Rel(filepath.Join(dir, "digests"), e.Output)
			if err != nil {
				rel = e.Output
			}
			out = fmt.Sprintf("[%s](%s)", paths.Tilde(e.Output, home), rel)
		}
		run := e.RunID
		if run == "" {
			run = "none"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", e.Task, e.Status, out, run)
	}
	var notes []string
	for _, e := range entries {
		if e.Note != "" {
			notes = append(notes, fmt.Sprintf("- **%s:** %s", e.Task, strings.ReplaceAll(e.Note, "\n", " ")))
		}
	}
	if len(notes) > 0 {
		b.WriteString("\n## Notes\n\n" + strings.Join(notes, "\n") + "\n")
	}
	b.WriteString("\nSee a run with `autobot show <run>`.\n")

	digestDir := filepath.Join(dir, "digests")
	if err := os.MkdirAll(digestDir, 0o755); err != nil {
		return "", "", err
	}
	path := filepath.Join(digestDir, now.Format(record.TimeFormat)+".md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", "", err
	}
	// latest.md sits one level up, so its relative links need adjusting.
	latest := strings.ReplaceAll(b.String(), "](../", "](")
	if err := os.WriteFile(filepath.Join(dir, "latest.md"), []byte(latest), 0o644); err != nil {
		return "", "", err
	}
	return path, b.String(), nil
}
