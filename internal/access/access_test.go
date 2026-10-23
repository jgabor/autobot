package access

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixture struct {
	home   string
	output string
	policy *Policy
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T, read, write []string) fixture {
	t.Helper()
	home := t.TempDir()
	// Resolve the temp dir itself, which may sit behind a symlink.
	home, _ = filepath.EvalSymlinks(home)
	output := filepath.Join(home, ".local/state/autobot/inbox/task")
	mustMkdir(t, output)
	mustMkdir(t, filepath.Join(home, ".config/autobot"))
	mustWrite(t, filepath.Join(home, ".config/autobot/config.toml"), "secret")
	mustWrite(t, filepath.Join(home, ".local/state/autobot/memory/shared.md"), "memory")
	mustWrite(t, filepath.Join(home, "notes/a.md"), "a")
	mustWrite(t, filepath.Join(home, "notes/sub/b.md"), "b")
	mustWrite(t, filepath.Join(home, "notes-evil/x.md"), "x")
	mustWrite(t, filepath.Join(home, "secret/s.md"), "s")
	mustLink(t, "../secret", filepath.Join(home, "notes/link-to-secret"))
	mustLink(t, "../secret/s.md", filepath.Join(home, "notes/link.md"))
	mustMkdir(t, filepath.Join(home, "other"))
	mustLink(t, "../notes/a.md", filepath.Join(home, "other/into-notes.md"))
	p, err := New(Options{
		Home:      home,
		Output:    output,
		Read:      read,
		Write:     write,
		Protected: []string{filepath.Join(home, ".config/autobot"), filepath.Join(home, ".local/state/autobot")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{home, output, p}
}

func TestEscapeAttempts(t *testing.T) {
	f := setup(t, []string{"~/notes"}, []string{"~/notes/next.md"})
	cases := []struct {
		name  string
		op    string
		path  string
		allow bool
	}{
		{"file in read root", "read", "~/notes/a.md", true},
		{"nested file in read root", "read", "~/notes/sub/b.md", true},
		{"read root itself with trailing slash", "read", "~/notes/", true},
		{"dot dot out of root", "read", "~/notes/../secret/s.md", false},
		{"dot dot staying in root", "read", "~/notes/sub/../a.md", true},
		{"symlinked folder pointing out", "read", "~/notes/link-to-secret/s.md", false},
		{"symlink to folder outside", "read", "~/notes/link-to-secret", false},
		{"symlinked file pointing out", "read", "~/notes/link.md", false},
		{"symlink from outside into root", "read", "~/other/into-notes.md", true},
		{"sibling with shared prefix", "read", "~/notes-evil/x.md", false},
		{"different case", "read", "~/Notes/a.md", false},
		{"home itself", "read", "~", false},
		{"absolute path outside", "read", "/etc/passwd", false},
		{"output folder by relative path", "read", "result.md", true},
		{"output folder by absolute path", "read", "OUTPUT/result.md", true},
		{"relative path escaping output", "read", "../other-task/x.md", false},
		{"autobot config", "read", "~/.config/autobot/config.toml", false},
		{"autobot memory", "read", "~/.local/state/autobot/memory/shared.md", false},
		{"named write file", "write", "~/notes/next.md", true},
		{"other file in read root", "write", "~/notes/other.md", false},
		{"dot dot from write file", "write", "~/notes/next.md/../evil.md", false},
		{"new file under symlinked folder", "write", "~/notes/link-to-secret/new.md", false},
		{"output folder", "write", "report.md", true},
		{"new folder in output", "write", "sub/dir/report.md", true},
		{"escape output to inbox", "write", "../escape.md", false},
		{"escape output to memory", "write", "../../memory/shared.md", false},
		{"double slash", "write", "~//notes//next.md", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := strings.Replace(c.path, "OUTPUT", f.output, 1)
			var err error
			if c.op == "read" {
				_, err = f.policy.CheckRead(path)
			} else {
				_, err = f.policy.CheckWrite(path)
			}
			if c.allow && err != nil {
				t.Fatalf("%s %s: want allowed, got %v", c.op, path, err)
			}
			if !c.allow && err == nil {
				t.Fatalf("%s %s: want denied, got allowed", c.op, path)
			}
		})
	}
}

func TestProtectedEvenWhenHomeIsReadable(t *testing.T) {
	f := setup(t, []string{"~"}, []string{"~"})
	for _, p := range []string{"~/.config/autobot/config.toml", "~/.local/state/autobot/memory/shared.md", "~/.local/state/autobot/schedule.json"} {
		if _, err := f.policy.CheckRead(p); err == nil || !strings.Contains(err.Error(), "autobot's own files") {
			t.Errorf("read %s: want protected error, got %v", p, err)
		}
		if _, err := f.policy.CheckWrite(p); err == nil {
			t.Errorf("write %s: want denied", p)
		}
	}
	if _, err := f.policy.CheckRead("~/notes/a.md"); err != nil {
		t.Errorf("read under home: %v", err)
	}
	if !f.policy.CanRead(filepath.Join(f.output, "x")) {
		t.Error("output folder inside protected state folder must stay readable")
	}
}

func TestBadPaths(t *testing.T) {
	f := setup(t, []string{"~/notes"}, nil)
	for _, p := range []string{"", "~/notes/a\x00.md"} {
		if _, err := f.policy.CheckRead(p); err == nil {
			t.Errorf("path %q: want error", p)
		}
	}
}

func TestDeniedMessageNamesTheFix(t *testing.T) {
	f := setup(t, []string{"~/notes"}, nil)
	_, err := f.policy.CheckRead("~/secret/s.md")
	if err == nil {
		t.Fatal("want denied")
	}
	for _, want := range []string{"~/secret/s.md", "read paths", "~/notes", "`read:`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRootThatDoesNotExistYet(t *testing.T) {
	f := setup(t, nil, []string{"~/reports/weekly.md"})
	if _, err := f.policy.CheckWrite("~/reports/weekly.md"); err != nil {
		t.Fatalf("write to a root that does not exist yet: %v", err)
	}
	if _, err := f.policy.CheckWrite("~/reports/other.md"); err == nil {
		t.Fatal("want denied")
	}
}

func TestWriteFolderRootWithSymlinksOut(t *testing.T) {
	f := setup(t, nil, []string{"~/notes"})
	for _, p := range []string{"~/notes/link.md", "~/notes/link-to-secret/new.md", "~/notes/link-to-secret/s.md"} {
		if _, err := f.policy.CheckWrite(p); err == nil {
			t.Errorf("write %s: want denied, the link points outside the root", p)
		}
	}
	if _, err := f.policy.CheckWrite("~/notes/new.md"); err != nil {
		t.Errorf("write new file in folder root: %v", err)
	}
}

func TestSymlinksInsideProtectedFolders(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	// A dotfile manager keeps the real files elsewhere and links them in.
	mustWrite(t, filepath.Join(home, "dotfiles/autobot/environment"), "OPENAI_API_KEY=sk-secret")
	mustWrite(t, filepath.Join(home, "dotfiles/autobot/tasks/a.md"), "task")
	mustMkdir(t, filepath.Join(home, ".config/autobot"))
	mustLink(t, filepath.Join(home, "dotfiles/autobot/environment"), filepath.Join(home, ".config/autobot/environment"))
	mustLink(t, filepath.Join(home, "dotfiles/autobot/tasks"), filepath.Join(home, ".config/autobot/tasks"))
	output := filepath.Join(home, ".local/state/autobot/inbox/task")
	mustMkdir(t, output)
	p, err := New(Options{Home: home, Output: output, Read: []string{"~"}, Write: []string{"~"},
		Protected: []string{filepath.Join(home, ".config/autobot"), filepath.Join(home, ".local/state/autobot")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"~/.config/autobot/environment",
		"~/dotfiles/autobot/environment",
		"~/.config/autobot/tasks/a.md",
		"~/dotfiles/autobot/tasks/a.md",
		"~/dotfiles/autobot/tasks/new.md",
	} {
		if _, err := p.CheckRead(path); err == nil {
			t.Errorf("read %s: want denied", path)
		}
		if _, err := p.CheckWrite(path); err == nil {
			t.Errorf("write %s: want denied", path)
		}
	}
	if _, err := p.CheckRead("~/dotfiles/other.md"); err != nil {
		t.Errorf("unrelated dotfile: %v", err)
	}
}

func TestProcIsProtected(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	output := filepath.Join(home, "out")
	mustMkdir(t, output)
	p, err := New(Options{Home: home, Output: output, Read: []string{"/"}, Protected: []string{"/proc"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/proc/self/environ", "/proc/1/environ"} {
		if _, err := p.CheckRead(path); err == nil {
			t.Errorf("read %s: want denied", path)
		}
	}
	if _, err := p.CheckRead(filepath.Join(home, "x")); err != nil {
		t.Errorf("protecting /proc must not protect everything: %v", err)
	}
}
