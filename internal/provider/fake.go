package provider

import (
	"context"
	"errors"
	"sync"
)

// Fake returns scripted replies. Tests use it in place of a real server.
type Fake struct {
	mu       sync.Mutex
	Replies  []Reply
	Requests []Request
}

// Reply is one scripted response or error.
type Reply struct {
	Response Response
	Err      error
}

// Text makes a reply with a final answer.
func Text(content string) Reply {
	return Reply{Response: Response{Message: Message{Role: "assistant", Content: content}, FinishReason: "stop"}}
}

// Calls makes a reply that asks for tool calls.
func Calls(calls ...ToolCall) Reply {
	return Reply{Response: Response{Message: Message{Role: "assistant", ToolCalls: calls}, FinishReason: "tool_calls"}}
}

// Call builds one tool call.
func Call(id, name, args string) ToolCall {
	return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: args}}
}

// Chat records the request and returns the next reply.
func (f *Fake) Chat(_ context.Context, req Request) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := req
	cp.Messages = append([]Message(nil), req.Messages...)
	f.Requests = append(f.Requests, cp)
	if len(f.Replies) == 0 {
		return Response{}, errors.New("fake provider: no scripted reply left")
	}
	r := f.Replies[0]
	f.Replies = f.Replies[1:]
	return r.Response, r.Err
}
