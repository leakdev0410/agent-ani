// ani-telegram is a single-owner Telegram bot with SQLite-backed durable memory
// (core state, diary, observations, topics) và auto-recall qua tool-calling.
//
// File này chỉ là entry point mỏng — toàn bộ orchestration, flag/env parsing,
// worker pool và main loop nằm trong package ani-telegram/internal/app.
// Build binary: go build ./cmd/ani-telegram
package main

import "ani-telegram/internal/app"

func main() {
	app.Main()
}
