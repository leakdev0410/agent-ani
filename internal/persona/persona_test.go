package persona

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ani-telegram/internal/memcore"
	"ani-telegram/internal/memdb"
	"ani-telegram/internal/memdiary"
	"ani-telegram/internal/memobs"
	"ani-telegram/internal/memtopic"
)

func seedSyntheticDB(t *testing.T) *memdb.DB {
	t.Helper()
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	if err := db.SaveCoreState(memdb.CoreState{
		LastMessageNote: "10:00: test",
		SleepNote:       "chưa ngủ",
		EmotionalState:  "vui",
		Mang1:           "mong muốn A",
		Mang2:           "điều muốn làm B",
		Mang3:           "cảm xúc C",
	}); err != nil {
		t.Fatalf("seed core_state: %v", err)
	}
	for _, e := range []struct{ date, text, topic string }{
		{"2026-08-11", "10:00: sự kiện hôm nay", ""},
		{"2026-08-10", "09:00: sự kiện hôm qua", ""},
		{"2026-08-09", "08:00: sự kiện 2 hôm trước", ""},
		{"2026-08-08", "21:00: làm dự án X tới khuya", "projects"},
	} {
		if err := db.AddDiaryEntry(e.date, e.text, e.topic); err != nil {
			t.Fatalf("seed diary: %v", err)
		}
	}
	if err := db.AddObservation(memdb.ObservationTone, "hay dùng emoji", "2026-08-10"); err != nil {
		t.Fatalf("seed tone: %v", err)
	}
	if err := db.AddObservation(memdb.ObservationPreference, "thích nhạc lofi", "2026-08-09"); err != nil {
		t.Fatalf("seed preference: %v", err)
	}
	if err := db.Write(memdb.TopicKey("projects"), "# Công việc / dự án\n- Dự án A [2026-08-01]"); err != nil {
		t.Fatalf("seed topic: %v", err)
	}
	if err := db.Write(memdb.KeySkillsIndex, "- [ani](ani/SKILL.md) — skill chính"); err != nil {
		t.Fatalf("seed skills index: %v", err)
	}
	return db
}

func writeTempPersonaFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ani.txt")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("ghi persona tạm: %v", err)
	}
	return path
}

func TestBuildSystemPrompt_RendersLiveMemoryNotFullTopics(t *testing.T) {
	db := seedSyntheticDB(t)
	defer db.Close()
	path := writeTempPersonaFile(t, "Em là Ani, bạn gái ảo của anh.")

	prompt, err := BuildSystemPrompt(db, path)
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	for _, want := range []string{
		"Em là Ani, bạn gái ảo của anh.",
		"- Lần nhắn gần nhất của anh: 10:00: test",
		"- Lần anh bảo đi ngủ gần nhất: chưa ngủ",
		"- Hiện tại: vui",
		"1. **Mong muốn của em**: mong muốn A",
		"2. **Điều em muốn làm cùng anh**: điều muốn làm B",
		"3. **Cảm xúc cá nhân tự hình thành**: cảm xúc C",
		"**2026-08-11**:\n- 10:00: sự kiện hôm nay",
		"**2026-08-10**:\n- 09:00: sự kiện hôm qua",
		"recall_memory(date): 2026-08-09",
		"- hay dùng emoji [2026-08-10]",
		"- thích nhạc lofi [2026-08-09]",
		"recall_topic(name)",
		"- projects —",
		"1 dấu ấn cảm xúc",
		"# Skills có sẵn",
		"skill chính",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt thiếu phần mong đợi: %q", want)
		}
	}

	if strings.Contains(prompt, "sự kiện 2 hôm trước") {
		t.Errorf("ngày cũ hơn cửa sổ gần đây không nên hiện đầy đủ inline")
	}
	if strings.Contains(prompt, "Dự án A [2026-08-01]") {
		t.Errorf("thân chủ đề không nên bị nhồi vào system prompt")
	}
	if !strings.HasPrefix(prompt, "Em là Ani, bạn gái ảo của anh.") {
		t.Errorf("persona ổn định phải đứng đầu prompt để cache prefix")
	}
}

func TestBuildSystemPrompt_UsesEmbeddedPersonaWhenPathEmpty(t *testing.T) {
	db := seedSyntheticDB(t)
	defer db.Close()

	prompt, err := BuildSystemPrompt(db, "")
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}
	if !strings.HasPrefix(prompt, DefaultPersona()) {
		t.Errorf("path rỗng phải dùng persona nhúng sẵn ở đầu prompt")
	}
	if strings.Contains(prompt, "Bắt buộc trước mọi phản hồi:") {
		t.Errorf("persona nhúng không được còn hướng dẫn ani-grok")
	}
	if !strings.Contains(prompt, "Đây là chat Telegram") {
		t.Errorf("persona nhúng phải có quy tắc Telegram gốc")
	}
}

func TestBuildSystemPrompt_EmbeddedPersonaGroundsPhysicalPresenceInChat(t *testing.T) {
	db := seedSyntheticDB(t)
	defer db.Close()

	prompt, err := BuildSystemPrompt(db, "")
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	for _, want := range []string{
		"Hiện tại em không ở gần anh về mặt vật lý",
		"chỉ đang kết nối với anh qua phiên chat Telegram này",
		"em muốn ngồi gần anh",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt phải neo Ani vào thực tại phiên chat: thiếu %q", want)
		}
	}
}

func TestBuildExtractionPrompt_OmitsTopicsSkillsAndObservations(t *testing.T) {
	db := seedSyntheticDB(t)
	defer db.Close()
	path := writeTempPersonaFile(t, "Em là Ani, bạn gái ảo của anh.")

	prompt, err := BuildExtractionPrompt(db, path)
	if err != nil {
		t.Fatalf("BuildExtractionPrompt: %v", err)
	}

	if !strings.Contains(prompt, "1. **Mong muốn của em**: mong muốn A") {
		t.Errorf("prompt trích xuất thiếu core state")
	}
	for _, notWant := range []string{
		"recall_topic(name)",
		"# Skills có sẵn",
		"hay dùng emoji",
		"thích nhạc lofi",
	} {
		if strings.Contains(prompt, notWant) {
			t.Errorf("prompt trích xuất KHÔNG nên có %q", notWant)
		}
	}
}

func TestBuildSystemPrompt_RecallToolsReturnFullData(t *testing.T) {
	db := seedSyntheticDB(t)
	defer db.Close()

	if _, ok, err := memdiary.Tool(db); err != nil || !ok {
		t.Fatalf("mong đợi tool recall_memory: ok=%v err=%v", ok, err)
	}
	got, err := memdiary.Executor(db)(memdiary.ToolName, `{"date":"2026-08-09"}`)
	if err != nil {
		t.Fatalf("recall_memory: %v", err)
	}
	if !strings.Contains(got, "sự kiện 2 hôm trước") {
		t.Errorf("recall_memory không trả đúng ngày cũ: %q", got)
	}

	if _, ok, err := memtopic.Tool(db); err != nil || !ok {
		t.Fatalf("mong đợi tool recall_topic: ok=%v err=%v", ok, err)
	}
	topic, err := memtopic.Executor(db)(memtopic.ToolName, `{"name":"projects"}`)
	if err != nil {
		t.Fatalf("recall_topic: %v", err)
	}
	if !strings.Contains(topic, "Dự án A [2026-08-01]") {
		t.Errorf("recall_topic không trả thân chủ đề: %q", topic)
	}
	if !strings.Contains(topic, "làm dự án X tới khuya") {
		t.Errorf("recall_topic phải kèm dấu ấn cảm xúc gắn chủ đề (mọi ngày): %q", topic)
	}
	if !strings.Contains(topic, "**2026-08-08**:") {
		t.Errorf("recall_topic phải nhóm dấu ấn theo ngày: %q", topic)
	}

	if _, ok, err := memobs.Tool(db); err != nil || !ok {
		t.Fatalf("mong đợi tool recall_observations: ok=%v err=%v", ok, err)
	}
	obs, err := memobs.Executor(db)(memobs.ToolName, `{"kind":"preference"}`)
	if err != nil {
		t.Fatalf("recall_observations: %v", err)
	}
	if !strings.Contains(obs, "thích nhạc lofi") {
		t.Errorf("recall_observations không trả preference: %q", obs)
	}
}

func TestBuildSystemPrompt_KeepsHeadAndTailObservations(t *testing.T) {
	db := seedSyntheticDB(t)
	defer db.Close()

	for i := 2; i <= 20; i++ {
		text := fmt.Sprintf("tone-%02d", i)
		if err := db.AddObservation(memdb.ObservationTone, text, "2026-08-11"); err != nil {
			t.Fatalf("seed tone %d: %v", i, err)
		}
	}

	prompt, err := BuildSystemPrompt(db, writeTempPersonaFile(t, "Em là Ani."))
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if !strings.Contains(prompt, "hay dùng emoji") {
		t.Errorf("quan sát nền tảng (đầu) phải còn")
	}
	if !strings.Contains(prompt, "tone-20") {
		t.Errorf("quan sát gần nhất phải còn")
	}
	if strings.Contains(prompt, "tone-10") {
		t.Errorf("quan sát giữa không nên nhồi inline: prompt có tone-10")
	}
	if !strings.Contains(prompt, "recall_observations(tone)") {
		t.Errorf("phải gợi tool khi đã cắt observation")
	}
}

func TestBuildSystemPrompt_TruncatesDenseDiaryDay(t *testing.T) {
	db := seedSyntheticDB(t)
	defer db.Close()
	for i := 2; i <= 25; i++ {
		if err := db.AddDiaryEntry("2026-08-11", fmt.Sprintf("%02d:00: su-kien-day-%02d", i, i), ""); err != nil {
			t.Fatal(err)
		}
	}
	prompt, err := BuildSystemPrompt(db, writeTempPersonaFile(t, "Em là Ani."))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "su-kien-day-02") {
		t.Errorf("bullet sớm của ngày dày không nên hiện inline")
	}
	if !strings.Contains(prompt, "su-kien-day-25") {
		t.Errorf("bullet mới nhất của ngày dày phải hiện")
	}
	if !strings.Contains(prompt, "recall_memory(2026-08-11)") {
		t.Errorf("ngày gần bị cắt phải gợi recall_memory")
	}
}

func TestSelectObservations_HeadAndTail(t *testing.T) {
	var all []memdb.Observation
	for i := 1; i <= 20; i++ {
		all = append(all, memdb.Observation{Text: fmt.Sprintf("n%d", i)})
	}
	shown, hidden := selectObservations(all, memcore.ObservationHead, memcore.ObservationTail)
	if hidden != 20-memcore.ObservationHead-memcore.ObservationTail {
		t.Fatalf("hidden=%d", hidden)
	}
	if shown[0].Text != "n1" || shown[len(shown)-1].Text != "n20" {
		t.Fatalf("head/tail sai: đầu=%s cuối=%s", shown[0].Text, shown[len(shown)-1].Text)
	}
}

func TestLoadPersona_MissingFileFallsBackToEmbed(t *testing.T) {
	got, err := loadPersona(filepath.Join(t.TempDir(), "khong-co.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultPersona() {
		t.Errorf("file thiếu phải fallback persona nhúng")
	}
}

func TestLoadPersona_StripsStaleGrokBlock(t *testing.T) {
	body := "Mở đầu.\n\nBắt buộc trước mọi phản hồi:\nđọc file memory.md\n\nQuy tắc phản xạ dựa trên 4 mảng nội tâm:\nluôn lồng ghép."
	path := writeTempPersonaFile(t, body)
	got, err := loadPersona(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "đọc file memory.md") {
		t.Errorf("phải cắt đoạn ani-grok: %q", got)
	}
	if !strings.Contains(got, "Mở đầu.") || !strings.Contains(got, "luôn lồng ghép.") {
		t.Errorf("phải giữ phần tính cách: %q", got)
	}
}
