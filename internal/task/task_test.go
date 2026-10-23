package task

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var defaults = Defaults{Schedule: "daily", MaxRounds: 20}

func TestParseFullHeader(t *testing.T) {
	src := `---
schedule: weekly
read: [~/Documents/Notes]
write:
  - ~/Documents/Notes/next-actions.md
network: true
commands:
  - systemctl --user list-units --type=service
model: small-model
max_rounds: 5
enabled: false
---
Review my notes.
`
	tk, err := Parse("/x/review-notes.md", []byte(src), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if tk.ID != "review-notes" || tk.Schedule.String() != "weekly" || !tk.Network || tk.Enabled ||
		tk.Model != "small-model" || tk.MaxRounds != 5 || tk.Prompt != "Review my notes." {
		t.Fatalf("unexpected task: %+v", tk)
	}
	if !reflect.DeepEqual(tk.Read, []string{"~/Documents/Notes"}) || !reflect.DeepEqual(tk.Write, []string{"~/Documents/Notes/next-actions.md"}) {
		t.Fatalf("paths: %v %v", tk.Read, tk.Write)
	}
}

func TestParseDefaults(t *testing.T) {
	tk, err := Parse("/x/plain.md", []byte("Just a prompt.\n"), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Schedule.String() != "daily" || tk.MaxRounds != 20 || !tk.Enabled || tk.Network || len(tk.Read) != 0 {
		t.Fatalf("defaults not applied: %+v", tk)
	}
	tk, err = Parse("/x/empty-header.md", []byte("---\n---\nPrompt."), defaults)
	if err != nil || tk.Prompt != "Prompt." {
		t.Fatalf("empty header: %+v %v", tk, err)
	}
	tk, err = Parse("/x/comments.md", []byte("---\n# schedule: weekly\n# read: [~/x]\n---\nPrompt."), defaults)
	if err != nil || tk.Schedule.String() != "daily" {
		t.Fatalf("comment-only header: %+v %v", tk, err)
	}
	tk, err = Parse("/x/crlf.md", []byte("---\r\nschedule: hourly\r\n---\r\nPrompt.\r\n"), defaults)
	if err != nil || tk.Schedule.String() != "hourly" {
		t.Fatalf("CRLF: %+v %v", tk, err)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct{ name, src, want string }{
		"unknown field":   {"a.md", "---\nshedule: daily\n---\nx", "shedule"},
		"bad schedule":    {"a.md", "---\nschedule: monthly\n---\nx", "monthly"},
		"no prompt":       {"a.md", "---\nschedule: daily\n---\n\n", "no prompt"},
		"relative read":   {"a.md", "---\nread: [Documents]\n---\nx", "must start with"},
		"unclosed header": {"a.md", "---\nschedule: daily\nx", "closing"},
		"bad name":        {"Bad Name.md", "x", "lowercase"},
		"reserved name":   {"shared.md", "x", "reserved"},
		"digest folder":   {"digests.md", "x", "reserved"},
		"shell command":   {"a.md", "---\ncommands: [\"ls | head\"]\n---\nx", "not supported"},
		"negative rounds": {"a.md", "---\nmax_rounds: -1\n---\nx", "positive"},
		"wrong type":      {"a.md", "---\nread: {a: b}\n---\nx", "header"},
	}
	for name, c := range cases {
		_, err := Parse("/x/"+c.name, []byte(c.src), defaults)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want error containing %q", name, err, c.want)
		}
	}
}

func TestLoadAllKeepsGoodTasks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b-good.md"), []byte("Prompt."), 0o644)
	os.WriteFile(filepath.Join(dir, "a-good.md"), []byte("Prompt."), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.md"), []byte("---\nnope: 1\n---\nx"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)
	tasks, invalid, err := LoadAll(dir, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].ID != "a-good" || tasks[1].ID != "b-good" {
		t.Fatalf("tasks: %+v", tasks)
	}
	if len(invalid) != 1 || invalid[0].ID != "broken" {
		t.Fatalf("invalid: %+v", invalid)
	}
	if _, _, err := LoadAll(filepath.Join(dir, "missing"), defaults); err == nil || !strings.Contains(err.Error(), "autobot init") {
		t.Fatalf("missing folder: %v", err)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		"systemctl --user list-units": {"systemctl", "--user", "list-units"},
		`echo "a b" 'c d' e\ f`:       {"echo", "a b", "c d", "e f"},
		`echo "say \"hi\""`:           {"echo", `say "hi"`},
		"  spaced   out  ":            {"spaced", "out"},
		"ls *.md":                     {"ls", "*.md"},
		"df -h /home":                 {"df", "-h", "/home"},
		"printf %s\\n x":              {"printf", "%sn", "x"},
	}
	for in, want := range cases {
		got, err := SplitCommand(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("SplitCommand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"ls | head", "a && b", "echo $HOME", "echo `id`", "cat > x", `echo "open`, "echo 'open", "ls ~/x", "x;y"} {
		if _, err := SplitCommand(bad); err == nil {
			t.Errorf("SplitCommand(%q): want error", bad)
		}
	}
}

func TestBareTildeMeansHome(t *testing.T) {
	for _, h := range []string{"read: [~]", "read:\n  - ~", "read: ~", "read: ~ # all of home", `read: ["~"]`} {
		tk, err := Parse("/x/a.md", []byte("---\n"+h+"\n---\nx"), defaults)
		if err != nil || !reflect.DeepEqual(tk.Read, []string{"~"}) {
			t.Errorf("%q: read %q, err %v", h, tk.Read, err)
		}
	}
	tk, err := Parse("/x/a.md", []byte("---\nread: ~/notes\nwrite:\n---\nx"), defaults)
	if err != nil || !reflect.DeepEqual(tk.Read, []string{"~/notes"}) || len(tk.Write) != 0 {
		t.Errorf("single path and empty field: %q %q %v", tk.Read, tk.Write, err)
	}
}

func TestHeaderToleratesSmallSlips(t *testing.T) {
	for _, src := range []string{"\n---\nenabled: false\n---\nPrompt.", "--- \nenabled: false\n---\t\nPrompt."} {
		tk, err := Parse("/x/a.md", []byte(src), defaults)
		if err != nil || tk.Enabled || tk.Prompt != "Prompt." {
			t.Errorf("%q: %+v %v", src, tk, err)
		}
	}
}
