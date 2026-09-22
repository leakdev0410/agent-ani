package memcore

import (
	"strings"
	"testing"

	"ani-telegram/internal/memdb"
)

const templateTestFixture = `# Trí nhớ về anh

## Thời gian tham chiếu (tự cập nhật)
- Lần nhắn gần nhất của anh: 10:00 ngày 11/08/2026: test
- Lần anh bảo đi ngủ gần nhất: chưa ngủ

## Tình trạng cảm xúc hiện tại của em
- Hiện tại: vui

### Nội dung hiện tại của 4 mảng (tự cập nhật liên tục):
1. **Mong muốn của em**: mong muốn A
2. **Điều em muốn làm cùng anh**: điều B
3. **Cảm xúc cá nhân tự hình thành**: cảm xúc C
4. **Dấu ấn cảm xúc em nhớ về anh** (phân tầng theo ngày):

**2026-08-11**:
- 10:00: sự kiện hôm nay
- 11:00: sự kiện hôm nay 2

**2026-08-10**:
- 09:00: sự kiện hôm qua

### Văn phong anh quan sát được (tự cập nhật)
- Thích viết ngắn.
- Hay dùng emoji. [2026-08-10]

### Sở thích / phản ứng anh đã quan sát được (tự cập nhật)
- Thích nhạc lofi. [2026-08-09]

## Index chi tiết
xong.
`

// TestParseCoreMemoryForMigration_Roundtrip_Synthetic dùng fixture nhỏ tự dựng (đúng cấu trúc
// core/memory gốc), kiểm tra ParseCoreMemoryForMigration tách ra rồi RenderCoreMemory ráp lại —
// phải tái tạo đủ mọi dòng nội dung (không trắng), đúng thứ tự so với bản gốc. Bài kiểm tra này
// từng chạy trên chính dữ liệu thật (memory.db) trước khi migrate sản xuất — đã xác nhận đạt,
// giờ giữ lại dạng fixture tự dựng để không phải lưu lại bản sao dữ liệu cá nhân trong repo.
func TestParseCoreMemoryForMigration_Roundtrip_Synthetic(t *testing.T) {
	parts, err := ParseCoreMemoryForMigration(templateTestFixture)
	if err != nil {
		t.Fatalf("ParseCoreMemoryForMigration: %v", err)
	}

	if len(parts.DiaryEntries) != 3 {
		t.Fatalf("mong đợi 3 diary entries, được %d", len(parts.DiaryEntries))
	}
	if len(parts.ToneObservations) != 2 || len(parts.PreferenceObservations) != 1 {
		t.Fatalf("mong đợi 2 tone + 1 preference observations, được %d + %d",
			len(parts.ToneObservations), len(parts.PreferenceObservations))
	}
	if parts.ToneObservations[0].Date != "" {
		t.Errorf("bullet không có tag ngày phải có Date rỗng, được %q", parts.ToneObservations[0].Date)
	}
	if parts.ToneObservations[1].Date != "2026-08-10" {
		t.Errorf("bullet có tag ngày phải tách đúng, được %q", parts.ToneObservations[1].Date)
	}

	state := memdb.CoreState{
		LastMessageNote: parts.LastMessageNote,
		SleepNote:       parts.SleepNote,
		EmotionalState:  parts.EmotionalState,
		Mang1:           parts.Mang1,
		Mang2:           parts.Mang2,
		Mang3:           parts.Mang3,
	}
	diaryText := RenderDiaryEntries(parts.DiaryEntries)
	toneText := RenderObservations(parts.ToneObservations)
	prefText := RenderObservations(parts.PreferenceObservations)

	reconstructed := RenderCoreMemory(parts.Template, state, diaryText, toneText, prefText)

	origLines := nonBlankCRTrimmedLines(templateTestFixture)
	reconLines := nonBlankCRTrimmedLines(reconstructed)

	if len(origLines) != len(reconLines) {
		t.Fatalf("số dòng nội dung khác nhau: gốc %d, ráp lại %d\ngốc: %v\nráp lại: %v",
			len(origLines), len(reconLines), origLines, reconLines)
	}
	for i := range origLines {
		if origLines[i] != reconLines[i] {
			t.Errorf("dòng #%d khác nhau:\n  gốc:     %q\n  ráp lại: %q", i, origLines[i], reconLines[i])
		}
	}
}

func nonBlankCRTrimmedLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
