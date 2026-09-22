#!/usr/bin/env bash
# Cross-compile the bot for Ubuntu 24.04 (linux/amd64, static).
#
# Output: deploy/ani-telegram  (gitignored — binary is rebuilt every deploy)
#
# Requires Go on PATH. Pure Go (modernc.org/sqlite), no cgo needed.

set -euo pipefail

cd "$(dirname "$0")/.."

mkdir -p deploy

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags='-s -w' \
    -o deploy/ani-telegram \
    ./cmd/ani-telegram

echo "Built deploy/ani-telegram ($(stat -c%s deploy/ani-telegram 2>/dev/null || stat -f%z deploy/ani-telegram) bytes)"
