# ani-telegram

Bot Telegram cho Ani (Golang, model qua OpenRouter — xem README.md cho kiến trúc/schema đầy đủ).

## Khi được yêu cầu "build lại deploy" / "build bản deploy"

Luôn làm theo đúng trình tự này, không hỏi lại trừ khi thiếu thông tin VPS (user/IP).

### 1. Build

```bash
go build ./...                                              # kiểm tra compile sạch trước
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o deploy/ani-telegram ./cmd/ani-telegram   # binary Ubuntu, static (CGO tắt vì dùng modernc.org/sqlite — pure Go, không cần cgo)
```

Hoặc dùng script wrapper:

```bash
./scripts/build.sh
```

Ghi đè `deploy/ani-telegram` (binary linux/amd64) — đây là bước bắt buộc mỗi lần "build lại deploy".

### 2. File nào cần re-upload lên VPS — chỉ upload cái THẬT SỰ đổi

| File trong `deploy/` | Khi nào re-upload |
|---|---|
| `ani-telegram` (binary) | LUÔN LUÔN, sau mỗi lần build lại |
| `.env` | Chỉ khi đổi secret/config (bot token, `OPENROUTER_API_KEY`, `ANI_ALLOWED_USER_ID`, `ANI_ALLOWED_CHAT_ID`...) |
| `ani.txt` (persona) | Chỉ khi đổi persona |
| `ani-telegram.service` | Chỉ khi đổi cấu hình systemd (ExecStart, flags...) |
| `restart.sh` | Chỉ khi đổi script |
| `memory.db` | **KHÔNG** re-upload định kỳ — VPS giữ bản sống riêng, ghi đè sẽ mất trí nhớ mới trên VPS. Chỉ dùng cho lần deploy đầu tiên hoặc khi cố ý restore từ backup |
| `offset.txt` | **KHÔNG BAO GIỜ** upload từ Windows — để VPS tự giữ offset Telegram của nó, upload đè dễ làm bot bỏ sót/xử lý lại tin |

### 3. Lệnh upload + reset server

```bash
# đổi user@VPS_IP cho đúng — thêm file khác vào lệnh scp nếu file đó cũng đổi (xem bảng trên)
scp deploy/ani-telegram user@VPS_IP:/opt/ani-telegram/
ssh user@VPS_IP "cd /opt/ani-telegram && sudo bash restart.sh"
```

`restart.sh` tự làm: `chmod 600 .env`, `chmod +x ani-telegram`, cài `ani-telegram.service` vào systemd nếu chưa có (`daemon-reload` + `enable`), rồi `systemctl restart ani-telegram` + in status.

**Quan trọng**: tắt bot Windows trước khi restart VPS — `taskkill /IM ani-telegram.exe /F` — Telegram chỉ cho 1 tiến trình `getUpdates` poll cùng lúc trên 1 bot token, chạy song song 2 bên sẽ dính lỗi `Conflict: terminated by other getUpdates request`.

### 4. Kiểm tra sau deploy

```bash
ssh user@VPS_IP "sudo systemctl status ani-telegram"
ssh user@VPS_IP "sudo journalctl -u ani-telegram -f"    # Ctrl+C để thoát; operational metadata đã redact
```

### Reset thủ công (không qua restart.sh) khi cần

```bash
sudo systemctl stop ani-telegram
sudo systemctl start ani-telegram      # hoặc: sudo systemctl restart ani-telegram
sudo systemctl status ani-telegram
journalctl -u ani-telegram -f          # log real-time qua systemd; không chứa nội dung chat mặc định
journalctl --disk-usage                # kiểm tra dung lượng journald; retention cấu hình ở host
```

Chi tiết đầy đủ (lần đầu chưa có systemd, test tay không qua systemd, backup định kỳ...): xem `deploy/DEPLOY.md`.
