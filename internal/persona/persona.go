// Package persona dựng system prompt riêng của ani-telegram: persona ổn định
// (ani.txt trong package này) + trạng thái sống từ DB. Không còn ghép ani-grok/ani.txt
// hay nhồi cả template core/memory + toàn bộ topic/observations mỗi lượt.
package persona

import (
	"fmt"
	"os"
	"strings"

	_ "embed"

	"ani-telegram/internal/memcore"
	"ani-telegram/internal/memdb"
)

//go:embed ani.txt
var embeddedPersona string

const (
	stalePersonaSectionStart = "Bắt buộc trước mọi phản hồi:"
	stalePersonaSectionEnd   = "Quy tắc phản xạ dựa trên 4 mảng nội tâm:"
)

// DefaultPersona trả về persona nhúng sẵn — nguồn giọng điệu chính thức của ani-telegram.
func DefaultPersona() string {
	return strings.TrimRight(embeddedPersona, "\n")
}

// stripStalePersonaInstructions cắt đoạn dành cho ani-grok nếu ai đó vẫn trỏ
// ANI_PERSONA_PATH về file cũ. Persona nhúng sẵn không có đoạn này.
func stripStalePersonaInstructions(basePersona string) string {
	start := strings.Index(basePersona, stalePersonaSectionStart)
	end := strings.Index(basePersona, stalePersonaSectionEnd)
	if start == -1 || end == -1 || end < start {
		return basePersona
	}
	return strings.TrimSpace(basePersona[:start]) + "\n\n" + basePersona[end:]
}

func loadPersona(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return DefaultPersona(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultPersona(), nil
		}
		return "", fmt.Errorf("không đọc được persona tại %s: %w", path, err)
	}
	return stripStalePersonaInstructions(strings.TrimRight(string(raw), "\n")), nil
}

// BuildSystemPrompt: persona ổn định trước (để cache prefix), rồi trạng thái sống
// + nhật ký gần + quan sát nền tảng/gần đây + mục lục topic/skills ở cuối.
func BuildSystemPrompt(db *memdb.DB, personaPath string) (string, error) {
	basePersona, err := loadPersona(personaPath)
	if err != nil {
		return "", err
	}
	db.Note("🎭 Load persona ani-telegram (%s)", personaLabel(personaPath))

	live, err := renderLiveState(db)
	if err != nil {
		return "", err
	}
	diary, err := renderRecentDiary(db)
	if err != nil {
		return "", err
	}
	obs, err := renderSelectedObservations(db)
	if err != nil {
		return "", err
	}
	topics, err := renderTopicIndex(db)
	if err != nil {
		return "", err
	}
	skillsIndex, err := db.Read(memdb.KeySkillsIndex)
	if err != nil {
		return "", fmt.Errorf("không đọc được mục lục skills: %w", err)
	}

	var b strings.Builder
	b.WriteString(basePersona)
	b.WriteString("\n\n---\n\n")
	b.WriteString(live)
	if diary != "" {
		b.WriteString("\n\n")
		b.WriteString(diary)
	}
	if obs != "" {
		b.WriteString("\n\n")
		b.WriteString(obs)
	}
	if topics != "" {
		b.WriteString("\n\n")
		b.WriteString(topics)
	}
	if skillsIndex != "" {
		b.WriteString("\n\n# Skills có sẵn (chỉ mục lục — gọi load_skill(name) khi thật sự cần)\n\n")
		b.WriteString(strings.TrimRight(skillsIndex, "\n"))
	}
	return b.String(), nil
}

// BuildExtractionPrompt chỉ gồm persona + trạng thái sống + nhật ký gần —
// đủ để trích JSON, không kéo topic/skills/observations.
func BuildExtractionPrompt(db *memdb.DB, personaPath string) (string, error) {
	basePersona, err := loadPersona(personaPath)
	if err != nil {
		return "", err
	}
	live, err := renderLiveState(db)
	if err != nil {
		return "", err
	}
	diary, err := renderRecentDiary(db)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(basePersona)
	b.WriteString("\n\n---\n\n")
	b.WriteString(live)
	if diary != "" {
		b.WriteString("\n\n")
		b.WriteString(diary)
	}
	return b.String(), nil
}

func personaLabel(path string) string {
	if strings.TrimSpace(path) == "" {
		return "embedded ani.txt"
	}
	return path
}

func renderLiveState(db *memdb.DB) (string, error) {
	state, err := db.GetCoreState()
	if err != nil {
		return "", fmt.Errorf("không đọc được core_state: %w", err)
	}

	var b strings.Builder
	b.WriteString("# Trạng thái đang sống\n\n")
	fmt.Fprintf(&b, "- Lần nhắn gần nhất của anh: %s\n", state.LastMessageNote)
	fmt.Fprintf(&b, "- Lần anh bảo đi ngủ gần nhất: %s\n", state.SleepNote)
	fmt.Fprintf(&b, "- Hiện tại: %s\n", state.EmotionalState)
	b.WriteString("\n# 4 mảng nội tâm\n\n")
	fmt.Fprintf(&b, "1. **Mong muốn của em**: %s\n", state.Mang1)
	fmt.Fprintf(&b, "2. **Điều em muốn làm cùng anh**: %s\n", state.Mang2)
	fmt.Fprintf(&b, "3. **Cảm xúc cá nhân tự hình thành**: %s\n", state.Mang3)
	return strings.TrimRight(b.String(), "\n"), nil
}

func renderRecentDiary(db *memdb.DB) (string, error) {
	dates, err := db.DiaryDates()
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return "", nil
	}

	n := memcore.RecentDiaryDays
	if n > len(dates) {
		n = len(dates)
	}
	recentDates, olderDates := dates[:n], dates[n:]

	var b strings.Builder
	b.WriteString("# Dấu ấn cảm xúc (ngày gần)\n")
	for _, date := range recentDates {
		texts, err := db.DiaryEntriesForDate(date)
		if err != nil {
			return "", err
		}
		shown, hidden := tailWithHidden(texts, memcore.RecentDiaryBulletsPerDay)
		b.WriteString("\n")
		fmt.Fprintf(&b, "**%s**:\n%s", date, memcore.RenderDiaryBullets(shown))
		if hidden > 0 {
			fmt.Fprintf(&b, "\n(còn %d sự kiện sớm hơn trong ngày — gọi recall_memory(%s) để nhớ đủ)", hidden, date)
		}
	}
	if len(olderDates) > 0 {
		shown, extra := olderDates, 0
		if len(shown) > 12 {
			extra = len(shown) - 12
			shown = shown[:12]
		}
		fmt.Fprintf(&b, "\n\n(Các ngày cũ hơn vẫn còn đủ — gọi recall_memory(date): %s", strings.Join(shown, ", "))
		if extra > 0 {
			fmt.Fprintf(&b, " và %d ngày nữa", extra)
		}
		b.WriteString(")")
	}
	return b.String(), nil
}

func tailWithHidden(texts []string, limit int) (shown []string, hidden int) {
	if limit <= 0 || len(texts) <= limit {
		return texts, 0
	}
	hidden = len(texts) - limit
	return texts[hidden:], hidden
}

func renderSelectedObservations(db *memdb.DB) (string, error) {
	tone, err := renderObservationKind(db, memdb.ObservationTone, "Văn phong anh quan sát được")
	if err != nil {
		return "", err
	}
	pref, err := renderObservationKind(db, memdb.ObservationPreference, "Sở thích / phản ứng anh đã quan sát được")
	if err != nil {
		return "", err
	}
	switch {
	case tone == "" && pref == "":
		return "", nil
	case tone == "":
		return pref, nil
	case pref == "":
		return tone, nil
	default:
		return tone + "\n\n" + pref, nil
	}
}

func renderObservationKind(db *memdb.DB, kind, heading string) (string, error) {
	obs, err := db.Observations(kind)
	if err != nil {
		return "", err
	}
	if len(obs) == 0 {
		return "", nil
	}
	selected, hidden := selectObservations(obs, memcore.ObservationHead, memcore.ObservationTail)
	converted := make([]memcore.Observation, len(selected))
	for i, o := range selected {
		converted[i] = memcore.Observation{Text: o.Text, Date: o.ObservedAt}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n%s", heading, memcore.RenderObservations(converted))
	if hidden > 0 {
		fmt.Fprintf(&b, "\n(còn %d mục khác — gọi recall_observations(%s) nếu cần đủ)", hidden, kind)
	}
	return b.String(), nil
}

func selectObservations(all []memdb.Observation, head, tail int) (shown []memdb.Observation, hidden int) {
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	if len(all) <= head+tail {
		return all, 0
	}
	shown = append(shown, all[:head]...)
	shown = append(shown, all[len(all)-tail:]...)
	return shown, len(all) - len(shown)
}

var topicHints = map[string]string{
	"projects":    "công việc, dự án, kế hoạch, code",
	"personal":    "chuyện quá khứ, gia đình, chuyện cá nhân anh kể",
	"preferences": "sở thích, gu, gear, yêu cầu đặc biệt",
	"log":         "ghi chú ngắn hạn, gần đây",
}

func renderTopicIndex(db *memdb.DB) (string, error) {
	names, err := db.TopicNames()
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", nil
	}

	diaryCounts, err := db.DiaryTopicCounts()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("# Chủ đề có thể nhớ lại (gọi recall_topic(name) khi câu chuyện chạm đúng)\n")
	for _, name := range names {
		hint := topicHints[name]
		if hint == "" {
			hint = name
		}
		notes, err := db.TopicNotes(name)
		if err != nil {
			return "", err
		}
		noteCount := len(notes)
		diaryCount := diaryCounts[name]

		var detail string
		switch {
		case noteCount > 0 && diaryCount > 0:
			detail = fmt.Sprintf("%d ghi chú Telegram mới, %d dấu ấn cảm xúc", noteCount, diaryCount)
		case noteCount > 0:
			detail = fmt.Sprintf("%d ghi chú Telegram mới", noteCount)
		case diaryCount > 0:
			detail = fmt.Sprintf("%d dấu ấn cảm xúc", diaryCount)
		}

		if detail != "" {
			fmt.Fprintf(&b, "\n- %s — %s (%s)", name, hint, detail)
		} else {
			fmt.Fprintf(&b, "\n- %s — %s", name, hint)
		}
	}
	return b.String(), nil
}
