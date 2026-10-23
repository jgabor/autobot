package inbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutputAndDigest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	started := time.Date(2026, 10, 20, 9, 15, 0, 0, time.Local)
	out, err := SaveOutput(dir, "notes", started, "  Answer.  ")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(out) != "2026-10-20T091500.md" {
		t.Fatalf("output path %s", out)
	}
	out2, _ := SaveOutput(dir, "notes", started, "Second")
	if out2 == out {
		t.Fatal("same-second outputs collided")
	}
	data, _ := os.ReadFile(out)
	if string(data) != "Answer.\n" {
		t.Fatalf("output %q", data)
	}
	entries := []Entry{
		{Task: "notes", Status: "completed", Output: out, RunID: "r1"},
		{Task: "broken", Status: "invalid", Note: "header: unknown field\nmore"},
	}
	path, text, err := WriteDigest(dir, started, entries, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "2 task(s) ran, 1 failed") || !strings.Contains(text, "](../notes/2026-10-20T091500.md)") ||
		!strings.Contains(text, "**broken:** header: unknown field more") {
		t.Fatalf("digest:\n%s", text)
	}
	if filepath.Dir(path) != filepath.Join(dir, "digests") {
		t.Fatalf("digest path %s", path)
	}
	latest, _ := os.ReadFile(filepath.Join(dir, "latest.md"))
	if !strings.Contains(string(latest), "](notes/2026-10-20T091500.md)") {
		t.Fatalf("latest.md links:\n%s", latest)
	}
	if !Failed(entries) || Failed(entries[:1]) {
		t.Fatal("Failed")
	}
}
