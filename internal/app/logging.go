package app

import (
	"log"
	"time"
)

func logInboundMessage(updateID, chatID int64, eventKind string, text string, photoCount int) {
	log.Printf("[chat %d] nhận %s: update_id=%d text_bytes=%d photos=%d",
		chatID, eventKind, updateID, len(text), photoCount)
}

func logAssistantMessage(chatID int64, proactiveRound int, text string) {
	if proactiveRound > 0 {
		log.Printf("[chat %d] đã gửi proactive: round=%d text_bytes=%d", chatID, proactiveRound, len(text))
		return
	}
	log.Printf("[chat %d] đã gửi reply: text_bytes=%d", chatID, len(text))
}

func logJSONParseFailure(chatID int64, raw string, err error) {
	log.Printf("[chat %d] auto-update memory: lỗi parse JSON: error_type=%T response_bytes=%d",
		chatID, err, len(raw))
}

func logReplyRetry(chatID int64, phase string, attempt int, code string, delay time.Duration) {
	if phase != "model" && phase != "delivery" {
		phase = "other"
	}
	log.Printf("[chat %d] reply retry: phase=%s attempt=%d error_code=%s finish_reason=%s delay=%s", chatID, phase, attempt, normalizedReplyError(code), completionFinishReason(code), delay)
}

func logReplyOutcome(chatID int64, outcome replyOutcome) {
	log.Printf("[chat %d] reply outcome=%s error_code=%s finish_reason=%s http_status=%d telegram_code=%d confirmed=%d total=%d memory_warning=%t", chatID, normalizedOutcome(outcome.Kind), normalizedReplyError(outcome.ErrorCode), completionFinishReason(outcome.ErrorCode), outcome.HTTPStatus, outcome.TelegramCode, outcome.Confirmed, outcome.Total, outcome.MemoryWarning)
}

func logDeliveryRetry(sequence, total, attempt, httpStatus, code int, delay time.Duration) {
	log.Printf("reply retry: phase=delivery sequence=%d total=%d attempt=%d http_status=%d telegram_code=%d delay=%s", sequence, total, attempt, httpStatus, code, delay)
}

func completionFinishReason(code string) string {
	switch code {
	case "truncated_completion":
		return "length"
	case "filtered_completion":
		return "content_filter"
	case "error_completion":
		return "error"
	case "unknown_finish_reason":
		return "unknown"
	default:
		return ""
	}
}
