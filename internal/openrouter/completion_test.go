package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ani-telegram/internal/limits"
	"ani-telegram/internal/safeio"
)

func TestCompletionRejectsEmptyAndTruncated(t *testing.T) {
	cases := []completionResult{
		{Message: Message{Content: "  "}, FinishReason: "stop"},
		{Message: Message{Content: nil}, FinishReason: "stop"},
		{Message: Message{Content: "cut"}, FinishReason: "length"},
		{Message: Message{Content: "ignored", ToolCalls: []ToolCall{{ID: "call"}}}, FinishReason: "length"},
	}
	for _, r := range cases {
		if err := validateCompletion(r, false); err == nil {
			t.Fatalf("accepted invalid completion reason=%q", r.FinishReason)
		}
	}
}

func TestCompletionAcceptsLegacyTextAndToolOnlyResponse(t *testing.T) {
	if err := validateCompletion(completionResult{Message: Message{Content: "ok"}}, false); err != nil {
		t.Fatalf("legacy response rejected: %v", err)
	}
	call := ToolCall{ID: "call", Type: "function"}
	call.Function.Name = "load"
	call.Function.Arguments = `{}`
	toolOnly := completionResult{Message: Message{Content: nil, ToolCalls: []ToolCall{call}}, FinishReason: "tool_calls"}
	if err := validateCompletion(toolOnly, true); err != nil {
		t.Fatalf("tool-only response rejected: %v", err)
	}
	if err := validateCompletion(toolOnly, false); err == nil {
		t.Fatal("tool-only response accepted when tools are disabled")
	}
}

func TestCompletionRejectsMalformedToolCallsBeforeExecution(t *testing.T) {
	makeCall := func(id, typ, name, arguments string) ToolCall {
		call := ToolCall{ID: id, Type: typ}
		call.Function.Name = name
		call.Function.Arguments = arguments
		return call
	}
	tests := []struct {
		name  string
		calls []ToolCall
	}{
		{name: "blank id", calls: []ToolCall{makeCall(" ", "function", "load", `{}`)}},
		{name: "wrong type", calls: []ToolCall{makeCall("1", "other", "load", `{}`)}},
		{name: "blank name", calls: []ToolCall{makeCall("1", "function", " ", `{}`)}},
		{name: "arguments are not json", calls: []ToolCall{makeCall("1", "function", "load", `{`)}},
		{name: "arguments are not object", calls: []ToolCall{makeCall("1", "function", "load", `[]`)}},
		{name: "duplicate ids", calls: []ToolCall{makeCall("1", "function", "first", `{}`), makeCall("1", "function", "second", `{}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCompletion(completionResult{Message: Message{ToolCalls: tt.calls}, FinishReason: "tool_calls"}, true)
			var invalid *CompletionError
			if !errors.As(err, &invalid) || invalid.Kind != "invalid_tool_completion" {
				t.Fatalf("err=%v", err)
			}
		})
	}

	var executions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"","type":"function","function":{"name":"load","arguments":"{}"}}]}}]}`))
	}))
	defer server.Close()
	tool := Tool{Type: "function", Function: ToolFunction{Name: "load"}}
	_, err := NewClient("k", server.URL, "m").ChatWithTools(context.Background(), "system", nil, "hello", []Tool{tool}, func(_, _ string) (string, error) {
		executions.Add(1)
		return "result", nil
	}, nil)
	var invalid *CompletionError
	if !errors.As(err, &invalid) || invalid.Kind != "invalid_tool_completion" {
		t.Fatalf("err=%v", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("executions=%d", executions.Load())
	}
}

func TestCompletionUsesConfiguredReplyLimitForTextAndToolResponses(t *testing.T) {
	responses := []string{
		completionJSON("12345", "stop"),
		`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"12345","tool_calls":[{"id":"1","type":"function","function":{"name":"load","arguments":"{}"}}]}}]}`,
	}
	for i, response := range responses {
		t.Run([]string{"text", "tool"}[i], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(response)) }))
			defer server.Close()
			lim := limits.Default()
			lim.ModelReplyBytes = 4
			client := NewClient("k", server.URL, "m", lim)
			if i == 0 {
				_, err := client.Chat(context.Background(), "system", nil, "hello", nil)
				var tooLarge *safeio.TooLargeError
				if !errors.As(err, &tooLarge) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			tool := Tool{Type: "function", Function: ToolFunction{Name: "load"}}
			var executions atomic.Int32
			_, err := client.ChatWithTools(context.Background(), "system", nil, "hello", []Tool{tool}, func(_, _ string) (string, error) { executions.Add(1); return "result", nil }, nil)
			var tooLarge *safeio.TooLargeError
			if !errors.As(err, &tooLarge) {
				t.Fatalf("err=%v", err)
			}
			if executions.Load() != 0 {
				t.Fatalf("executions=%d", executions.Load())
			}
		})
	}
}

func TestCompletionRecoveryRetriesBoundedly(t *testing.T) {
	tests := []struct {
		name      string
		responses []string
		want      string
		wantKind  string
	}{
		{name: "empty then ok", responses: []string{completionJSON("  ", "stop"), completionJSON("ok", "stop")}, want: "ok"},
		{name: "length then ok", responses: []string{completionJSON("cut", "length"), completionJSON("complete", "stop")}, want: "complete"},
		{name: "three empty", responses: []string{completionJSON("", "stop"), completionJSON("", "stop"), completionJSON("", "stop")}, wantKind: "empty_completion"},
		{name: "mixed invalid", responses: []string{completionJSON("", "stop"), completionJSON("cut", "length"), completionJSON("", "stop")}, wantKind: "empty_completion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				i := int(attempts.Add(1)) - 1
				_, _ = w.Write([]byte(tt.responses[i]))
			}))
			defer server.Close()
			got, err := NewClient("k", server.URL, "m").Chat(context.Background(), "system", nil, "hello", nil)
			if tt.wantKind == "" {
				if err != nil || got != tt.want {
					t.Fatalf("got=%q err=%v", got, err)
				}
			} else {
				var invalid *CompletionError
				if !errors.As(err, &invalid) || invalid.Kind != tt.wantKind {
					t.Fatalf("err=%v", err)
				}
			}
			if gotAttempts := int(attempts.Load()); gotAttempts != len(tt.responses) {
				t.Fatalf("attempts=%d want=%d", gotAttempts, len(tt.responses))
			}
		})
	}
}

func TestCompletionCancellationStopsRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		cancel()
		_, _ = w.Write([]byte(completionJSON("", "stop")))
	}))
	defer server.Close()
	_, err := NewClient("k", server.URL, "m").Chat(ctx, "system", nil, "hello", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts=%d", attempts.Load())
	}
}

func TestCompletionJSONLengthConsumesRetryBudget(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := attempts.Add(1)
		reason := "length"
		if i == 2 {
			reason = "stop"
		}
		_, _ = w.Write([]byte(completionJSON(`{"ok":true}`, reason)))
	}))
	defer server.Close()
	got, err := NewClient("k", server.URL, "m").ChatJSON(context.Background(), "system", nil, "hello", nil)
	if err != nil || got != `{"ok":true}` {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d", attempts.Load())
	}
}

func TestCompletionToolExecutionIsNotRepeatedForEmptyFinalText(t *testing.T) {
	var requests, executions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch requests.Add(1) {
		case 1:
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"1","type":"function","function":{"name":"load","arguments":"{}"}}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(completionJSON("", "stop")))
		default:
			_, _ = w.Write([]byte(completionJSON("done", "stop")))
		}
	}))
	defer server.Close()
	tool := Tool{Type: "function", Function: ToolFunction{Name: "load"}}
	got, err := NewClient("k", server.URL, "m").ChatWithTools(context.Background(), "system", nil, "hello", []Tool{tool}, func(_, _ string) (string, error) {
		executions.Add(1)
		return "result", nil
	}, nil)
	if err != nil || got != "done" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions=%d", executions.Load())
	}
}

func TestCompletionFinishReasonIsResponseMetadataOnly(t *testing.T) {
	body, err := NewClient("k", "http://example.test", "m").encodeRequest(chatRequest{Model: "m", Messages: []Message{{Role: "assistant", Content: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	r, err := body.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var decoded map[string]any
	if err := json.NewDecoder(r).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(decoded["messages"])
	if strings.Contains(string(raw), "finish_reason") {
		t.Fatalf("serialized messages contain response metadata: %s", raw)
	}
}

func TestBuildMessagesCoalescesAdjacentUserInputs(t *testing.T) {
	img := NewImageContent("photo", "image.jpg", "image/jpeg")
	messages := buildMessages("system", []Message{{Role: "user", Content: "first"}, {Role: "user", Content: "second"}}, img)
	if len(messages) != 2 {
		t.Fatalf("messages=%#v", messages)
	}
	got, ok := messages[1].Content.(ImageContent)
	if !ok {
		t.Fatalf("content type %T", messages[1].Content)
	}
	if got.Text != "first\nsecond\nphoto" || got.Path != img.Path || got.MIME != img.MIME {
		t.Fatalf("image=%#v", got)
	}
}

func TestBuildMessagesPreservesToolTranscriptBoundary(t *testing.T) {
	history := []Message{{Role: "user", Content: "before"}, {Role: "assistant", Content: nil, ToolCalls: []ToolCall{{ID: "1"}}}, {Role: "tool", ToolCallID: "1", Content: "result"}}
	messages := buildMessages("system", history, "after")
	if len(messages) != 5 {
		t.Fatalf("messages=%#v", messages)
	}
	if messages[1].Content != "before" || messages[4].Content != "after" {
		t.Fatalf("tool transcript was coalesced: %#v", messages)
	}
}

func completionJSON(content, reason string) string {
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": reason, "message": map[string]any{"role": "assistant", "content": content}}}})
	return string(raw)
}
