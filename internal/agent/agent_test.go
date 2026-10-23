package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jgabor/autobot/internal/provider"
)

// fakeTools echoes calls and fails on demand.
type fakeTools struct {
	calls []string
	out   map[string]string
}

func (f *fakeTools) Definitions() []provider.Tool {
	return []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "read"}}}
}

func (f *fakeTools) Call(_ context.Context, name, args string) (string, error) {
	f.calls = append(f.calls, name+" "+args)
	if name == "fail" {
		return "", errors.New("access denied: nope")
	}
	if out, ok := f.out[name]; ok {
		return out, nil
	}
	return "result of " + name, nil
}

func run(t *testing.T, replies []provider.Reply, mod func(*Config)) (Result, *provider.Fake, *fakeTools, error) {
	t.Helper()
	fake := &provider.Fake{Replies: replies}
	tools := &fakeTools{out: map[string]string{}}
	c := Config{Provider: fake, Model: "m", System: "sys", Prompt: "do it", Tools: tools, MaxRounds: 5, ContextTokens: 100000, MaxToolOutputTokens: 1000}
	if mod != nil {
		mod(&c)
	}
	res, err := Run(context.Background(), c)
	return res, fake, tools, err
}

func TestFinalAnswer(t *testing.T) {
	res, fake, _, err := run(t, []provider.Reply{provider.Text("  Done.  ")}, nil)
	if err != nil || res.Final != "Done." || res.Rounds != 0 {
		t.Fatalf("got %+v, %v", res, err)
	}
	req := fake.Requests[0]
	if req.Messages[0].Role != "system" || req.Messages[1].Content != "do it" || len(req.Tools) != 1 {
		t.Fatalf("request: %+v", req)
	}
}

func TestToolRoundTrip(t *testing.T) {
	var seen []provider.Message
	res, fake, tools, err := run(t, []provider.Reply{
		provider.Calls(provider.Call("c1", "read", `{"path":"a"}`), provider.Call("c2", "list", `{}`)),
		provider.Text("Read it."),
	}, func(c *Config) { c.OnMessage = func(m provider.Message) { seen = append(seen, m) } })
	if err != nil || res.Final != "Read it." || res.Rounds != 1 {
		t.Fatalf("got %+v, %v", res, err)
	}
	if len(tools.calls) != 2 || tools.calls[0] != `read {"path":"a"}` {
		t.Fatalf("calls: %v", tools.calls)
	}
	second := fake.Requests[1].Messages
	last := second[len(second)-1]
	if last.Role != "tool" || last.ToolCallID != "c2" || last.Content != "result of list" {
		t.Fatalf("tool result message: %+v", last)
	}
	if res.ToolCalls["read"] != 1 || res.ToolCalls["list"] != 1 {
		t.Fatalf("tool counts: %v", res.ToolCalls)
	}
	// system, user, assistant, tool, tool, assistant
	if len(seen) != 6 {
		t.Fatalf("OnMessage saw %d messages", len(seen))
	}
}

func TestToolErrorsGoBackToTheModel(t *testing.T) {
	_, fake, _, err := run(t, []provider.Reply{
		provider.Calls(
			provider.Call("c1", "fail", `{}`),
			provider.Call("c2", "read", `{not json`),
			provider.Call("", "", `{}`),
			provider.Call("", "read", ``),
		),
		provider.Text("Gave up."),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := fake.Requests[1].Messages
	results := msgs[len(msgs)-4:]
	wants := []string{"Tool error: access denied", "Tool error: arguments are not valid JSON", "Tool error: the tool call has no function name", "result of read"}
	for i, w := range wants {
		if !strings.HasPrefix(results[i].Content, w) {
			t.Errorf("result %d: %q, want prefix %q", i, results[i].Content, w)
		}
	}
	assistant := msgs[len(msgs)-5]
	for _, tc := range assistant.ToolCalls {
		if tc.ID == "" || tc.Type != "function" {
			t.Errorf("tool call without id or type: %+v", tc)
		}
	}
	if results[2].ToolCallID == "" || results[2].ToolCallID == results[3].ToolCallID {
		t.Errorf("generated ids must be unique: %q %q", results[2].ToolCallID, results[3].ToolCallID)
	}
}

func TestMaxRounds(t *testing.T) {
	var replies []provider.Reply
	for range 4 {
		replies = append(replies, provider.Calls(provider.Call("c", "read", `{}`)))
	}
	res, _, tools, err := run(t, replies, func(c *Config) { c.MaxRounds = 3 })
	if !errors.Is(err, ErrMaxRounds) || res.Rounds != 3 || len(tools.calls) != 3 {
		t.Fatalf("got rounds %d calls %d err %v", res.Rounds, len(tools.calls), err)
	}
}

func TestEmptyReply(t *testing.T) {
	_, _, _, err := run(t, []provider.Reply{provider.Text("<think>hmm</think>  ")}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty reply") {
		t.Fatalf("want empty reply error, got %v", err)
	}
}

func TestThinkingIsStripped(t *testing.T) {
	res, _, _, err := run(t, []provider.Reply{provider.Text("<think>\nplan\n</think>\nAnswer.")}, nil)
	if err != nil || res.Final != "Answer." {
		t.Fatalf("got %q, %v", res.Final, err)
	}
}

func TestProviderErrorIsMarked(t *testing.T) {
	_, _, _, err := run(t, []provider.Reply{{Err: errors.New("connection refused")}}, nil)
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want ProviderError, got %T %v", err, err)
	}
}

func TestLongToolOutputIsCut(t *testing.T) {
	fake := &provider.Fake{Replies: []provider.Reply{
		provider.Calls(provider.Call("c1", "big", `{}`)),
		provider.Text("ok"),
	}}
	tools := &fakeTools{out: map[string]string{"big": strings.Repeat("w", 5000)}}
	_, err := Run(context.Background(), Config{Provider: fake, Model: "m", System: "s", Prompt: "p", Tools: tools,
		MaxRounds: 5, ContextTokens: 100000, MaxToolOutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	msgs := fake.Requests[1].Messages
	got := msgs[len(msgs)-1].Content
	if len(got) > 700 || !strings.Contains(got, "[output cut: showed the first 400 of 5000 characters") {
		t.Fatalf("tool output not cut: %d chars, %q", len(got), got[len(got)-150:])
	}
}

func TestCutOutput(t *testing.T) {
	long := strings.Repeat("x", 2000)
	out := CutOutput(long, 100)
	if !strings.HasPrefix(out, strings.Repeat("x", 400)) || strings.HasPrefix(out, strings.Repeat("x", 401)) || !strings.Contains(out, "output cut") || !strings.Contains(out, "of 2000 characters") {
		t.Fatalf("cut: %q", out[390:])
	}
	if CutOutput("short", 100) != "short" {
		t.Fatal("short output must not change")
	}
}

func TestOldToolOutputMakesRoom(t *testing.T) {
	big := strings.Repeat("y", 8000) // about 2000 tokens
	fake := &provider.Fake{Replies: []provider.Reply{
		provider.Calls(provider.Call("c1", "read", `{}`)),
		provider.Calls(provider.Call("c2", "read", `{}`)),
		provider.Calls(provider.Call("c3", "read", `{}`)),
		provider.Text("done"),
	}}
	tools := &fakeTools{out: map[string]string{"read": big}}
	res, err := Run(context.Background(), Config{Provider: fake, Model: "m", System: "s", Prompt: "p", Tools: tools,
		MaxRounds: 5, ContextTokens: 5000, MaxToolOutputTokens: 4000})
	if err != nil || res.Final != "done" {
		t.Fatalf("got %+v %v", res.Final, err)
	}
	last := fake.Requests[3].Messages
	removed := 0
	for _, m := range last {
		if m.Role == "tool" && m.Content == removedNote {
			removed++
		}
	}
	if removed == 0 || last[len(last)-1].Content != big {
		t.Fatalf("expected the oldest outputs removed and the newest kept; removed %d", removed)
	}
}

func TestContextOverflow(t *testing.T) {
	_, err := Run(context.Background(), Config{Provider: &provider.Fake{}, Model: "m", System: strings.Repeat("z", 40000), Prompt: "p",
		MaxRounds: 5, ContextTokens: 1000})
	if !errors.Is(err, ErrContext) {
		t.Fatalf("want ErrContext, got %v", err)
	}
}

func TestLoneClosingThinkTag(t *testing.T) {
	if got := strings.TrimSpace(StripThinking("reasoning here\n</think>\nAnswer.")); got != "Answer." {
		t.Fatalf("got %q", got)
	}
}

func TestToolErrorOutputIsCut(t *testing.T) {
	fake := &provider.Fake{Replies: []provider.Reply{provider.Calls(provider.Call("c1", "noisy", `{}`)), provider.Text("ok")}}
	_, err := Run(context.Background(), Config{Provider: fake, Model: "m", System: "s", Prompt: "p", Tools: noisyTools{},
		MaxRounds: 5, ContextTokens: 100000, MaxToolOutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	msgs := fake.Requests[1].Messages
	if got := msgs[len(msgs)-1].Content; len(got) > 700 || !strings.HasPrefix(got, "Tool error: timed out") {
		t.Fatalf("error output not cut: %d chars", len(got))
	}
}

type noisyTools struct{}

func (noisyTools) Definitions() []provider.Tool { return nil }
func (noisyTools) Call(context.Context, string, string) (string, error) {
	return strings.Repeat("n", 100000), errors.New("timed out")
}
