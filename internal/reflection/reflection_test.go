package reflection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jgabor/autobot/internal/agent"
	"github.com/jgabor/autobot/internal/memory"
	"github.com/jgabor/autobot/internal/provider"
)

func newStore(t *testing.T) *memory.Store {
	return &memory.Store{Dir: filepath.Join(t.TempDir(), "memory"), MaxBytes: 4000}
}

func input() Input {
	return Input{
		TaskID: "notes", Prompt: "Review notes.", Status: "completed", Final: "Found 3 tasks.", RunID: "run1", MaxBytes: 4000,
		Transcript: []provider.Message{
			{Role: "assistant", ToolCalls: []provider.ToolCall{provider.Call("c1", "list", `{"pattern":"~/Notes/*.md"}`)}},
			{Role: "tool", Name: "list", Content: "~/Notes/a.md"},
		},
	}
}

func TestAppliesValidChanges(t *testing.T) {
	store := newStore(t)
	fake := &provider.Fake{Replies: []provider.Reply{provider.Text("```json\n[{\"op\":\"add\",\"section\":\"Locations\",\"text\":\"Notes are in ~/Notes\"}]\n```")}}
	out, err := Run(context.Background(), fake, "m", store, input(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary != "memory: 1 added" {
		t.Errorf("summary %q", out.Summary)
	}
	task, _, _ := store.Load("notes")
	if !strings.Contains(task.Render(), "Notes are in ~/Notes") {
		t.Fatalf("memory:\n%s", task.Render())
	}
	prompt := fake.Requests[0].Messages[1].Content
	for _, want := range []string{"Review notes.", "Found 3 tasks.", "call list", "~/Notes/a.md", "Current task memory"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("reflection prompt missing %q", want)
		}
	}
}

func TestRetriesOnceWithTheError(t *testing.T) {
	store := newStore(t)
	fake := &provider.Fake{Replies: []provider.Reply{
		provider.Text(`[{"op":"remove","id":"m-42"}]`),
		provider.Text(`[{"op":"add","section":"Gotchas","text":"fixed"}]`),
	}}
	if _, err := Run(context.Background(), fake, "m", store, input(), nil); err != nil {
		t.Fatal(err)
	}
	retry := fake.Requests[1].Messages
	if !strings.Contains(retry[len(retry)-1].Content, "unknown id") {
		t.Fatalf("retry did not include the error: %q", retry[len(retry)-1].Content)
	}
}

func TestInvalidChangesLeaveMemoryUntouched(t *testing.T) {
	store := newStore(t)
	if err := store.Change("notes", []memory.Op{{Op: "add", Section: "Locations", Text: "keep me"}}, "seed"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(store.Dir, "notes.md"))
	for name, reply := range map[string]string{
		"not json":      "I learned a lot today.",
		"unknown field": `[{"op":"add","section":"Gotchas","text":"x","priority":1}]`,
		"bad op":        `[{"op":"delete","id":"m-1"}]`,
	} {
		fake := &provider.Fake{Replies: []provider.Reply{provider.Text(reply), provider.Text(reply)}}
		_, err := Run(context.Background(), fake, "m", store, input(), nil)
		if err == nil || !strings.Contains(err.Error(), "memory unchanged") {
			t.Errorf("%s: want rejection, got %v", name, err)
		}
		after, _ := os.ReadFile(filepath.Join(store.Dir, "notes.md"))
		if string(before) != string(after) {
			t.Errorf("%s: memory changed", name)
		}
	}
}

func TestEmptyListChangesNothing(t *testing.T) {
	store := newStore(t)
	fake := &provider.Fake{Replies: []provider.Reply{provider.Text("<think>nothing new</think>[]")}}
	out, err := Run(context.Background(), fake, "m", store, input(), nil)
	if err != nil || out.Summary != "no memory changes" {
		t.Fatalf("got %+v %v", out, err)
	}
	if _, err := os.Stat(store.Dir); err == nil {
		if _, err := store.Diff(1); err != memory.ErrNoHistory {
			t.Fatal("empty list must not commit")
		}
	}
}

func TestRedactsSecrets(t *testing.T) {
	store := newStore(t)
	fake := &provider.Fake{Replies: []provider.Reply{provider.Text(`[{"op":"add","section":"Insights","text":"the key is sk-secret-123"}]`)}}
	redact := func(s string) string { return strings.ReplaceAll(s, "sk-secret-123", "[redacted]") }
	if _, err := Run(context.Background(), fake, "m", store, input(), redact); err != nil {
		t.Fatal(err)
	}
	task, _, _ := store.Load("notes")
	if strings.Contains(task.Render(), "sk-secret") {
		t.Fatal("secret stored in memory")
	}
}

func TestProviderFailure(t *testing.T) {
	fake := &provider.Fake{Replies: []provider.Reply{{Err: errors.New("down")}}}
	_, err := Run(context.Background(), fake, "m", newStore(t), input(), nil)
	var pe *agent.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want ProviderError, got %v", err)
	}
}

func TestParseOps(t *testing.T) {
	ops, err := ParseOps("Here you go:\n[{\"op\":\"remove\",\"id\":\"s-1\"}]\nThanks")
	if err != nil || len(ops) != 1 || ops[0].ID != "s-1" {
		t.Fatalf("got %+v %v", ops, err)
	}
	if _, err := ParseOps("{}"); err == nil {
		t.Fatal("object instead of array: want error")
	}
}

func TestNetworkTasksCannotTouchSharedMemory(t *testing.T) {
	store := newStore(t)
	store.Change("other", []memory.Op{{Op: "add", Scope: "shared", Section: "Insights", Text: "private fact"}}, "seed")
	fake := &provider.Fake{Replies: []provider.Reply{
		provider.Text(`[{"op":"add","scope":"shared","section":"Insights","text":"from the web"}]`),
		provider.Text(`[{"op":"add","section":"Insights","text":"task only"}]`),
	}}
	in := input()
	in.NoShared = true
	if _, err := Run(context.Background(), fake, "m", store, in, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fake.Requests[0].Messages[1].Content, "private fact") {
		t.Error("shared memory shown to a network task")
	}
	_, shared, _ := store.Load("notes")
	if strings.Contains(shared.Render(), "from the web") {
		t.Error("network task changed shared memory")
	}
}
