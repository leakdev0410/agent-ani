#!/usr/bin/env bash
# Kiểm tra nhanh service Ani và các log gần nhất từ journald.
set -euo pipefail

status=0
sudo systemctl --no-pager --full status ani-telegram || status=$?
echo
sudo journalctl -u ani-telegram -n 100 --no-pager
exit "$status"
