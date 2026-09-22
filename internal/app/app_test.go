package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
)

func TestEnsureSearchIndex_MakesSeededTopicSearchable(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Write(memdb.TopicKey("projects"), "Kế hoạch: hoàn thiện trình tìm kiếm ký ức FTS5."); err != nil {
		t.Fatal(err)
	}
	if err := ensureSearchIndex(db, ""); err != nil {
		t.Fatal(err)
	}

	results, err := db.SearchMemory("tìm kiếm ký ức", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SourceType != "topic" {
		t.Fatalf("seeded topic must be searchable after startup rebuild, got %+v", results)
	}
}

// newTestScheduler dựng 1 scheduler gắn với memory.db tạm (mỗi test 1 file riêng) — proactive_test.go
// dùng chung helper này. allowedChatIDs[0] nếu có sẽ là chat được whitelist; mặc định 42.
func newTestScheduler(t *testing.T, allowedChatIDs ...int64) (*proactiveScheduler, *memdb.DB, chan chatJob) {
	t.Helper()
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	jobs := make(chan chatJob, 4)
	allowedChatID := int64(42)
	if len(allowedChatIDs) > 0 {
		allowedChatID = allowedChatIDs[0]
	}
	sched := newProactiveScheduler(jobs, db, allowedChatID)
	t.Cleanup(sched.Cancel)
	return sched, db, jobs
}

func TestSplitTelegramTextRespectsUTF16Limit(t *testing.T) {
	text := strings.Repeat("😀", 2500)
	parts := splitTelegramText(text, 4096)
	if len(parts) != 2 {
		t.Fatalf("parts=%d, want 2", len(parts))
	}
	for _, part := range parts {
		if utf16Units(part) > 4096 {
			t.Fatalf("part vượt UTF-16 limit")
		}
	}
	if strings.Join(parts, "") != text {
		t.Fatal("split làm mất nội dung")
	}
}

func TestCommandParsingHandlesBotSuffix(t *testing.T) {
	if !matchesCommand("/help@AniBot", helpCommand) {
		t.Fatal("không nhận command có bot suffix")
	}
	if arg, ok := parseArgCommand("/model@AniBot deepseek/deepseek-chat", modelCommand); !ok || arg != "deepseek/deepseek-chat" {
		t.Fatalf("arg=%q ok=%v", arg, ok)
	}
	if matchesCommand("/helpful", helpCommand) {
		t.Fatal("không được prefix-match command")
	}
}

func TestDescribeStatusAlwaysIncludesProactiveLine(t *testing.T) {
	sched := newProactiveScheduler(make(chan chatJob, 1), nil, 42)
	got := describeStatus(&botStatus{}, 0, sched)
	if !strings.Contains(got, "🟢") || !strings.Contains(got, "⏰") || !strings.Contains(got, "heap") {
		t.Fatalf("status thiếu phần: %q", got)
	}
}

// This fails if /status stops exposing only operational journal/index state,
// or leaks either temporary chat content or committed embedding source text.
func TestDescribeStatusWithMemoryJobsReportsPrivateSafeJournalAndEmbeddingState(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const temporaryPayload = "private temporary chat payload"
	const committedText = "private committed embedding source"
	if _, err := db.CreateMemoryJob([]byte(temporaryPayload)); err != nil {
		t.Fatalf("create memory job: %v", err)
	}
	if err := db.AddDiaryEntry("2026-08-26", committedText, "projects"); err != nil {
		t.Fatalf("add committed embedding source: %v", err)
	}
	if err := db.RebuildSearchIndex(); err != nil {
		t.Fatalf("rebuild search index: %v", err)
	}

	sched := newProactiveScheduler(make(chan chatJob, 1), nil, 42)
	got := describeStatusWithMemoryJobs(&botStatus{}, 0, sched, db)
	for _, want := range []string{
		"Memory journal: pending 1, ready 0",
		"job cũ nhất",
		"Semantic index: queued 1, indexed 0",
		"queue cũ nhất",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status thiếu %q: %q", want, got)
		}
	}
	if strings.Contains(got, "blocked") {
		t.Fatalf("status must not expose a blocked journal state: %q", got)
	}
	for _, private := range []string{temporaryPayload, committedText, "diary:1:2026-08-26"} {
		if strings.Contains(got, private) {
			t.Fatalf("status làm lộ nội dung private %q: %q", private, got)
		}
	}
}

func TestFlexIntUnmarshalToleratesModelQuirks(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{`45`, 45},
		{`"45"`, 45},
		{`""`, 0},
		{`null`, 0},
		{`" 90 "`, 90},
		{`"khoảng 45 phút"`, 45},
		{`"45.5"`, 45},
		{`"không hẹn"`, 0},
		{`0`, 0},
	}
	for _, c := range cases {
		var got flexInt
		if err := json.Unmarshal([]byte(c.raw), &got); err != nil {
			t.Errorf("%s: không được trả lỗi (1 field lệch kiểu làm hỏng cả JSON ghi memory): %v", c.raw, err)
			continue
		}
		if int(got) != c.want {
			t.Errorf("%s: được %d, mong đợi %d", c.raw, int(got), c.want)
		}
	}
}

func TestApptListUnmarshalToleratesModelQuirks(t *testing.T) {
	var single apptList
	if err := json.Unmarshal([]byte(`{"after_minutes":30,"reason":"lẻ 1 object"}`), &single); err != nil {
		t.Fatalf("object lẻ: %v", err)
	}
	if len(single) != 1 || single[0].Reason != "lẻ 1 object" {
		t.Fatalf("object lẻ phải thành mảng 1 phần tử: %+v", single)
	}

	var bareNums apptList
	if err := json.Unmarshal([]byte(`[30, 180]`), &bareNums); err != nil {
		t.Fatalf("mảng số trần: %v", err)
	}
	if len(bareNums) != 2 || bareNums[0].AfterMinutes != 30 || bareNums[1].AfterMinutes != 180 {
		t.Fatalf("mảng số trần vớt sai: %+v", bareNums)
	}

	var empty apptList
	if err := json.Unmarshal([]byte(`null`), &empty); err != nil || len(empty) != 0 {
		t.Fatalf("null phải thành mảng rỗng không lỗi: err=%v len=%d", err, len(empty))
	}
}

func TestMemoryUpdateJSONProactiveFieldAsString(t *testing.T) {
	raw := `{"last_message_note":"21:15 anh đang code","emotional_state":"vui",
		"proactive_after_minutes":"45","proactive_reason":"anh nói code dở, đợi anh nghỉ tay"}`
	var parsed memoryUpdateJSON
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if int(parsed.ProactiveAfterMinutes) != 45 {
		t.Errorf("phút sai: %d", int(parsed.ProactiveAfterMinutes))
	}
	if parsed.ProactiveReason == "" {
		t.Errorf("mất proactive_reason")
	}
	if parsed.LastMessageNote == "" {
		t.Errorf("field cũ bị ảnh hưởng: %q", parsed.LastMessageNote)
	}
}

func TestAppendAssistantMergesWhenLastIsAssistant(t *testing.T) {
	h := newHistoryStore()
	h.Append(1, "chào em", "chào anh")

	got := h.AppendAssistant(1, "anh ngủ chưa")
	if len(got) != 2 {
		t.Fatalf("mong đợi 2 message (giữ luân phiên user/assistant), được %d: %+v", len(got), got)
	}
	if got[1].Role != "assistant" {
		t.Fatalf("message cuối phải là assistant, được %q", got[1].Role)
	}
	if text := messageText(got[1]); text != "chào anh\nanh ngủ chưa" {
		t.Errorf("nội dung gộp sai: %q", text)
	}
}

func TestAppendAssistantAddsNewWhenLastIsUser(t *testing.T) {
	h := newHistoryStore()
	h.mu.Lock()
	h.data[1] = []openrouter.Message{{Role: "user", Content: "ngủ đây"}}
	h.mu.Unlock()

	got := h.AppendAssistant(1, "ngủ ngon nha anh")
	if len(got) != 2 || got[1].Role != "assistant" {
		t.Fatalf("mong đợi thêm 1 message assistant mới, được %+v", got)
	}
	if messageText(got[1]) != "ngủ ngon nha anh" {
		t.Errorf("nội dung sai: %q", messageText(got[1]))
	}
}

func TestAppendAssistantCapsAtMaxHistoryTurns(t *testing.T) {
	h := newHistoryStore()
	for i := 0; i < maxHistoryTurns; i++ {
		h.Append(1, "hỏi", "đáp")
	}
	got := h.AppendAssistant(1, "thêm câu nữa")
	if len(got) > maxHistoryTurns*2 {
		t.Errorf("vượt giới hạn: %d message (tối đa %d)", len(got), maxHistoryTurns*2)
	}
}

func TestIdleChatsRequiresBothHistoryAndElapsedTime(t *testing.T) {
	h := newHistoryStore()
	now := time.Now()

	// Chưa có tin nhắn nào -> không có gì để seal.
	if got := h.IdleChats(now, idleSealAfter); len(got) != 0 {
		t.Fatalf("chat rỗng không được coi là idle: %v", got)
	}

	h.Append(1, "chào em", "chào anh")
	h.NoteUserMessage(1, now.Add(-idleSealAfter+time.Minute))

	// Mới im lặng idleSealAfter-1' -> chưa idle.
	if got := h.IdleChats(now, idleSealAfter); len(got) != 0 {
		t.Fatalf("chưa đủ thời gian im lặng không được coi là idle: %v", got)
	}

	// 2 phút sau, tổng cộng đã im lặng idleSealAfter+1' -> idle.
	if got := h.IdleChats(now.Add(2*time.Minute), idleSealAfter); len(got) != 1 || got[0] != 1 {
		t.Fatalf("chat im lặng quá idleSealAfter phải được liệt kê, được: %v", got)
	}
}

func TestIdleChatsIgnoresChatWithoutHistory(t *testing.T) {
	h := newHistoryStore()
	now := time.Now()
	// NoteUserMessage mà chưa từng Append (VD chỉ gõ lệnh) -> không có history để seal.
	h.NoteUserMessage(2, now.Add(-idleSealAfter-time.Minute))
	if got := h.IdleChats(now, idleSealAfter); len(got) != 0 {
		t.Fatalf("chat chưa có history không được coi là idle: %v", got)
	}
}

func TestHistoryDeleteClearsIdleTracking(t *testing.T) {
	h := newHistoryStore()
	now := time.Now()
	h.Append(1, "chào em", "chào anh")
	h.NoteUserMessage(1, now.Add(-idleSealAfter-time.Minute))

	h.Delete(1)

	if got := h.IdleChats(now, idleSealAfter); len(got) != 0 {
		t.Fatalf("sau Delete không còn gì để seal, được: %v", got)
	}
	if got := h.Get(1); got != nil {
		t.Fatalf("Delete phải xoá sạch history, được: %v", got)
	}
}

func TestParseBackfillAssignmentsValid(t *testing.T) {
	raw := `{"assignments":[
		{"line":1,"topic":"projects"},
		{"line":2,"topic":"none"},
		{"line":3,"topic":"personal"}
	]}`
	got, err := parseBackfillAssignments(raw, 3)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[1] != "projects" || got[2] != "none" || got[3] != "personal" {
		t.Errorf("assignments sai: %v", got)
	}
}

func TestParseBackfillAssignmentsRejectsOutOfRange(t *testing.T) {
	raw := `{"assignments":[{"line":1,"topic":"projects"},{"line":9,"topic":"log"}]}`
	if _, err := parseBackfillAssignments(raw, 2); err == nil {
		t.Errorf("mong đợi lỗi khi line ngoài phạm vi")
	}
}

func TestBuildBackfillUserLinesMatchBatch(t *testing.T) {
	entries := []memdb.DiaryTopicEntry{
		{ID: 10, EntryDate: "2026-08-11", Text: "10:00: sáng làm việc"},
		{ID: 11, EntryDate: "2026-08-11", Text: "22:00: đi ngủ"},
	}
	got := buildBackfillUser(entries)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("mong đợi 2 dòng, được %d: %q", len(lines), got)
	}
	if lines[0] != "1. 2026-08-11 10:00: sáng làm việc" {
		t.Errorf("dòng 1 sai: %q", lines[0])
	}
	if lines[1] != "2. 2026-08-11 22:00: đi ngủ" {
		t.Errorf("dòng 2 sai: %q", lines[1])
	}
}
