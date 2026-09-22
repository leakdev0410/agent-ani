package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ani-telegram/internal/openrouter"
)

type replyOutcome struct {
	Kind, ErrorCode          string
	Confirmed, Total         int
	HTTPStatus, TelegramCode int
	MemoryWarning            bool
	At                       time.Time
}

func (s *botStatus) SetOutcome(outcome replyOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setOutcomeLocked(outcome)
}

func (s *botStatus) setOutcomeLocked(outcome replyOutcome) {
	outcome.Kind = normalizedOutcome(outcome.Kind)
	outcome.ErrorCode = normalizedReplyError(outcome.ErrorCode)
	if outcome.At.IsZero() {
		outcome.At = time.Now()
	}
	s.lastOutcome = outcome
}

func (s *botStatus) SetOutcomeContext(ctx context.Context, outcome replyOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentContext == ctx {
		s.setOutcomeLocked(outcome)
	}
}

func (s *botStatus) LastOutcome() replyOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOutcome
}

func (s *botStatus) SetActionContext(ctx context.Context, action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy && s.currentContext == ctx {
		s.action = action
	}
}

func (s *botStatus) DoneContext(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentContext != ctx {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.busy, s.action, s.cancel, s.currentChatID, s.currentContext = false, "", nil, 0, nil
}

func (s *botStatus) deliveryCoordinator() *deliveryCoordinator {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deliveries == nil {
		s.deliveries = &deliveryCoordinator{}
	}
	return s.deliveries
}

func normalizedOutcome(kind string) string {
	switch kind {
	case "", "sent", "failed", "partial", "unknown", "cancelled", "interrupted", "model_failed":
		return kind
	}
	return "failed"
}

func normalizedReplyError(code string) string {
	switch code {
	case "", "empty_completion", "truncated_completion", "filtered_completion", "error_completion", "unknown_finish_reason", "invalid_tool_completion", "model_error", "rejected", "rate_limited", "not_sent", "unknown", "cancelled", "journal_error", "empty_reply", "deadline":
		return code
	}
	return "model_error"
}

func describeReplyOutcome(o replyOutcome) string {
	var line string
	switch o.Kind {
	case "unknown":
		line = fmt.Sprintf("⚠️ Chưa xác nhận gửi đoạn %d (%d/%d đoạn đã nhận).", o.Confirmed+1, o.Confirmed, o.Total)
	case "partial":
		line = fmt.Sprintf("⚠️ Lượt trước gửi được %d/%d đoạn.", o.Confirmed, o.Total)
	case "failed", "cancelled", "interrupted":
		line = fmt.Sprintf("⚠️ Lượt trước chưa gửi được đầy đủ (%d/%d đoạn).", o.Confirmed, o.Total)
	case "model_failed":
		line = "⚠️ Lượt trước model chưa tạo được câu trả lời đầy đủ."
	}
	if o.ErrorCode != "" && line != "" {
		line += " Mã: " + normalizedReplyError(o.ErrorCode) + "."
	}
	if o.TelegramCode != 0 && line != "" {
		line += fmt.Sprintf(" Telegram: %d.", o.TelegramCode)
	}
	if o.MemoryWarning {
		if line != "" {
			line += "\n"
		}
		if o.Total > 0 && o.Confirmed == o.Total {
			line += "⚠️ Đã gửi; lưu memory đang lỗi."
		} else {
			line += "⚠️ Lưu memory đang lỗi."
		}
	}
	return line
}

func (h *historyStore) SnapshotTurn(chatID int64) ([]openrouter.Message, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return cloneHistory(h.data[chatID]), h.generation[chatID]
}

func (h *historyStore) IsGeneration(chatID int64, generation uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.generation[chatID] == generation
}

func (h *historyStore) AppendDelivered(chatID int64, generation uint64, userText, confirmedText string, proactive bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.generation[chatID] != generation {
		return false
	}
	msgs := appendDeliveredHistory(h.data[chatID], userText, confirmedText, proactive)
	if len(msgs) > maxHistoryTurns*2 {
		msgs = msgs[len(msgs)-maxHistoryTurns*2:]
	}
	h.data[chatID] = msgs
	return true
}

func cloneHistory(messages []openrouter.Message) []openrouter.Message {
	cloned := append([]openrouter.Message(nil), messages...)
	for i := range cloned {
		cloned[i].ToolCalls = append([]openrouter.ToolCall(nil), cloned[i].ToolCalls...)
	}
	return cloned
}

func appendDeliveredHistory(messages []openrouter.Message, userText, confirmedText string, proactive bool) []openrouter.Message {
	msgs := cloneHistory(messages)
	if !proactive && strings.TrimSpace(userText) != "" {
		msgs = append(msgs, openrouter.Message{Role: "user", Content: userText})
	}
	if strings.TrimSpace(confirmedText) != "" {
		if n := len(msgs); n > 0 && msgs[n-1].Role == "assistant" {
			msgs[n-1].Content = messageText(msgs[n-1]) + "\n" + confirmedText
		} else {
			msgs = append(msgs, openrouter.Message{Role: "assistant", Content: confirmedText})
		}
	}
	return msgs
}
