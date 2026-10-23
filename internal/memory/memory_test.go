package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRender(t *testing.T) {
	text := `# Memory: notes

## Locations

- [m-1] Notes live in ~/Documents/Notes
- [m-7] Drafts are in _old/

## Gotchas

- written by hand without an id
plain line without a bullet

## Custom heading

- [m-2] kept under its own heading
`
	d := Parse(text, "Memory: notes", "m")
	if d.Len() != 5 {
		t.Fatalf("items: %d\n%s", d.Len(), d.Render())
	}
	out := d.Render()
	for _, want := range []string{"- [m-1] Notes live", "- [m-7] Drafts", "- [m-8] written by hand", "- [m-9] plain line", "## Custom heading", "- [m-2] kept", "## Follow-ups"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	again := Parse(out, "", "m")
	if again.Render() != out {
		t.Errorf("render is not stable:\n%s\n---\n%s", out, again.Render())
	}
}

func TestParseDropsForeignAndDuplicateIDs(t *testing.T) {
	d := Parse("## Insights\n- [s-1] from shared\n- [m-1] one\n- [m-1] dup\n", "t", "m")
	ids := map[string]bool{}
	for _, s := range d.Sections {
		for _, it := range s.Items {
			if ids[it.ID] || !strings.HasPrefix(it.ID, "m-") {
				t.Errorf("bad id %q in\n%s", it.ID, d.Render())
			}
			ids[it.ID] = true
		}
	}
}

func TestApply(t *testing.T) {
	task := Parse("## Locations\n- [m-1] old location\n## Gotchas\n- [m-2] stale\n", "t", "m")
	shared := New("Shared memory", "s")
	nt, ns, err := Apply(task, shared, []Op{
		{Op: "add", Section: "follow-ups", Text: "check   the\nreport"},
		{Op: "add", Scope: "shared", Section: "Insights", Text: "user prefers short lists"},
		{Op: "update", ID: "m-1", Text: "new location"},
		{Op: "remove", ID: "m-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := nt.Render()
	for _, want := range []string{"- [m-1] new location", "- [m-3] check the report"} {
		if !strings.Contains(out, want) {
			t.Errorf("task memory missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stale") {
		t.Errorf("removed item still present:\n%s", out)
	}
	if !strings.Contains(ns.Render(), "- [s-1] user prefers short lists") {
		t.Errorf("shared memory:\n%s", ns.Render())
	}
	if strings.Contains(task.Render(), "new location") {
		t.Error("Apply changed its input")
	}
}

func TestApplyRejectsInvalidLists(t *testing.T) {
	task := Parse("## Locations\n- [m-1] x\n", "t", "m")
	shared := New("Shared memory", "s")
	bad := map[string][]Op{
		"unknown op":       {{Op: "replace", ID: "m-1", Text: "y"}},
		"unknown id":       {{Op: "remove", ID: "m-9"}},
		"unknown prefix":   {{Op: "remove", ID: "x-1"}},
		"unknown section":  {{Op: "add", Section: "Feelings", Text: "y"}},
		"empty text":       {{Op: "add", Section: "Gotchas", Text: "  "}},
		"add with id":      {{Op: "add", ID: "m-5", Section: "Gotchas", Text: "y"}},
		"bad scope":        {{Op: "add", Scope: "global", Section: "Gotchas", Text: "y"}},
		"too long":         {{Op: "add", Section: "Gotchas", Text: strings.Repeat("a", MaxItemText+1)}},
		"second op is bad": {{Op: "add", Section: "Gotchas", Text: "fine"}, {Op: "remove", ID: "m-99"}},
	}
	for name, ops := range bad {
		if _, _, err := Apply(task, shared, ops); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestStoreChangeDiffUndo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	s := &Store{Dir: dir, MaxBytes: 2000}
	if err := s.Change("notes", []Op{{Op: "add", Section: "Locations", Text: "first fact"}}, "reflect: notes (run1)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Change("notes", []Op{{Op: "add", Section: "Gotchas", Text: "second fact"}}, "reflect: notes (run2)"); err != nil {
		t.Fatal(err)
	}
	diff, err := s.Diff(1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "reflect: notes (run2)") || !strings.Contains(diff, "+- [m-2] second fact") || strings.Contains(diff, "run1") {
		t.Fatalf("diff:\n%s", diff)
	}
	if all, _ := s.Diff(5); !strings.Contains(all, "run1") || !strings.Contains(all, "run2") {
		t.Fatalf("diff of 5:\n%s", all)
	}

	msg, err := s.Undo(false)
	if err != nil || msg != "reflect: notes (run2)" {
		t.Fatalf("undo: %q %v", msg, err)
	}
	task, _, _ := s.Load("notes")
	if strings.Contains(task.Render(), "second fact") || !strings.Contains(task.Render(), "first fact") {
		t.Fatalf("after undo:\n%s", task.Render())
	}
	if dirty, _ := s.Dirty(); dirty {
		t.Fatal("undo must commit")
	}
	// A second undo steps back one more change.
	msg, err = s.Undo(false)
	if err != nil || msg != "reflect: notes (run1)" {
		t.Fatalf("second undo: %q %v", msg, err)
	}
	task, _, _ = s.Load("notes")
	if strings.Contains(task.Render(), "fact") {
		t.Fatalf("after two undos:\n%s", task.Render())
	}
	if _, err := s.Undo(false); err == nil {
		t.Fatal("nothing left to undo: want error")
	}
}

func TestStoreSizeLimitLeavesFilesUntouched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	s := &Store{Dir: dir, MaxBytes: 600}
	if err := s.Change("notes", []Op{{Op: "add", Section: "Locations", Text: "small"}}, "first"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "notes.md"))
	err := s.Change("notes", []Op{{Op: "add", Section: "Insights", Text: strings.Repeat("b", 400)}, {Op: "add", Section: "Insights", Text: strings.Repeat("c", 400)}}, "too big")
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("want size error, got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "notes.md"))
	if string(before) != string(after) {
		t.Fatal("rejected change modified the file")
	}
}

func TestHandEditsAreKeptSeparate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	s := &Store{Dir: dir}
	if err := s.Change("notes", []Op{{Op: "add", Section: "Locations", Text: "auto"}}, "reflect 1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "notes.md")
	data, _ := os.ReadFile(path)
	os.WriteFile(path, append(data, []byte("- added by hand\n")...), 0o644)
	if dirty, _ := s.Dirty(); !dirty {
		t.Fatal("hand edit not seen")
	}
	if _, err := s.Undo(false); err != ErrDirty {
		t.Fatalf("undo with hand edits: want ErrDirty, got %v", err)
	}
	if err := s.Change("notes", []Op{{Op: "add", Section: "Gotchas", Text: "auto 2"}}, "reflect 2"); err != nil {
		t.Fatal(err)
	}
	// Undo reverts only the reflection, not the hand edit committed before it.
	if _, err := s.Undo(false); err != nil {
		t.Fatal(err)
	}
	task, _, _ := s.Load("notes")
	out := task.Render()
	if !strings.Contains(out, "added by hand") || strings.Contains(out, "auto 2") {
		t.Fatalf("after undo:\n%s", out)
	}
}

func TestUndoForceDiscardsHandEdits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	s := &Store{Dir: dir}
	s.Change("notes", []Op{{Op: "add", Section: "Locations", Text: "one"}}, "reflect 1")
	s.Change("notes", []Op{{Op: "add", Section: "Locations", Text: "two"}}, "reflect 2")
	os.WriteFile(filepath.Join(dir, "scratch.md"), []byte("- junk\n"), 0o644)
	if _, err := s.Undo(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch.md")); err == nil {
		t.Fatal("force undo kept an uncommitted file")
	}
	task, _, _ := s.Load("notes")
	if strings.Contains(task.Render(), "two") {
		t.Fatal("force undo did not revert")
	}
}

func TestNothingToUndo(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "memory")}
	if _, err := s.Undo(false); err != ErrNoHistory {
		t.Fatalf("empty repo: %v", err)
	}
	if _, err := s.Diff(1); err != ErrNoHistory {
		t.Fatalf("empty repo diff: %v", err)
	}
}

func TestOverLimitFileCanShrinkAndOtherFileStillChanges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	big := New("Shared memory", "s")
	for i := 0; i < 30; i++ {
		big.Sections[0].Items = append(big.Sections[0].Items, Item{Text: strings.Repeat("x", 40)})
	}
	big = Parse(big.Render(), "Shared memory", "s")
	s := &Store{Dir: dir, MaxBytes: 600}
	if err := s.Write(SharedName, big, "import"); err != nil {
		t.Fatal(err)
	}
	if err := s.Change("notes", []Op{{Op: "add", Section: "Gotchas", Text: "task fact"}}, "task only"); err != nil {
		t.Fatalf("a task-only change must not fail because shared.md is large: %v", err)
	}
	if err := s.Change("notes", []Op{{Op: "remove", ID: "s-1"}}, "shrink"); err != nil {
		t.Fatalf("shrinking an over-limit file must be allowed: %v", err)
	}
	if err := s.Change("notes", []Op{{Op: "add", Scope: "shared", Section: "Gotchas", Text: "more"}}, "grow"); err == nil {
		t.Fatal("growing an over-limit file must be rejected")
	}
}

func TestNoOpChangeDoesNotCommitOrUndoHandEdits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	s := &Store{Dir: dir}
	s.Change("notes", []Op{{Op: "add", Section: "Locations", Text: "fact"}}, "reflect 1")
	path := filepath.Join(dir, "notes.md")
	data, _ := os.ReadFile(path)
	os.WriteFile(path, append(data, []byte("- by hand\n")...), 0o644)
	if err := s.Change("notes", []Op{{Op: "update", ID: "m-1", Text: "fact"}}, "reflect 2"); err != ErrNoChange {
		t.Fatalf("same text: want ErrNoChange, got %v", err)
	}
	s.Commit(manualEdit)
	if _, err := s.Undo(false); err == nil || !strings.Contains(err.Error(), "your own edit") {
		t.Fatalf("undo of a hand edit: %v", err)
	}
	task, _, _ := s.Load("notes")
	if !strings.Contains(task.Render(), "by hand") {
		t.Fatal("hand edit lost")
	}
}

func TestForceUndoAfterHandDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	s := &Store{Dir: dir}
	s.Change("a", []Op{{Op: "add", Section: "Locations", Text: "one"}}, "reflect a")
	s.Change("b", []Op{{Op: "add", Section: "Locations", Text: "two"}}, "reflect b")
	os.Remove(filepath.Join(dir, "b.md"))
	if _, err := s.Undo(true); err != nil {
		t.Fatalf("force undo after a hand delete: %v", err)
	}
}
