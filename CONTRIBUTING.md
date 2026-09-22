# Contributing

Thanks for your interest in contributing to ani-telegram.

## Reporting issues

- Search existing issues first to avoid duplicates.
- Include the Go version (`go version`), OS, and the exact steps to reproduce.
- For bugs, attach the relevant logs (`journalctl -u ani-telegram -n 200` if
  you run via systemd, or the in-app log with `-log` enabled).
- For security issues, please email the maintainer privately rather than
  opening a public issue.

## Development setup

Requirements:

- Go ≥ 1.22 (the project uses `modernc.org/sqlite` — pure Go, no cgo).
- A Telegram bot token (talk to [@BotFather](https://t.me/BotFather)).
- An OpenRouter API key.

Clone, configure, and run:

```bash
git clone https://github.com/<your-org>/ani-telegram
cd ani-telegram

cp configs/.env.example .env
# edit .env — set ANI_TELEGRAM_BOT_TOKEN, OPENROUTER_API_KEY, ANI_ALLOWED_CHAT_ID

go build ./...
go test ./...

# run locally with verbose log
./ani-telegram -log
```

## Code style

- Go standard formatting (`gofmt -s -w .`, `goimports`).
- Run `go vet ./...` before pushing — must report zero issues.
- Keep packages focused. `internal/app/` is orchestration; deeper packages
  (`internal/memdb`, `internal/openrouter`, `internal/telegram`, ...) hold
  one responsibility each.
- Add or update unit tests alongside any non-trivial change.
- Vietnamese is welcome in user-facing strings (this bot is intentionally
  Vietnamese-first); code identifiers and comments should remain in English
  unless quoting a user-facing message.

## Pull requests

1. Branch off `main`.
2. Commit messages in English, present tense, imperative mood
   ("Add X", not "Added X"). Reference any related issue.
3. Push the branch and open a PR against `main`.
4. PR description should explain the *why* — behaviour changes, edge cases,
   test plan, and any breaking changes.
5. Expect review feedback. Force-pushes during review are fine; squash at
   merge time.

## Project structure

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the layout,
package responsibilities, and data flow.

## License

By contributing, you agree that your contributions will be licensed under the
[MIT License](LICENSE).
