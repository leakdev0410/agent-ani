#!/usr/bin/env bash
# Chạy trên VPS sau khi upload gói deploy vào /opt/ani-telegram
set -euo pipefail
cd "$(dirname "$0")"

chmod 600 .env
chmod +x ani-telegram
mkdir -p backup

sudo install -m 0644 ani-telegram.service /etc/systemd/system/ani-telegram.service
sudo systemctl daemon-reload
sudo systemctl enable ani-telegram

sudo systemctl restart ani-telegram
sudo systemctl --no-pager --full status ani-telegram
