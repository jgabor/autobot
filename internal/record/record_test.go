package record

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jgabor/autobot/internal/provider"
)

func TestRunRecord(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 10, 20, 9, 15, 0, 0, time.Local)
	r, err := Start(dir, "notes", started, Redactor{"sk-secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "2026-10-20T091500-notes" {
		t.Fatalf("id %q", r.ID)
	}
	r.Message(provider.Message{Role: "user", Content: "use sk-secret-key please"})
	r.Message(provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{provider.Call("c1", "read", `{"path":"sk-secret-key"}`)}})
	if err := r.Finish(Meta{ID: r.ID, Task: "notes", Status: "completed", Started: started, Finished: started.Add(time.Minute), Error: "sk-secret-key failed"}, "# report sk-secret-key\n"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"transcript.jsonl", "meta.json", "report.md"} {
		data, err := os.ReadFile(filepath.Join(r.Dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "sk-secret-key") {
			t.Errorf("%s contains the secret", name)
		}
	}
	msgs, times, err := Transcript(r.Dir)
	if err != nil || len(msgs) != 2 || len(times) != 2 || msgs[1].ToolCalls[0].Function.Name != "read" {
		t.Fatalf("transcript: %+v %v", msgs, err)
	}

	// Same second, same task: a second folder, not a collision.
	r2, err := Start(dir, "notes", started, nil)
	if err != nil || r2.ID != "2026-10-20T091500-notes-2" {
		t.Fatalf("second run id %q %v", r2.ID, err)
	}
	r2.Finish(Meta{ID: r2.ID, Task: "notes", Status: "failed"}, "")
	r3, _ := Start(dir, "other", started.Add(time.Hour), nil)
	r3.Finish(Meta{ID: r3.ID, Task: "other", Status: "completed"}, "")

	runs, err := List(dir)
	if err != nil || len(runs) != 3 || runs[0].Task != "other" {
		t.Fatalf("list: %+v %v", runs, err)
	}
	cases := map[string]string{
		"":                        "2026-10-20T101500-other",
		"last":                    "2026-10-20T101500-other",
		"notes":                   "2026-10-20T091500-notes-2",
		"2026-10-20T091500-notes": "2026-10-20T091500-notes",
		"2026-10-20T10":           "2026-10-20T101500-other",
	}
	for q, want := range cases {
		m, err := Find(dir, q)
		if err != nil || m.ID != want {
			t.Errorf("Find(%q) = %q, %v; want %q", q, m.ID, err, want)
		}
	}
	if _, err := Find(dir, "2026-10-20T09"); err == nil || !strings.Contains(err.Error(), "matches 2 runs") {
		t.Errorf("ambiguous prefix: %v", err)
	}
	if _, err := Find(dir, "nope"); err == nil {
		t.Error("unknown run: want error")
	}
}

func TestIncompleteRunIsListed(t *testing.T) {
	dir := t.TempDir()
	if _, err := Start(dir, "notes", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	runs, _ := List(dir)
	if len(runs) != 1 || runs[0].Status != "incomplete" || runs[0].Task != "notes" {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	for _, d := range []int{40, 31, 29, 1} {
		r, _ := Start(dir, "t", now.AddDate(0, 0, -d), nil)
		r.Finish(Meta{Task: "t"}, "")
	}
	os.Mkdir(filepath.Join(dir, "not-a-run"), 0o700)
	n, err := Prune(dir, 30*24*time.Hour, now)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Fatalf("left %d entries", len(entries))
	}
}
