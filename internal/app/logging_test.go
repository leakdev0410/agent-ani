package app

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"ani-telegram/internal/safelog"
)

func TestOperationalMessageLogsExcludeConversationContentAndSecrets(t *testing.T) {
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})

	userText := "sentinel-user-private-text"
	reply := "sentinel-model-private-reply"
	raw := `{"private":"sentinel-raw-model-json"}`
	fakeToken := "123456789:" + strings.Repeat("Z", 35)
	var out bytes.Buffer
	log.SetOutput(safelog.NewWriter(&out))
	log.SetFlags(0)
	log.SetPrefix("")

	logInboundMessage(7, 22, "message", userText, 1)
	logAssistantMessage(22, 0, reply)
	logAssistantMessage(22, 2, reply)
	logJSONParseFailure(22, raw, errors.New("parse failed at "+fakeToken))
	logReplyRetry(22, fakeToken, 2, raw, 0)
	logReplyOutcome(22, replyOutcome{Kind: reply, ErrorCode: userText, Confirmed: 1, Total: 3, MemoryWarning: true})
	logReplyOutcome(22, replyOutcome{Kind: "failed", ErrorCode: "truncated_completion", HTTPStatus: 403, TelegramCode: 403})
	logDeliveryRetry(2, 3, 2, 429, 429, 0)

	got := out.String()
	for _, private := range []string{userText, reply, raw, "sentinel-raw-model-json", fakeToken} {
		if strings.Contains(got, private) {
			t.Fatalf("operational log còn chứa private value %q trong %q", private, got)
		}
	}
	for _, metadata := range []string{"update_id=7", "text_bytes=", "photos=1", "round=2", "response_bytes=", "finish_reason=length", "http_status=403", "telegram_code=429"} {
		if !strings.Contains(got, metadata) {
			t.Fatalf("operational log thiếu metadata %q trong %q", metadata, got)
		}
	}
}
