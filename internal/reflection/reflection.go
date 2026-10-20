// Package reflection asks the model what a task run taught it, as a list of
// memory changes, and applies the changes if they are valid.
package reflection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jgabor/autobot/internal/agent"
	"github.com/jgabor/autobot/internal/memory"
	"github.com/jgabor/autobot/internal/provider"
)

// Input describes the run to reflect on.
type Input struct {
	TaskID     string
	Prompt     string
	Status     string // "completed" or "failed"
	Final      string
	Error      string
	Transcript []provider.Message
	RunID      string
	MaxBytes   int
	// NoShared keeps the task away from shared memory. Tasks with network
	// access get it, so what one task read cannot leak through another.
	NoShared bool
}

// Outcome is what reflection did.
type Outcome struct {
	Ops     []memory.Op
	Summary string
}

const system = `You maintain the memory of autobot, a tool that runs recurring chores with an AI model.

After each task run you decide what is worth remembering for future runs of this task, or of every task. Memory is small and must stay useful.

Keep:
- Locations: paths, files and folders that matter, and what is in them.
- Procedures: steps that worked and should be repeated.
- Gotchas: mistakes made, assumptions that failed, and how to avoid them.
- Insights: durable facts about the user's files or setup.
- Follow-ups: work left open that a future run should check.

Rules:
- Only record durable knowledge. Skip one-off events and narration.
- Remove items that are wrong, stale, done or duplicated. Update items that changed.
- Use scope "shared" only for facts every task benefits from. Default to the task.
- Never record secrets, keys, passwords or personal data beyond what the task needs.
- Each item is one short line.
- An empty list is a good answer when nothing new was learned.

Reply with only a JSON array of changes, with no other text:
[
  {"op": "add", "scope": "task", "section": "Gotchas", "text": "..."},
  {"op": "update", "id": "m-3", "text": "..."},
  {"op": "remove", "id": "s-2"}
]
Sections: Locations, Procedures, Gotchas, Insights, Follow-ups. Item ids starting with m- are task memory, s- are shared memory.`

const maxTranscript = 12000

// Summarize turns a transcript into a compact list of tool calls and results.
func Summarize(msgs []provider.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "- call %s %s\n", tc.Function.Name, clip(tc.Function.Arguments, 300))
			}
			if t := strings.TrimSpace(agent.StripThinking(m.Content)); t != "" && len(m.ToolCalls) > 0 {
				fmt.Fprintf(&b, "- note: %s\n", clip(t, 300))
			}
		case "tool":
			fmt.Fprintf(&b, "  result: %s\n", clip(m.Content, 300))
		}
	}
	s := b.String()
	if len(s) > maxTranscript {
		s = "[earlier calls omitted]\n" + s[len(s)-maxTranscript:]
	}
	if s == "" {
		return "(no tool calls)"
	}
	return s
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Run asks for memory changes and applies them. If the reply is invalid it
// asks once more with the error; if that also fails, memory is untouched.
// redact removes secrets from item text before it is stored.
func Run(ctx context.Context, p provider.Provider, model string, store *memory.Store, in Input, redact func(string) string) (Outcome, error) {
	task, shared, err := store.Load(in.TaskID)
	if err != nil {
		return Outcome{}, err
	}
	var user strings.Builder
	fmt.Fprintf(&user, "Task: %s\n\nTask prompt:\n%s\n\nStatus: %s\n", in.TaskID, in.Prompt, in.Status)
	if in.Error != "" {
		fmt.Fprintf(&user, "Error: %s\n", in.Error)
	}
	if in.Final != "" {
		fmt.Fprintf(&user, "\nFinal answer:\n%s\n", clip(in.Final, 4000))
	}
	fmt.Fprintf(&user, "\nTool calls:\n%s\n", Summarize(in.Transcript))
	fmt.Fprintf(&user, "\nCurrent task memory (%d of %d bytes):\n%s\n", len(task.Render()), in.MaxBytes, task.Render())
	if in.NoShared {
		user.WriteString("\nThis task has network access, so it cannot read or change shared memory. Use task scope only.\n")
	} else {
		fmt.Fprintf(&user, "\nCurrent shared memory (%d of %d bytes):\n%s\n", len(shared.Render()), in.MaxBytes, shared.Render())
	}

	msgs := []provider.Message{{Role: "system", Content: system}, {Role: "user", Content: user.String()}}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := p.Chat(ctx, provider.Request{Model: model, Messages: msgs})
		if err != nil {
			return Outcome{}, &agent.ProviderError{Err: err}
		}
		ops, err := ParseOps(resp.Message.Content)
		if err == nil && in.NoShared {
			err = checkNoShared(ops)
		}
		if err == nil {
			if redact != nil {
				for i := range ops {
					ops[i].Text = redact(ops[i].Text)
				}
			}
			msg := fmt.Sprintf("reflect: %s (%s)", in.TaskID, in.RunID)
			if len(ops) == 0 {
				return Outcome{Summary: "no memory changes"}, nil
			}
			err = store.Change(in.TaskID, ops, msg)
			if errors.Is(err, memory.ErrNoChange) {
				return Outcome{Summary: "no memory changes"}, nil
			}
			if err == nil {
				return Outcome{Ops: ops, Summary: describe(ops)}, nil
			}
		}
		lastErr = err
		msgs = append(msgs,
			provider.Message{Role: "assistant", Content: resp.Message.Content},
			provider.Message{Role: "user", Content: "That change list was rejected: " + err.Error() + "\nReply with a corrected JSON array only."})
	}
	return Outcome{}, fmt.Errorf("reflection rejected, memory unchanged: %w", lastErr)
}

func describe(ops []memory.Op) string {
	counts := map[string]int{}
	for _, op := range ops {
		counts[op.Op]++
	}
	var parts []string
	for _, k := range []string{"add", "update", "remove"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], map[string]string{"add": "added", "update": "updated", "remove": "removed"}[k]))
		}
	}
	return "memory: " + strings.Join(parts, ", ")
}

// ParseOps reads a JSON array of ops from a reply, allowing a code fence or
// a thinking block around it.
func ParseOps(reply string) ([]memory.Op, error) {
	s := strings.TrimSpace(agent.StripThinking(reply))
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			s = strings.TrimSpace(rest[:j])
		}
	}
	start, end := strings.IndexByte(s, '['), strings.LastIndexByte(s, ']')
	if start < 0 || end < start {
		return nil, errors.New("the reply does not contain a JSON array")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(s[start : end+1])))
	dec.DisallowUnknownFields()
	var ops []memory.Op
	if err := dec.Decode(&ops); err != nil {
		return nil, fmt.Errorf("the reply is not a valid change list: %w", err)
	}
	return ops, nil
}

func checkNoShared(ops []memory.Op) error {
	for i, op := range ops {
		if op.Scope == "shared" || strings.HasPrefix(op.ID, "s-") {
			return fmt.Errorf("change %d: this task cannot change shared memory; use task scope", i+1)
		}
	}
	return nil
}
