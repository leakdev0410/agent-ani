package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

type completionResult struct {
	Message      Message
	FinishReason string
}

type CompletionError struct{ Kind string }

func (e *CompletionError) Error() string { return "openrouter: " + e.Kind }

const maxCompletionAttempts = 3

func validateCompletion(r completionResult, allowTools bool) error {
	switch r.FinishReason {
	case "", "stop":
	case "tool_calls":
		if !allowTools || !validToolCalls(r.Message.ToolCalls) {
			return &CompletionError{Kind: "invalid_tool_completion"}
		}
		return nil
	case "length":
		return &CompletionError{Kind: "truncated_completion"}
	case "content_filter":
		return &CompletionError{Kind: "filtered_completion"}
	case "error":
		return &CompletionError{Kind: "error_completion"}
	default:
		return &CompletionError{Kind: "unknown_finish_reason"}
	}
	if len(r.Message.ToolCalls) > 0 {
		if allowTools && validToolCalls(r.Message.ToolCalls) {
			return nil
		}
		return &CompletionError{Kind: "invalid_tool_completion"}
	}
	text, ok := r.Message.Content.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return &CompletionError{Kind: "empty_completion"}
	}
	return nil
}

func validToolCalls(calls []ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	ids := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.ID) == "" || call.Type != "function" || strings.TrimSpace(call.Function.Name) == "" {
			return false
		}
		if _, duplicate := ids[call.ID]; duplicate {
			return false
		}
		ids[call.ID] = struct{}{}
		var arguments map[string]json.RawMessage
		if json.Unmarshal([]byte(call.Function.Arguments), &arguments) != nil || arguments == nil {
			return false
		}
	}
	return true
}

func (c *Client) complete(ctx context.Context, messages []Message, format *responseFormat, tools []Tool, onRetry RetryObserver) (Message, error) {
	requestMessages := append([]Message(nil), messages...)
	var last error
	for attempt := 1; attempt <= maxCompletionAttempts; attempt++ {
		result, err := c.chat(ctx, requestMessages, format, tools, onRetry)
		if err != nil {
			return Message{}, err
		}
		last = validateCompletion(result, len(tools) > 0)
		if last == nil {
			if _, err := c.checkedContent(result.Message.Content); err != nil {
				return Message{}, err
			}
			return result.Message, nil
		}
		var invalid *CompletionError
		if !errors.As(last, &invalid) || (invalid.Kind != "empty_completion" && invalid.Kind != "truncated_completion") {
			return Message{}, last
		}
		if attempt == maxCompletionAttempts {
			break
		}
		if err := ctx.Err(); err != nil {
			return Message{}, err
		}
		if onRetry != nil {
			onRetry(attempt, last)
		}
		if invalid.Kind == "truncated_completion" {
			requestMessages = withCompletionRecovery(messages)
		}
	}
	return Message{}, last
}

func withCompletionRecovery(messages []Message) []Message {
	out := append([]Message(nil), messages...)
	const instruction = "\n\nLần sinh trước chưa hoàn tất. Hãy viết lại câu trả lời đầy đủ, ngắn gọn, kết thúc trọn ý trong giới hạn output; không nhắc lỗi kỹ thuật."
	if len(out) > 0 && out[0].Role == "system" {
		if text, ok := out[0].Content.(string); ok {
			out[0].Content = text + instruction
		}
	}
	return out
}
