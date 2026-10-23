// Package agent runs the loop between the model and a task's tools.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jgabor/autobot/internal/provider"
)

// Tools is what the loop needs from a tool set.
type Tools interface {
	Definitions() []provider.Tool
	Call(ctx context.Context, name, arguments string) (string, error)
}

// Config is one agent run.
type Config struct {
	Provider            provider.Provider
	Model               string
	System              string
	Prompt              string
	Tools               Tools
	MaxRounds           int
	ContextWindow       int
	MaxToolOutputTokens int
	// OnMessage is called for every message added to the conversation.
	OnMessage func(provider.Message)
}

// Result is what a run produced.
type Result struct {
	Final     string
	Rounds    int
	ToolCalls map[string]int
	Usage     provider.Usage
	Messages  []provider.Message
}

// ErrMaxRounds means the model kept calling tools past the round limit.
var ErrMaxRounds = errors.New("the model used its maximum number of tool rounds without giving a final answer")

// ErrContext means the conversation no longer fits the context window.
var ErrContext = errors.New("the conversation no longer fits in the context window")

// ProviderError wraps a failed provider request, so callers can tell a
// broken provider from a task that went wrong.
type ProviderError struct{ Err error }

func (e *ProviderError) Error() string { return e.Err.Error() }
func (e *ProviderError) Unwrap() error { return e.Err }

// EstimateTokens guesses a token count from text length.
func EstimateTokens(s string) int { return (utf8.RuneCountInString(s) + 3) / 4 }

const removedNote = "[earlier tool output removed to fit the context window]"

// Run drives the conversation until the model gives a final answer.
func Run(ctx context.Context, c Config) (Result, error) {
	res := Result{ToolCalls: map[string]int{}}
	var defs []provider.Tool
	if c.Tools != nil {
		defs = c.Tools.Definitions()
	}
	defsJSON, _ := json.Marshal(defs)
	defsTokens := EstimateTokens(string(defsJSON))

	add := func(m provider.Message) {
		res.Messages = append(res.Messages, m)
		if c.OnMessage != nil {
			c.OnMessage(m)
		}
	}
	add(provider.Message{Role: "system", Content: c.System})
	add(provider.Message{Role: "user", Content: c.Prompt})

	// ratio calibrates the length estimate against the server's own count.
	ratio := 1.0
	for {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		estimate := func() int {
			n := defsTokens
			for _, m := range res.Messages {
				n += messageTokens(m)
			}
			return int(float64(n) * ratio)
		}
		est := estimate()
		if c.ContextWindow > 0 {
			budget := c.ContextWindow * 85 / 100
			for i := range res.Messages {
				if est <= budget {
					break
				}
				m := &res.Messages[i]
				if m.Role == "tool" && m.Content != removedNote {
					m.Content = removedNote
					est = estimate()
				}
			}
			if est > budget {
				return res, fmt.Errorf("%w (about %d tokens, limit %d)", ErrContext, est, c.ContextWindow)
			}
		}

		resp, err := c.Provider.Chat(ctx, provider.Request{Model: c.Model, Messages: res.Messages, Tools: defs})
		if err != nil {
			return res, &ProviderError{err}
		}
		res.Usage.PromptTokens += resp.Usage.PromptTokens
		res.Usage.CompletionTokens += resp.Usage.CompletionTokens
		if resp.Usage.PromptTokens > 0 && est > 0 {
			ratio = max(0.5, min(4, ratio*float64(resp.Usage.PromptTokens)/float64(est)))
		}

		msg := resp.Message
		msg.Role = "assistant"
		if len(msg.ToolCalls) == 0 {
			final := strings.TrimSpace(StripThinking(msg.Content))
			add(msg)
			if final == "" {
				return res, errors.New("the model returned an empty reply")
			}
			res.Final = final
			return res, nil
		}
		if res.Rounds >= c.MaxRounds {
			add(msg)
			return res, fmt.Errorf("%w (%d)", ErrMaxRounds, c.MaxRounds)
		}
		res.Rounds++
		for i := range msg.ToolCalls {
			if msg.ToolCalls[i].ID == "" {
				msg.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d", res.Rounds, i)
			}
			if msg.ToolCalls[i].Type == "" {
				msg.ToolCalls[i].Type = "function"
			}
		}
		add(msg)
		for _, call := range msg.ToolCalls {
			out := runTool(ctx, c, call)
			res.ToolCalls[call.Function.Name]++
			add(provider.Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: out})
		}
	}
}

func runTool(ctx context.Context, c Config, call provider.ToolCall) string {
	name := call.Function.Name
	if name == "" {
		return "Tool error: the tool call has no function name"
	}
	args := call.Function.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return "Tool error: arguments are not valid JSON: " + truncate(args, 200)
	}
	if c.Tools == nil {
		return fmt.Sprintf("Tool error: unknown tool %q", name)
	}
	out, err := c.Tools.Call(ctx, name, args)
	if err != nil {
		msg := "Tool error: " + err.Error()
		if out != "" {
			msg += "\n" + out
		}
		return CutOutput(msg, c.MaxToolOutputTokens)
	}
	return CutOutput(out, c.MaxToolOutputTokens)
}

// CutOutput shortens a tool result to about maxTokens and says what was cut.
func CutOutput(out string, maxTokens int) string {
	if maxTokens <= 0 || EstimateTokens(out) <= maxTokens {
		return out
	}
	keep := maxTokens * 4
	runes := []rune(out)
	return string(runes[:keep]) + fmt.Sprintf(
		"\n[output cut: showed the first %d of %d characters. Ask for less, for example with offset and limit, a narrower pattern or a smaller path.]",
		keep, len(runes))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func messageTokens(m provider.Message) int {
	n := 4 + EstimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		n += 4 + EstimateTokens(tc.Function.Name) + EstimateTokens(tc.Function.Arguments)
	}
	return n
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// StripThinking removes <think> blocks some reasoning models put in replies.
// Some servers send only the closing tag, because the opening one is part of
// the prompt template; then everything before it is thinking.
func StripThinking(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	return s
}
