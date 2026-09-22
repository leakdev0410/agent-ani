// Package safelog removes known secret shapes before operational text reaches a log sink.
package safelog

import (
	"io"
	"regexp"
)

const redacted = "[REDACTED]"

var (
	telegramTokenPattern = regexp.MustCompile(`[0-9]{6,}:[A-Za-z0-9_-]{20,}\b`)
	openRouterKeyPattern = regexp.MustCompile(`\bsk-or-v1-[A-Za-z0-9_-]{20,}\b`)
	bearerPattern        = regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/-]{6,}=*`)
)

// Redact returns text with Telegram tokens, OpenRouter keys, and bearer credentials removed.
func Redact(text string) string {
	text = telegramTokenPattern.ReplaceAllString(text, redacted)
	text = openRouterKeyPattern.ReplaceAllString(text, redacted)
	return bearerPattern.ReplaceAllString(text, "Bearer "+redacted)
}

type writer struct {
	dst io.Writer
}

// NewWriter wraps a log destination and reports successful writes using the original input
// length, as required by io.Writer even when redaction changes the byte count.
func NewWriter(dst io.Writer) io.Writer {
	return &writer{dst: dst}
}

func (w *writer) Write(p []byte) (int, error) {
	clean := []byte(Redact(string(p)))
	n, err := w.dst.Write(clean)
	if err != nil {
		return 0, err
	}
	if n != len(clean) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}
