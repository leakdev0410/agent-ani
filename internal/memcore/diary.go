package memcore

import (
	"fmt"
	"strings"
)

const (
	// RecentDiaryDays là số ngày "Dấu ấn cảm xúc" gần nhất giữ inline trong system prompt.
	RecentDiaryDays = 2
	// RecentDiaryBulletsPerDay giới hạn số sự kiện mới nhất của mỗi ngày gần hiện inline.
	// Ngày dày (cả chục tin) không bị mất — phần sớm hơn lấy lại bằng recall_memory(date).
	RecentDiaryBulletsPerDay = 20
	// ObservationHead / ObservationTail: quan sát nền tảng (cũ nhất) + gần đây nhất.
	// Phần giữa lấy lại bằng recall_observations(kind).
	ObservationHead = 5
	ObservationTail = 8
)

// DiaryDay là toàn bộ nội dung 1 ngày trong khối "Dấu ấn cảm xúc", giữ NGUYÊN VẸN text gốc
// (Body là các dòng gốc nối lại, không diễn giải lại) — đảm bảo không mất chữ nào dù ngày đó
// được hiển thị inline hay chỉ lấy qua recall_memory.
type DiaryDay struct {
	Date string // "YYYY-MM-DD"
	Body string // các dòng bullet gốc của ngày đó, nối bằng "\n", đã trim khoảng trắng thừa
}

// ParseDiary tách khối "Dấu ấn cảm xúc ... (phân tầng theo ngày):" ra khỏi nội dung memory.md.
// Trả về:
//   - days: từng ngày theo đúng thứ tự trong file (mới nhất trước, đúng quy ước "ghi mới lên trên")
//   - rest: toàn bộ memory.md SAU KHI bỏ các khối ngày (dòng marker vẫn giữ nguyên, phần dưới nối liền)
//
// Không tìm thấy marker thì trả về days=nil, rest=nguyên văn input (fixture/test không có khối
// diary vẫn hoạt động bình thường, không coi là lỗi).
func ParseDiary(memoryMD string) (days []DiaryDay, rest string, err error) {
	lines := strings.Split(memoryMD, "\n")

	markerIdx := -1
	for i, l := range lines {
		if strings.Contains(l, diaryMarker) {
			markerIdx = i
			break
		}
	}
	if markerIdx == -1 {
		return nil, memoryMD, nil
	}

	blockEnd := len(lines)
	for i := markerIdx + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			blockEnd = i
			break
		}
	}

	var current *DiaryDay
	var bodyLines []string
	flush := func() {
		if current != nil {
			current.Body = strings.TrimRight(strings.Join(bodyLines, "\n"), "\n ")
			days = append(days, *current)
		}
	}

	for i := markerIdx + 1; i < blockEnd; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if date, ok := parseDiaryDateHeading(trimmed); ok {
			flush()
			current = &DiaryDay{Date: date}
			bodyLines = nil
			continue
		}
		if current == nil {
			continue // khoảng trắng trước ngày đầu tiên — chỉ để canh dòng, không phải nội dung
		}
		bodyLines = append(bodyLines, lines[i])
	}
	flush()

	restLines := make([]string, 0, markerIdx+1+(len(lines)-blockEnd))
	restLines = append(restLines, lines[:markerIdx+1]...)
	restLines = append(restLines, lines[blockEnd:]...)
	rest = strings.Join(restLines, "\n")

	return days, rest, nil
}

// parseDiaryDateHeading nhận diện đúng dòng "**YYYY-MM-DD**:" (heading 1 ngày trong khối diary).
func parseDiaryDateHeading(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "**") || !strings.HasSuffix(trimmed, "**:") {
		return "", false
	}
	date := strings.TrimSuffix(strings.TrimPrefix(trimmed, "**"), "**:")
	if len(date) != len("2006-01-02") {
		return "", false
	}
	return date, true
}

// RenderDiaryDays dựng lại đúng định dạng "**YYYY-MM-DD**:\n<bullets>" cho từng ngày trong days,
// nối các ngày bằng 1 dòng trống — dùng để chèn ngày gần đây (RecentDiaryDays) vào system prompt.
func RenderDiaryDays(days []DiaryDay) string {
	var b strings.Builder
	for i, d := range days {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "**%s**:\n%s", d.Date, d.Body)
	}
	return b.String()
}

// SplitDiaryBullets tách Body (nhiều dòng "- HH:MM: nội dung") thành từng bullet riêng, đã bỏ
// tiền tố "- " — dùng khi migrate 1 DiaryDay sang các row riêng trong bảng diary_entries. Bỏ qua
// dòng trắng hoặc dòng không bắt đầu bằng "- " (không nên có trong Body, nhưng phòng hờ).
func SplitDiaryBullets(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		out = append(out, strings.TrimPrefix(trimmed, "- "))
	}
	return out
}

// RenderDiaryBullets dựng lại các dòng "- text" từ danh sách text đã lưu trong diary_entries
// (bảng SQL) cho 1 ngày cụ thể.
func RenderDiaryBullets(texts []string) string {
	lines := make([]string, len(texts))
	for i, t := range texts {
		lines[i] = "- " + t
	}
	return strings.Join(lines, "\n")
}

// InsertDiaryAfterMarker chèn diaryText vào ngay sau dòng chứa diaryMarker trong rest (kết quả
// của ParseDiary) — dùng khi dựng lại system prompt sau khi đã tách khối diary ra riêng.
func InsertDiaryAfterMarker(rest, diaryText string) string {
	lines := strings.Split(rest, "\n")
	for i, l := range lines {
		if !strings.Contains(l, diaryMarker) {
			continue
		}
		result := make([]string, 0, len(lines)+3)
		result = append(result, lines[:i+1]...)
		result = append(result, "", diaryText)
		result = append(result, lines[i+1:]...)
		return strings.Join(result, "\n")
	}
	return rest
}
