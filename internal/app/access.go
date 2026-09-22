package app

import "ani-telegram/internal/telegram"

type telegramAccessPolicy struct {
	allowedUserID int64
	allowedChatID int64
}

type telegramAuthorization struct {
	allowed bool
	reason  string
}

// authorizedMessageFromUpdate is the single inbound authorization boundary. A denied update
// intentionally returns a nil message so downstream code cannot accidentally use its content.
func authorizedMessageFromUpdate(upd telegram.Update, policy telegramAccessPolicy) (*telegram.Message, string, telegramAuthorization) {
	msg := upd.Message
	eventKind := "message"
	if msg == nil {
		msg = upd.EditedMessage
		eventKind = "edited_message"
	}
	if msg == nil {
		return nil, "", telegramAuthorization{reason: "missing_message"}
	}
	if msg.From == nil {
		return nil, "", telegramAuthorization{reason: "missing_sender"}
	}
	if msg.From.ID != policy.allowedUserID {
		return nil, "", telegramAuthorization{reason: "wrong_user"}
	}
	if msg.Chat.ID != policy.allowedChatID {
		return nil, "", telegramAuthorization{reason: "wrong_chat"}
	}
	if msg.Chat.Type != "private" {
		return nil, "", telegramAuthorization{reason: "non_private_chat"}
	}
	return msg, eventKind, telegramAuthorization{allowed: true}
}
