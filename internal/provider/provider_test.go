package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEndpoint(t *testing.T) {
	cases := map[string]string{
		"http://localhost:11434/v1":                   "http://localhost:11434/v1/chat/completions",
		"http://localhost:11434/v1/":                  "http://localhost:11434/v1/chat/completions",
		"https://api.example.com":                     "https://api.example.com/v1/chat/completions",
		"https://api.example.com/v1/chat/completions": "https://api.example.com/v1/chat/completions",
		"https://openrouter.ai/api/v1":                "https://openrouter.ai/api/v1/chat/completions",
	}
	for in, want := range cases {
		if got := Endpoint(in); got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChatWireFormat(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("path %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		// Arguments as an object and null content, as some local servers send.
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"read","arguments":{"path":"a.md"}}},
			{"id":"c2","type":"function","function":{"name":"list","arguments":"{\"pattern\":\"*\"}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer srv.Close()
	o := &OpenAI{URL: srv.URL + "/v1", Key: "k"}
	resp, err := o.Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "read", Parameters: map[string]any{"type": "object"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "m" || got["tool_choice"] != "auto" || got["stream"] != false {
		t.Errorf("request body: %v", got)
	}
	if len(resp.Message.ToolCalls) != 2 || resp.Message.ToolCalls[0].Function.Arguments != `{"path":"a.md"}` ||
		resp.Message.ToolCalls[1].Function.Arguments != `{"pattern":"*"}` || resp.Message.Content != "" {
		t.Errorf("response: %+v", resp.Message)
	}
	if resp.Usage.PromptTokens != 10 || resp.FinishReason != "tool_calls" {
		t.Errorf("usage: %+v", resp)
	}
}

func TestChatContentParts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":[{"type":"text","text":"Hello "},{"type":"text","text":"there"}]}}]}`)
	}))
	defer srv.Close()
	resp, err := (&OpenAI{URL: srv.URL}).Chat(context.Background(), Request{Model: "m"})
	if err != nil || resp.Message.Content != "Hello there" || resp.Message.Role != "assistant" {
		t.Fatalf("parts: %+v %v", resp, err)
	}
}

func TestChatRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "busy")
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()
	o := &OpenAI{URL: srv.URL, Retries: 2, Backoff: time.Millisecond}
	resp, err := o.Chat(context.Background(), Request{Model: "m"})
	if err != nil || resp.Message.Content != "ok" || calls.Load() != 3 {
		t.Fatalf("retry: %v %q after %d calls", err, resp.Message.Content, calls.Load())
	}
}

func TestChatDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"model not found"}}`)
	}))
	defer srv.Close()
	o := &OpenAI{URL: srv.URL, Retries: 3, Backoff: time.Millisecond}
	_, err := o.Chat(context.Background(), Request{Model: "m"})
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 400 || !strings.Contains(err.Error(), "model not found") || calls.Load() != 1 {
		t.Fatalf("client error: %v after %d calls", err, calls.Load())
	}
}

func TestChatErrors(t *testing.T) {
	bodies := map[string]string{
		"no choices":   `{"choices":[]}`,
		"invalid json": `not json`,
		"error object": `{"error":{"message":"context too long"}}`,
	}
	for name, body := range bodies {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		if _, err := (&OpenAI{URL: srv.URL}).Chat(context.Background(), Request{Model: "m"}); err == nil {
			t.Errorf("%s: want error", name)
		}
		srv.Close()
	}
	_, err := (&OpenAI{URL: "http://127.0.0.1:1"}).Chat(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Error("connection refused: want error")
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	_, err := (&OpenAI{URL: srv.URL, Timeout: 20 * time.Millisecond}).Chat(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("want timeout error")
	}
}
