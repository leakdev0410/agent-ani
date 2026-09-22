# Deploy ani-telegram lên Ubuntu 24.04 (x86_64)

Thư mục `deploy/` chứa systemd unit và tài liệu triển khai. Binary, secret, persona và
memory DB đặt cùng thư mục gốc `/opt/ani-telegram` trên VPS — không cần Go hay monorepo.
Binary build `linux/amd64`, static (CGO tắt), chạy được trên Ubuntu 24.

```
/opt/ani-telegram/         # thư mục gốc trên VPS
├── ani-telegram            # binary Linux x86-64 (copy từ deploy/ sau khi build)
├── .env                    # token + API key + user ID (nhạy cảm)
├── ani.txt                 # persona riêng (tuỳ chọn; ANI_PERSONA_PATH=./ani.txt)
├── memory.db               # trí nhớ mới nhất (giữ nguyên trên VPS, không upload đè)
├── offset.txt              # offset Telegram của VPS (KHÔNG upload từ Windows)
└── ani-telegram.service    # systemd unit (copy từ deploy/)

# script chạy trên máy build (Windows/Linux):
scripts/
├── build.sh                # cross-compile → deploy/ani-telegram
├── restart.sh              # chmod + restart systemd helper
└── status.sh               # kiểm tra service + 100 log gần nhất
```

Không copy `offset.txt` từ máy Windows vào đây — để VPS giữ offset Telegram
của chính nó. Upload đè `offset.txt` dễ làm bot bỏ sót hoặc xử lý lại tin.

⚠️ **`.env` chứa secret thật** — scp qua SSH thì được, đừng zip/upload chỗ
công khai. Tắt bot Windows trước khi start VPS (`taskkill /IM ani-telegram.exe /F`),
Telegram chỉ cho 1 tiến trình poll trên 1 token.

## Server đã có sẵn (upload + restart)

Từ máy build (Windows hoặc Linux), trong thư mục `ani-telegram`:

1. Build binary mới:

    ```bash
    ./scripts/build.sh                # → deploy/ani-telegram
    ```

2. Chỉ upload các file thực sự đổi. Service đã chỉnh khớp nội dung VPS cung cấp
   (không thêm `-log`), nếu VPS vẫn giữ đúng cấu hình thì chỉ cần upload binary.
   Lệnh tối thiểu:

    ```bash
    scp deploy/ani-telegram user@your-vps-ip:/opt/ani-telegram/
    ssh user@your-vps-ip "cd /opt/ani-telegram && sudo bash /opt/ani-telegram/restart.sh"
    ```

   (`restart.sh` nên có sẵn ở `/opt/ani-telegram/` từ lần deploy đầu tiên — copy từ
   `scripts/restart.sh` cùng `deploy/ani-telegram.service` trong lần setup ban đầu.)

`restart.sh` tự `chmod 600 .env`, `chmod +x` binary, cài lại unit file mới nhất,
`daemon-reload`, rồi `systemctl restart`. Không upload `memory.db` hoặc `offset.txt` —
giữ bản sống trên VPS.

`restart.sh` cài service từ file `ani-telegram.service` trong thư mục bot. Nếu file đó khác
cấu hình đang cài trong systemd, đồng bộ cho đúng trước restart; chỉ upload service khi thực sự đổi.
Service hiện không bật `-log`: journald vẫn thu stdout/stderr nhưng log chẩn đoán ứng dụng bị tắt.

Trước lần chạy binary có migration delivery, tạo backup DB nhất quán bằng SQLite backup
API/CLI ngay trên VPS (xem bên dưới). Bản backup source trên Windows không chứa DB sống.

## Delivery recovery và chẩn đoán

- Output model rỗng/cụt: tối đa 3 phản hồi cho mỗi bước sinh. `length` không được gửi như
  câu hoàn chỉnh; tool đã chạy không bị chạy lại. Retry transport OpenRouter mỗi 60 giây
  giữ nguyên, độc lập với ngân sách output-invalid. `ChatJSON` giữ tổng 3 lượt.
- Telegram: tối đa 5 attempt/đoạn, deadline 2 phút cả lượt; cách các đoạn 1 giây. 429 đợi
  `retry_after` (thiếu: 2 giây). DNS/dial chắc chắn chưa gửi dùng backoff 1/2/4/8 giây.
- Timeout/5xx/mất ACK là `unknown`, không tự replay. Không bảo đảm exactly-once; một HTTP
  request đã bay đi có thể tới Telegram dù vừa `/restart` hoặc `/new`.
- Journal held trước-send chưa chứa draft. Chỉ prefix ACK được lưu; FIFO không extraction
  khi sender còn sở hữu job. Job bỏ dở được khôi phục startup/runtime, không gửi lại HTTP.
- `/status` giữ failure/partial/unknown và số đoạn ACK; DB lỗi sau ACK có cảnh báo memory
  riêng, không báo gửi thất bại. `journalctl -u ani-telegram` có metadata khi bật `-log`,
  không ghi nội dung chat hoặc secret.

Rollback binary: trước khi chạy binary cũ, dùng bản mới để giải phóng/recover toàn bộ held
jobs rồi dừng bot. Binary cũ không biết gate `delivery_pending`; chạy khi còn held có thể
trích xuất quá sớm. Không restore DB cũ chỉ để rollback source vì sẽ mất memory mới.

## Lần đầu (chưa có systemd)

```bash
scp -r deploy/ user@your-vps-ip:/opt/ani-telegram
ssh user@your-vps-ip "cd /opt/ani-telegram && sudo bash restart.sh"
```

Kiểm tra: `sudo bash status.sh`. Script in trạng thái service và 100 dòng log gần
nhất từ journald. Muốn theo dõi realtime dùng `sudo journalctl -u ani-telegram -f`.

## Test tay (không qua systemd)

```bash
ssh user@your-vps-ip
cd /opt/ani-telegram
sudo systemctl stop ani-telegram
./ani-telegram -log -logdb
```

## 4. Backup định kỳ trên VPS (khuyến nghị)

`memory.db` là nơi duy nhất giữ trí nhớ. Nếu VPS có sẵn SQLite CLI, dùng `.backup` để tạo
snapshot nhất quán, kể cả khi bot đang ghi. Tạo thư mục backup có quyền truy cập phù hợp
trước; dùng tên snapshot mới mỗi lần, không ghi đè bản đã có. Ví dụ cron:

```bash
# crontab -e — backup mỗi ngày lúc 3h sáng, giữ dạng snapshot có ngày tháng
0 3 * * * sqlite3 /opt/ani-telegram/memory.db ".backup '/opt/ani-telegram/backup/memory-$(date +\%Y\%m\%d-\%H\%M\%S).db'"
```

Không dùng copy file thô khi bot đang ghi làm bản backup trước migration. Nếu thiếu SQLite CLI,
chuẩn bị công cụ backup SQLite phù hợp trước khi deploy; không tự cài hoặc sửa DB trong lượt sửa source.
