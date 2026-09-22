// Phần "định dạng bảng thật": core/memory không còn là 1 blob text chứa hết mọi thứ — chỉ còn
// giữ lại phần văn xuôi TĨNH (quy tắc, hướng dẫn viết tay, hiếm khi đổi) dưới dạng template có
// {{PLACEHOLDER}}; phần "sống" (last_message_note, mang1-3, diary, quan sát văn phong/sở thích)
// chuyển hẳn sang bảng SQL (core_state, diary_entries, observations trong memdb) — dễ query,
// dễ cập nhật bằng UPDATE/INSERT thay vì dò/thay dòng trong text.
package memcore

import (
	"fmt"
	"regexp"
	"strings"

	"ani-telegram/internal/memdb"
)

// Các placeholder trong template — persona.go điền giá trị thật vào khi dựng system prompt.
const (
	PlaceholderLastMessageNote        = "{{LAST_MESSAGE_NOTE}}"
	PlaceholderSleepNote              = "{{SLEEP_NOTE}}"
	PlaceholderEmotionalState         = "{{EMOTIONAL_STATE}}"
	PlaceholderMang1                  = "{{MANG1}}"
	PlaceholderMang2                  = "{{MANG2}}"
	PlaceholderMang3                  = "{{MANG3}}"
	PlaceholderDiary                  = "{{DIARY}}"
	PlaceholderToneObservations       = "{{TONE_OBSERVATIONS}}"
	PlaceholderPreferenceObservations = "{{PREFERENCE_OBSERVATIONS}}"
)

const (
	toneObservationsHeading       = "### Văn phong anh quan sát được (tự cập nhật)"
	preferenceObservationsHeading = "### Sở thích / phản ứng anh đã quan sát được (tự cập nhật)"
)

// Observation là 1 ghi nhận văn phong/sở thích đã tách ra từ core/memory cũ — Date rỗng nếu
// bullet gốc không có tag "[YYYY-MM-DD]" (một số bullet cũ hơn tính năng auto-observation).
type Observation struct {
	Text string
	Date string
}

var observationDateSuffix = regexp.MustCompile(`\s*\[(\d{4}-\d{2}-\d{2})\]\s*$`)

// DiaryEntryRecord là 1 bullet nhật ký đã tách rời khỏi khối theo-ngày — sẵn sàng INSERT thành
// 1 row trong bảng diary_entries.
type DiaryEntryRecord struct {
	Date string
	Text string // "HH:MM: nội dung", không có "- " ở đầu
}

// CoreMemoryParts là kết quả tách core/memory cũ (1 blob text) thành template + dữ liệu có cấu
// trúc — dùng đúng 1 lần khi migrate sang bảng SQL.
type CoreMemoryParts struct {
	Template               string
	LastMessageNote        string
	SleepNote              string
	EmotionalState         string
	Mang1                  string
	Mang2                  string
	Mang3                  string
	DiaryEntries           []DiaryEntryRecord
	ToneObservations       []Observation
	PreferenceObservations []Observation
}

// ParseCoreMemoryForMigration tách core/memory cũ thành CoreMemoryParts — dùng 1 lần khi
// chuyển từ "1 blob text" sang "template + bảng SQL". Trả lỗi rõ ràng nếu không tìm thấy 1 mốc
// cấu trúc nào đó, thay vì âm thầm bỏ qua (an toàn hơn cho dữ liệu quý — dừng lại để người dùng
// biết, không đoán mò).
func ParseCoreMemoryForMigration(raw string) (CoreMemoryParts, error) {
	var p CoreMemoryParts
	lines := strings.Split(raw, "\n")

	var ok bool
	p.LastMessageNote, lines, ok = extractPrefixValue(lines, lastMessagePrefix, PlaceholderLastMessageNote)
	if !ok {
		return p, fmt.Errorf("không tìm thấy dòng %q", lastMessagePrefix)
	}
	p.SleepNote, lines, ok = extractPrefixValue(lines, sleepPrefix, PlaceholderSleepNote)
	if !ok {
		return p, fmt.Errorf("không tìm thấy dòng %q", sleepPrefix)
	}
	p.EmotionalState, lines, ok = extractPrefixValue(lines, emotionalPrefix, PlaceholderEmotionalState)
	if !ok {
		return p, fmt.Errorf("không tìm thấy dòng %q", emotionalPrefix)
	}
	p.Mang1, lines, ok = extractPrefixValue(lines, mang1Prefix, PlaceholderMang1)
	if !ok {
		return p, fmt.Errorf("không tìm thấy dòng %q", mang1Prefix)
	}
	p.Mang2, lines, ok = extractPrefixValue(lines, mang2Prefix, PlaceholderMang2)
	if !ok {
		return p, fmt.Errorf("không tìm thấy dòng %q", mang2Prefix)
	}
	p.Mang3, lines, ok = extractPrefixValue(lines, mang3Prefix, PlaceholderMang3)
	if !ok {
		return p, fmt.Errorf("không tìm thấy dòng %q", mang3Prefix)
	}

	working := strings.Join(lines, "\n")
	days, rest, err := ParseDiary(working)
	if err != nil {
		return p, fmt.Errorf("ParseDiary: %w", err)
	}
	if len(days) == 0 {
		return p, fmt.Errorf("không tách được ngày nào trong khối Dấu ấn cảm xúc")
	}
	for _, d := range days {
		for _, bullet := range SplitDiaryBullets(d.Body) {
			p.DiaryEntries = append(p.DiaryEntries, DiaryEntryRecord{Date: d.Date, Text: bullet})
		}
	}
	if len(p.DiaryEntries) == 0 {
		return p, fmt.Errorf("tách được ngày nhưng không có bullet nào trong khối Dấu ấn cảm xúc")
	}
	rest = InsertDiaryAfterMarker(rest, PlaceholderDiary)

	lines = strings.Split(rest, "\n")
	p.ToneObservations, lines, err = extractObservations(lines, toneObservationsHeading, PlaceholderToneObservations)
	if err != nil {
		return p, err
	}
	p.PreferenceObservations, lines, err = extractObservations(lines, preferenceObservationsHeading, PlaceholderPreferenceObservations)
	if err != nil {
		return p, err
	}

	p.Template = strings.Join(lines, "\n")
	return p, nil
}

// extractPrefixValue tìm dòng đầu tiên bắt đầu bằng prefix, lấy phần giá trị sau prefix, thay
// dòng đó bằng "prefix placeholder" trong kết quả trả về.
func extractPrefixValue(lines []string, prefix, placeholder string) (value string, result []string, found bool) {
	result = make([]string, len(lines))
	copy(result, lines)
	for i, l := range result {
		trimmed := strings.TrimSpace(l)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		value = strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		result[i] = prefix + " " + placeholder
		return value, result, true
	}
	return "", lines, false
}

// extractObservations tìm heading, thu hết bullet "- ..." tới heading tiếp theo (# bất kỳ cấp),
// tách text + tag ngày [YYYY-MM-DD] nếu có, thay cả khối bullet bằng 1 dòng placeholder.
func extractObservations(lines []string, heading, placeholder string) ([]Observation, []string, error) {
	headingIdx := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			headingIdx = i
			break
		}
	}
	if headingIdx == -1 {
		return nil, lines, fmt.Errorf("không tìm thấy heading %q", heading)
	}

	boundary := len(lines)
	for i := headingIdx + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			boundary = i
			break
		}
	}

	var obs []Observation
	for i := headingIdx + 1; i < boundary; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		text := strings.TrimPrefix(trimmed, "- ")
		date := ""
		if m := observationDateSuffix.FindStringSubmatch(text); m != nil {
			date = m[1]
			text = strings.TrimSpace(observationDateSuffix.ReplaceAllString(text, ""))
		}
		obs = append(obs, Observation{Text: text, Date: date})
	}
	if len(obs) == 0 {
		return nil, lines, fmt.Errorf("không tìm thấy bullet nào dưới heading %q", heading)
	}

	result := make([]string, 0, headingIdx+2+(len(lines)-boundary))
	result = append(result, lines[:headingIdx+1]...)
	result = append(result, placeholder)
	result = append(result, lines[boundary:]...)

	return obs, result, nil
}

// RenderObservations dựng lại đúng định dạng bullet gốc ("- text [date]" hoặc "- text" nếu
// không có ngày) từ danh sách Observation, mỗi bullet 1 dòng.
func RenderObservations(obs []Observation) string {
	lines := make([]string, len(obs))
	for i, o := range obs {
		if o.Date != "" {
			lines[i] = fmt.Sprintf("- %s [%s]", o.Text, o.Date)
		} else {
			lines[i] = "- " + o.Text
		}
	}
	return strings.Join(lines, "\n")
}

// RenderDiaryEntries dựng lại đúng định dạng "**YYYY-MM-DD**:\n- bullet..." từ danh sách
// DiaryEntryRecord phẳng (giả định các entry cùng ngày nằm liền nhau, đúng thứ tự gốc trong
// file / đúng thứ tự INSERT) — dùng để đối chiếu khi kiểm tra round-trip migration.
func RenderDiaryEntries(entries []DiaryEntryRecord) string {
	var b strings.Builder
	currentDate := ""
	for _, e := range entries {
		if e.Date != currentDate {
			if currentDate != "" {
				b.WriteString("\n\n")
			}
			fmt.Fprintf(&b, "**%s**:\n", e.Date)
			currentDate = e.Date
		} else {
			b.WriteString("\n")
		}
		b.WriteString("- " + e.Text)
	}
	return b.String()
}

// RenderCoreMemory điền CoreState + diary text + observation text vào template — dựng lại
// system prompt phần "trí nhớ cốt lõi" từ dữ liệu có cấu trúc trong DB.
func RenderCoreMemory(template string, state memdb.CoreState, diaryText, toneText, prefText string) string {
	r := strings.NewReplacer(
		PlaceholderLastMessageNote, state.LastMessageNote,
		PlaceholderSleepNote, state.SleepNote,
		PlaceholderEmotionalState, state.EmotionalState,
		PlaceholderMang1, state.Mang1,
		PlaceholderMang2, state.Mang2,
		PlaceholderMang3, state.Mang3,
		PlaceholderDiary, diaryText,
		PlaceholderToneObservations, toneText,
		PlaceholderPreferenceObservations, prefText,
	)
	return r.Replace(template)
}
