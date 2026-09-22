package app

import (
	"testing"

	"ani-telegram/internal/telegram"
)

func TestAuthorizedMessageFromUpdate(t *testing.T) {
	policy := telegramAccessPolicy{allowedUserID: 11, allowedChatID: 22}
	private := func(userID, chatID int64, chatType string) *telegram.Message {
		return &telegram.Message{
			From: &telegram.User{ID: userID},
			Chat: telegram.Chat{ID: chatID, Type: chatType},
			Text: "sentinel-private-message",
		}
	}

	tests := []struct {
		name       string
		update     telegram.Update
		wantKind   string
		wantReason string
		wantAllow  bool
	}{
		{name: "allowed message", update: telegram.Update{Message: private(11, 22, "private")}, wantKind: "message", wantAllow: true},
		{name: "allowed edited message", update: telegram.Update{EditedMessage: private(11, 22, "private")}, wantKind: "edited_message", wantAllow: true},
		{name: "missing message", update: telegram.Update{}, wantReason: "missing_message"},
		{name: "missing sender", update: telegram.Update{Message: &telegram.Message{Chat: telegram.Chat{ID: 22, Type: "private"}}}, wantReason: "missing_sender"},
		{name: "wrong user", update: telegram.Update{Message: private(12, 22, "private")}, wantReason: "wrong_user"},
		{name: "wrong chat", update: telegram.Update{Message: private(11, 23, "private")}, wantReason: "wrong_chat"},
		{name: "group", update: telegram.Update{Message: private(11, 22, "group")}, wantReason: "non_private_chat"},
		{name: "supergroup", update: telegram.Update{Message: private(11, 22, "supergroup")}, wantReason: "non_private_chat"},
		{name: "channel", update: telegram.Update{Message: private(11, 22, "channel")}, wantReason: "non_private_chat"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, kind, decision := authorizedMessageFromUpdate(tt.update, policy)
			if decision.allowed != tt.wantAllow || decision.reason != tt.wantReason {
				t.Fatalf("decision=%+v, want allow=%v reason=%q", decision, tt.wantAllow, tt.wantReason)
			}
			if tt.wantAllow {
				if msg == nil || kind != tt.wantKind {
					t.Fatalf("allowed update trả msg=%v kind=%q, want kind=%q", msg != nil, kind, tt.wantKind)
				}
			} else if msg != nil {
				t.Fatal("denied update không được trả message cho downstream side effects")
			}
		})
	}
}
