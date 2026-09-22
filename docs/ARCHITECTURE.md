# Architecture

ani-telegram is a single-owner Telegram bot that keeps a durable, structured
memory of every conversation and uses it to auto-recall relevant context
on every reply via OpenRouter tool-calling.

## Layout

```
ani-telegram/
├── cmd/
│   └── ani-telegram/main.go       # thin entry point, calls app.Main()
├── internal/
│   ├── app/                       # orchestration: flags, worker pool, main loop
│   │   ├── app.go                 # Main(), chatJob, command dispatch
│   │   ├── proactive.go           # proactive scheduler
│   │   ├── memoryupdate.go        # reply generation + memory journal worker
│   │   ├── delivery*.go           # reply delivery + persistent delivery state
│   │   ├── embedding_indexer.go   # optional semantic index background worker
│   │   ├── runtime_helpers.go     # /status, debug server
│   │   ├── commands.go            # /model, /provider, /help, /proactive
│   │   └── ...
│   ├── persona/                   # default persona loader (Ani)
│   ├── memdb/                     # SQLite schema + storage layer
│   ├── memcore/                   # core_state/diary_entries/observations + migrate/compact
│   ├── memsearch/                 # hybrid FTS5 + embedding retrieval
│   ├── memtopic/                  # topic tagging
│   ├── memdiary/                  # diary compaction
│   ├── memobs/                    # observation extraction
│   ├── openrouter/                # OpenRouter HTTP client + tool-call schema
│   ├── telegram/                  # Telegram Bot API client
│   ├── config/                    # .env loader
│   ├── skills/                    # skills loader
│   ├── limits/                    # size / response-byte guards
│   ├── safelog/                   # secret-redacting log writer
│   └── safeio/                    # bounded IO helpers
├── configs/.env.example           # template — copy to .env
├── scripts/
│   ├── build.sh                   # cross-compile to deploy/ani-telegram
│   ├── restart.sh                 # systemd restart helper
│   └── status.sh                  # status helper
├── deploy/
│   ├── DEPLOY.md                  # Ubuntu 24.04 deployment guide
│   ├── BETA.md                    # beta build notes
│   └── ani-telegram.service       # systemd unit template
├── docs/ARCHITECTURE.md           # this file
├── go.mod / go.sum
├── .env.example / .gitignore
├── LICENSE
├── CHANGELOG.md
└── CONTRIBUTING.md
```

## Runtime data flow

```
Telegram getUpdates ──► cmd/ani-telegram/main
                                │
                                ▼
                       internal/app.Main()
                                │
            ┌───────────────────┼───────────────────┐
            ▼                   ▼                   ▼
   telegram.Client       openrouter.Client    memdb.DB (SQLite)
   (poll + send)         (chat + tools)        (state + journal)
            │                   │                   │
            └─────────► chatJob queue (chan) ◄─────┘
                                │
                                ▼
                       messageWorker goroutine
                                │
                                ▼
              replyWithSkills → LLM with tool calls
                                │
                                ▼
                    finishForegroundReply (delivery journal)
                                │
                                ▼
                       Telegram sendMessage
```

In parallel, two long-running goroutines handle background work:

- `memoryJournalWorkerWithDelivery` — consumes memory-job wake signals,
  re-extracts observations / diary entries / topic tags, persists them, and
  schedules proactive follow-ups.
- `startOptionalEmbeddingIndexer` — when an embedding model is configured,
  rebuilds a bounded semantic index on top of FTS5 to power hybrid recall.

## Memory model

Memory lives entirely in `memory.db` (SQLite, single file). The schema
(`internal/memdb/tables.go`) splits durable state into:

- `core_state` — current persona / mood / current focus (one row, compacted).
- `diary_entries` — recent diary entries (date, body, optional topic).
- `observations` — durable facts about the owner.
- `topics` — tagged notes (searchable).
- `settings` — runtime toggles like `current_model`, `current_provider`.
- `memory_jobs` — durable delivery journal; survives restarts so a reply
  promised to the user is never lost across crashes.
- FTS5 virtual tables mirror the text columns for fast lexical recall.

## Safety boundaries

- `internal/safelog` redacts tokens and chat-id prefixes before any log
  line reaches `stderr` or systemd journal.
- `internal/limits` enforces per-byte caps on Telegram payloads, OpenRouter
  responses, system prompt, model reply, and tool results so a single bad
  request can't blow up memory or trigger rate limits.
- `internal/safeio` wraps every external `ReadFile`/`WriteFile` with the
  configured size caps.
- Single-owner: `ANI_ALLOWED_USER_ID` and `ANI_ALLOWED_CHAT_ID` whitelist
  exactly one Telegram user; every other update is logged and dropped.

## Extension points

- Replace the persona by setting `ANI_PERSONA_PATH` in `.env` — the default
  in `internal/persona/ani.txt` is just a starting point.
- Add a new tool by extending `internal/openrouter/models.go` and the
  tool-dispatch path in `internal/app/memoryupdate.go`.
- Swap memory backend by re-implementing the small surface used by
  `internal/app` (`memdb.DB`); no other package reaches into SQLite directly.
