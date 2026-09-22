package safelog

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactRemovesKnownSecretShapes(t *testing.T) {
	telegramToken := "123456789:" + strings.Repeat("B", 35)
	openRouterKey := "sk-or-v1-" + strings.Repeat("c", 48)
	input := "telegram=https://api.telegram.org/bot" + telegramToken + "/getUpdates openrouter=" + openRouterKey + " Authorization: Bearer header-secret"

	got := Redact(input)
	for _, secret := range []string{telegramToken, openRouterKey, "header-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("Redact còn chứa secret %q trong %q", secret, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("Redact không để marker: %q", got)
	}
}

func TestWriterRedactsAndReportsOriginalLength(t *testing.T) {
	secret := "sk-or-v1-" + strings.Repeat("d", 48)
	var dst bytes.Buffer
	w := NewWriter(&dst)
	input := []byte("request failed: " + secret)

	n, err := w.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(input) {
		t.Fatalf("Write n=%d, want original len %d", n, len(input))
	}
	if strings.Contains(dst.String(), secret) {
		t.Fatalf("writer làm lộ secret: %q", dst.String())
	}
}
