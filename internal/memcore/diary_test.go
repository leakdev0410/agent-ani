package memcore

import (
	"strings"
	"testing"
)

// TestParseDiary_Roundtrip_Synthetic dùng fixture nhỏ tự dựng, kiểm tra InsertDiaryAfterMarker +
// RenderDiaryDays dựng lại đúng cấu trúc khi ghép ngược vào rest.
func TestParseDiary_Roundtrip_Synthetic(t *testing.T) {
	fixture := `# Trí nhớ về anh

4. **Dấu ấn cảm xúc em nhớ về anh** (phân tầng theo ngày):

**2026-08-11**:
- 10:00: sự kiện A
- 11:00: sự kiện B

**2026-08-10**:
- 09:00: sự kiện C

## Mục tiếp theo
nội dung khác`

	days, rest, err := ParseDiary(fixture)
	if err != nil {
		t.Fatalf("ParseDiary: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("mong đợi 2 ngày, được %d", len(days))
	}
	if days[0].Date != "2026-08-11" || days[1].Date != "2026-08-10" {
		t.Errorf("thứ tự/ngày sai: %+v", days)
	}
	if !strings.Contains(days[0].Body, "sự kiện A") || !strings.Contains(days[0].Body, "sự kiện B") {
		t.Errorf("mất nội dung ngày 2026-08-11: %q", days[0].Body)
	}
	if !strings.Contains(days[1].Body, "sự kiện C") {
		t.Errorf("mất nội dung ngày 2026-08-10: %q", days[1].Body)
	}
	if !strings.Contains(rest, "## Mục tiếp theo") || !strings.Contains(rest, "nội dung khác") {
		t.Errorf("rest bị mất nội dung sau khối diary: %q", rest)
	}
	if strings.Contains(rest, "sự kiện A") {
		t.Errorf("rest vẫn còn lẫn nội dung diary, chưa tách sạch")
	}

	recent := RenderDiaryDays(days[:1])
	spliced := InsertDiaryAfterMarker(rest, recent)
	if !strings.Contains(spliced, "**2026-08-11**:") || !strings.Contains(spliced, "sự kiện A") {
		t.Errorf("chèn lại diary vào rest thất bại: %q", spliced)
	}
	if !strings.Contains(spliced, "## Mục tiếp theo") {
		t.Errorf("mất nội dung sau diary sau khi chèn lại: %q", spliced)
	}
}
