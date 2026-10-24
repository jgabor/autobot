package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestMoveFile(t *testing.T) {
	e := newEnv(t, nil, []string{"~/dl/"}, nil)
	write(t, filepath.Join(e.home, "dl/a.pdf"), "%PDF\x00binary")
	write(t, filepath.Join(e.home, "dl/b.zip"), "zip")
	write(t, filepath.Join(e.home, "dl/Archives/old.zip"), "old")

	mustCall(t, e.set, "move", map[string]any{"from": "~/dl/a.pdf", "to": "~/dl/Documents/"})
	if !exists(filepath.Join(e.home, "dl/Documents/a.pdf")) || exists(filepath.Join(e.home, "dl/a.pdf")) {
		t.Fatal("file was not moved into a new folder")
	}
	mustCall(t, e.set, "move", map[string]any{"from": "~/dl/b.zip", "to": "~/dl/Archives"})
	if !exists(filepath.Join(e.home, "dl/Archives/b.zip")) {
		t.Fatal("file was not moved into an existing folder")
	}
	mustCall(t, e.set, "move", map[string]any{"from": "~/dl/Archives/b.zip", "to": "~/dl/Archives/c.zip"})
	if !exists(filepath.Join(e.home, "dl/Archives/c.zip")) {
		t.Fatal("file was not renamed")
	}
	moved := e.set.Moved()
	if len(moved) != 3 || moved[0].From != filepath.Join(e.home, "dl/a.pdf") || moved[0].To != filepath.Join(e.home, "dl/Documents/a.pdf") {
		t.Errorf("Moved: %+v", moved)
	}
}

func TestMoveRefusals(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, []string{"~/dl/"}, nil)
	write(t, filepath.Join(e.home, "dl/a.pdf"), "a")
	write(t, filepath.Join(e.home, "dl/Documents/a.pdf"), "other a")
	write(t, filepath.Join(e.home, "dl/sub/x.md"), "x")
	os.Symlink(filepath.Join(e.home, "dl/sub/x.md"), filepath.Join(e.home, "dl/link.md"))
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"from": "~/dl/a.pdf", "to": "~/dl/Documents/"}, "already exists"},
		{map[string]any{"from": "~/dl/a.pdf", "to": "~/notes/a.pdf"}, "outside"},
		{map[string]any{"from": "~/notes/a.md", "to": "~/dl/"}, "outside"},
		{map[string]any{"from": "~/dl/a.pdf", "to": "~/.local/state/autobot/memory/a.pdf"}, "autobot's own files"},
		{map[string]any{"from": "~/dl", "to": "~/dl2"}, "folder listed under `write:`"},
		{map[string]any{"from": "~/dl/link.md", "to": "~/dl/Documents/"}, "symbolic link"},
		{map[string]any{"from": "~/dl/missing.pdf", "to": "~/dl/Documents/"}, "does not exist"},
		{map[string]any{"from": "~/dl/sub", "to": "~/dl/sub/inner"}, "into itself"},
		{map[string]any{"from": "~/dl/a.pdf", "to": "~/dl/a.pdf"}, "same path"},
		{map[string]any{"from": "~/dl/a.pdf"}, "required"},
	} {
		_, err := call(t, e.set, "move", c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("move %v: got %v, want error containing %q", c.args, err, c.want)
		}
	}
	if !exists(filepath.Join(e.home, "dl/a.pdf")) || !exists(filepath.Join(e.home, "dl/link.md")) || len(e.set.Moved()) != 0 {
		t.Error("a refused move changed something")
	}
}

func TestMoveFolder(t *testing.T) {
	e := newEnv(t, nil, []string{"~/dl/"}, nil)
	write(t, filepath.Join(e.home, "dl/Papers/a.pdf"), "a")
	write(t, filepath.Join(e.home, "dl/Papers/deep/b.pdf"), "b")
	write(t, filepath.Join(e.home, "dl/Documents/c.pdf"), "c")
	write(t, filepath.Join(e.home, "dl/Scans/c.pdf"), "other c")
	write(t, filepath.Join(e.home, "dl/Scans/d.pdf"), "d")

	mustCall(t, e.set, "move", map[string]any{"from": "~/dl/Scans", "to": "~/dl/Images"})
	if !exists(filepath.Join(e.home, "dl/Images/d.pdf")) || exists(filepath.Join(e.home, "dl/Scans")) {
		t.Fatal("folder was not renamed")
	}
	out := mustCall(t, e.set, "move", map[string]any{"from": "~/dl/Papers/", "to": "~/dl/Documents/", "merge": true})
	if !strings.Contains(out, "2 entries") || exists(filepath.Join(e.home, "dl/Papers")) ||
		!exists(filepath.Join(e.home, "dl/Documents/a.pdf")) || !exists(filepath.Join(e.home, "dl/Documents/deep/b.pdf")) {
		t.Fatalf("merge: %q", out)
	}
	_, err := call(t, e.set, "move", map[string]any{"from": "~/dl/Images", "to": "~/dl/Documents", "merge": true})
	if err == nil || !strings.Contains(err.Error(), "nothing was moved") || !strings.Contains(err.Error(), "c.pdf") {
		t.Fatalf("merge with a clash: %v", err)
	}
	if !exists(filepath.Join(e.home, "dl/Images/d.pdf")) || exists(filepath.Join(e.home, "dl/Documents/d.pdf")) {
		t.Error("a merge with a clash moved something")
	}
	for _, args := range []map[string]any{
		{"from": "~/dl/Documents/c.pdf", "to": "~/dl/Images", "merge": true},
		{"from": "~/dl/Images", "to": "~/dl/Missing", "merge": true},
	} {
		if _, err := call(t, e.set, "move", args); err == nil || !strings.Contains(err.Error(), "two existing folders") {
			t.Errorf("merge %v: %v", args, err)
		}
	}
}

func TestMoveFolderInside(t *testing.T) {
	e := newEnv(t, nil, []string{"~/dl/"}, nil)
	write(t, filepath.Join(e.home, "dl/Sidebery/a.json"), "a")
	write(t, filepath.Join(e.home, "dl/Tools/b.zip"), "b")
	write(t, filepath.Join(e.home, "dl/Work/x.pdf"), "x")
	write(t, filepath.Join(e.home, "dl/Other/Work/y.pdf"), "y")

	mustCall(t, e.set, "move", map[string]any{"from": "~/dl/Sidebery", "to": "~/dl/Work"})
	if !exists(filepath.Join(e.home, "dl/Work/Sidebery/a.json")) || exists(filepath.Join(e.home, "dl/Work/a.json")) {
		t.Fatal("folder was not moved inside the existing folder")
	}
	mustCall(t, e.set, "move", map[string]any{"from": "~/dl/Tools", "to": "~/dl/Software/"})
	if !exists(filepath.Join(e.home, "dl/Software/Tools/b.zip")) {
		t.Fatal("folder was not moved inside a new folder")
	}
	_, err := call(t, e.set, "move", map[string]any{"from": "~/dl/Other/Work", "to": "~/dl/"})
	if err == nil || !strings.Contains(err.Error(), "already exists") || !exists(filepath.Join(e.home, "dl/Other/Work/y.pdf")) {
		t.Fatalf("moving onto a folder of the same name: %v", err)
	}

	// into needs no trailing / to keep the name, for a new folder too.
	mustCall(t, e.set, "move", map[string]any{"from": "~/dl/Software/Tools", "into": "~/dl/Code"})
	mustCall(t, e.set, "move", map[string]any{"from": "~/dl/Work/x.pdf", "into": "~/dl/Papers"})
	if !exists(filepath.Join(e.home, "dl/Code/Tools/b.zip")) || !exists(filepath.Join(e.home, "dl/Papers/x.pdf")) {
		t.Fatal("into did not keep the names")
	}
	for _, args := range []map[string]any{
		{"from": "~/dl/Papers/x.pdf", "into": "~/dl/Work", "to": "~/dl/Work/x.pdf"},
		{"from": "~/dl/Code", "into": "~/dl/Work", "merge": true},
	} {
		if _, err := call(t, e.set, "move", args); err == nil || !strings.Contains(err.Error(), "into on its own") {
			t.Errorf("move %v: %v", args, err)
		}
	}
}

func TestMoveDryRun(t *testing.T) {
	e := newEnv(t, nil, []string{"~/dl/"}, func(o *Options) { o.DryRun = true })
	write(t, filepath.Join(e.home, "dl/a.pdf"), "a")
	write(t, filepath.Join(e.home, "dl/Old/b.pdf"), "b")
	write(t, filepath.Join(e.home, "dl/New/c.pdf"), "c")
	if out := mustCall(t, e.set, "move", map[string]any{"from": "~/dl/a.pdf", "to": "~/dl/Documents/"}); !strings.Contains(out, "dry run") {
		t.Errorf("dry-run result must say the move was not done: %q", out)
	}
	mustCall(t, e.set, "move", map[string]any{"from": "~/dl/Old", "to": "~/dl/New"})
	if !exists(filepath.Join(e.home, "dl/a.pdf")) || exists(filepath.Join(e.home, "dl/Documents")) || !exists(filepath.Join(e.home, "dl/Old/b.pdf")) {
		t.Fatal("a dry run changed the disk")
	}
	if m := e.set.Moved(); len(m) != 2 || m[1].To != filepath.Join(e.home, "dl/New/Old") {
		t.Errorf("dry-run moves: %+v", m)
	}
}
