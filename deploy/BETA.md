# Ani Telegram — beta-20260914-172450

Gói cập nhật thử nghiệm từ source đã sửa ba lỗi phản hồi. Chưa triển khai hay kiểm tra trực tiếp trên VPS.

Đã chỉnh ba đường dẫn service sang `/root/ani-telegram` theo yêu cầu người dùng; không thêm `-log`.
Chỉ cập nhật cấu hình/gói ZIP, binary và SHA-256 dưới đây không đổi.

- Thời điểm build: 2026-09-14T17:24:50+07:00.
- Target: Linux/amd64, GOAMD64=v1, CGO_ENABLED=0; ELF64 tĩnh, không có dynamic linker.
- Toolchain trên máy build: go1.26.5.
- Binary: `ani-telegram`, 16,807,671 byte.
- SHA-256 binary: `E855AF45172B836C972221FD33833E52D33F73EC34EE4C7899CDA187D382360A`.
- Đã chạy mới: `go build ./...`, cross-build Linux, `go test -count=1 ./...`: PASS.
- Test chạy trên Windows với HTTP giả lập/SQLite tạm; chưa chạy binary Linux trên VPS.
- Race detector chưa được xác minh; môi trường hiện tắt CGO.

## Gói này gồm

1. `ani-telegram` — binary mới, thay bản server khi cài beta.
2. `ani-telegram.service` — sửa WorkingDirectory, ExecStart và EnvironmentFile sang `/root/ani-telegram`; không bật `-log`.
3. `BETA.md` — thông tin build này, không cần cài lên VPS.

Đây là gói cập nhật cho VPS đã cài bot, không phải bộ cài lần đầu. Không chứa `.env`, token,
API key, `memory.db`, `offset.txt`, persona hoặc script không đổi. Giữ nguyên các file sống trên VPS.

## Khi cài beta (thực hiện riêng sau khi được yêu cầu)

1. Backup SQLite sống nhất quán bằng SQLite backup API/CLI trước khi chạy binary mới: bản này
   thêm các cột delivery vào `memory_jobs`. Không dùng copy thô khi bot đang ghi.
2. Tắt bot Windows nếu đang chạy để tránh hai tiến trình Telegram getUpdates cùng token.
3. Upload service đã sửa vào `/root/ani-telegram/ani-telegram.service`. Nếu đã upload binary beta
   thì không cần upload lại binary. Chạy `cd /root/ani-telegram && bash restart.sh` để cài lại
   service từ file trong thư mục bot, daemon-reload và restart.
4. Kiểm tra systemd status, journald và `/status`. Tuyệt đối không upload DB/offset từ Windows.

Không có `-log` thì log chẩn đoán chi tiết bị tắt; `StandardOutput=journal` và
`StandardError=journal` vẫn giữ nguyên nhưng không tự bật log ứng dụng.

Rollback: giữ lại binary cũ trước cài beta. Trước khi chạy binary cũ, dùng bản mới phục hồi/
giải phóng hết held delivery jobs rồi dừng bot; binary cũ không biết gate `delivery_pending`.
Không restore DB cũ chỉ để rollback source vì sẽ mất memory mới.

## Phạm vi thay đổi

- Phục hồi output model rỗng hoặc bị cắt (`length`), tối đa 3 phản hồi mỗi bước sinh.
- Gửi Telegram tối đa 5 attempt/đoạn và 2 phút cả lượt; retry đúng đoạn, không replay unknown.
- History/journal chỉ lưu phần đã ACK; status có kết quả gửi, huỷ và lỗi lưu memory riêng.
- Có test hồi quy cho race owner-release journal và lỗi TLS sau-send có thể gây gửi trùng.

Các phát hiện audit khác ngoài ba lỗi delivery không nằm trong bản sửa này.
