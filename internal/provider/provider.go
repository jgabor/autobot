// Package provider talks to an OpenAI-compatible chat completions endpoint.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message is one chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// UnmarshalJSON accepts null content, which servers send with tool calls.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []ToolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = Message{Role: raw.Role, ToolCalls: raw.ToolCalls, ToolCallID: raw.ToolCallID, Name: raw.Name}
	if len(raw.Content) > 0 && string(raw.Content) != "null" {
		if err := json.Unmarshal(raw.Content, &m.Content); err != nil {
			// Some servers send content as a list of parts.
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err2 := json.Unmarshal(raw.Content, &parts); err2 != nil {
				return fmt.Errorf("message content: %w", err)
			}
			var b strings.Builder
			for _, p := range parts {
				b.WriteString(p.Text)
			}
			m.Content = b.String()
		}
	}
	return nil
}

// ToolCall is a model's request to run a tool.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the tool and carries its JSON arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UnmarshalJSON accepts arguments as a JSON string, as the API specifies, or
// as an object, which some local servers send.
func (f *FunctionCall) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.Name = raw.Name
	f.Arguments = ""
	if len(raw.Arguments) == 0 || string(raw.Arguments) == "null" {
		return nil
	}
	if raw.Arguments[0] == '"' {
		return json.Unmarshal(raw.Arguments, &f.Arguments)
	}
	f.Arguments = string(raw.Arguments)
	return nil
}

// Tool describes a function the model may call.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is a tool's name, purpose and JSON schema.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Request is one chat completion request.
type Request struct {
	Model    string
	Messages []Message
	Tools    []Tool
}

// Usage is the token count a server reports.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Response is the first choice of a chat completion.
type Response struct {
	Message      Message
	FinishReason string
	Usage        Usage
}

// Provider sends chat requests.
type Provider interface {
	Chat(ctx context.Context, req Request) (Response, error)
}

// Error is a failed request. Status is 0 for network errors.
type Error struct {
	Status int
	Body   string
	Err    error
}

func (e *Error) Error() string {
	switch {
	case e.Err != nil:
		return "provider request failed: " + e.Err.Error()
	case e.Body != "":
		return fmt.Sprintf("provider returned HTTP %d: %s", e.Status, e.Body)
	default:
		return fmt.Sprintf("provider returned HTTP %d", e.Status)
	}
}

func (e *Error) Unwrap() error { return e.Err }

func (e *Error) retryable() bool {
	return e.Err != nil || e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// OpenAI is a client for OpenAI-compatible chat completions.
type OpenAI struct {
	URL     string
	Key     string
	Timeout time.Duration
	Client  *http.Client
	// Retries is how many times a failed request is retried on rate limits,
	// server errors and network errors.
	Retries int
	Backoff time.Duration
}

// Endpoint turns a base URL into the chat completions URL.
func Endpoint(base string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(base, "/chat/completions"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/chat/completions"
	default:
		return base + "/v1/chat/completions"
	}
}

type wireRequest struct {
	Model      string    `json:"model"`
	Messages   []Message `json:"messages"`
	Tools      []Tool    `json:"tools,omitempty"`
	ToolChoice any       `json:"tool_choice,omitempty"`
	Stream     bool      `json:"stream"`
}

type wireResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat sends one request, retrying transient failures.
func (o *OpenAI) Chat(ctx context.Context, req Request) (Response, error) {
	w := wireRequest{Model: req.Model, Messages: req.Messages, Tools: req.Tools}
	if len(req.Tools) > 0 {
		w.ToolChoice = "auto"
	}
	payload, err := json.Marshal(w)
	if err != nil {
		return Response{}, err
	}
	backoff := o.Backoff
	if backoff == 0 {
		backoff = 2 * time.Second
	}
	var lastErr error
	for attempt := 0; attempt <= o.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Response{}, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		resp, err := o.send(ctx, payload)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var pe *Error
		if !errors.As(err, &pe) || !pe.retryable() || ctx.Err() != nil {
			break
		}
	}
	return Response{}, lastErr
}

func (o *OpenAI) send(ctx context.Context, payload []byte) (Response, error) {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint(o.URL), bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.Key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.Key)
	}
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, &Error{Err: err}
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 32<<20))
	if err != nil {
		return Response{}, &Error{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		return Response{}, &Error{Status: httpResp.StatusCode, Body: snippet(body)}
	}
	var wr wireResponse
	if err := json.Unmarshal(body, &wr); err != nil {
		return Response{}, &Error{Status: httpResp.StatusCode, Body: "invalid JSON: " + snippet(body)}
	}
	if wr.Error != nil {
		return Response{}, &Error{Status: httpResp.StatusCode, Body: wr.Error.Message}
	}
	if len(wr.Choices) == 0 {
		return Response{}, &Error{Status: httpResp.StatusCode, Body: "response has no choices: " + snippet(body)}
	}
	out := Response{Message: wr.Choices[0].Message, FinishReason: wr.Choices[0].FinishReason}
	if out.Message.Role == "" {
		out.Message.Role = "assistant"
	}
	if wr.Usage != nil {
		out.Usage = *wr.Usage
	}
	return out, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	return s
}
