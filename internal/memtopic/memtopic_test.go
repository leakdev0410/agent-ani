package memtopic

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"ani-telegram/internal/memdb"
)

func TestAppendNote_InsertsRow(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()

	if err := db.Write(memdb.TopicKey("projects"), "# Công việc / dự án\n"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := AppendNote(db, "projects", "TEST-MARKER-note-1"); err != nil {
		t.Fatalf("AppendNote lần 1: %v", err)
	}
	if err := AppendNote(db, "projects", "TEST-MARKER-note-2"); err != nil {
		t.Fatalf("AppendNote lần 2: %v", err)
	}

	notes, err := db.TopicNotes("projects")
	if err != nil {
		t.Fatalf("TopicNotes: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("mong đợi 2 note, được %d", len(notes))
	}
	if notes[0].Text != "TEST-MARKER-note-1" || notes[1].Text != "TEST-MARKER-note-2" {
		t.Errorf("nội dung note sai: %+v", notes)
	}

	// Nội dung tĩnh gốc phải còn nguyên, không bị đụng vào.
	content, err := db.Read(memdb.TopicKey("projects"))
	if err != nil {
		t.Fatalf("đọc lại nội dung tĩnh: %v", err)
	}
	if content != "# Công việc / dự án\n" {
		t.Errorf("nội dung tĩnh bị thay đổi: %q", content)
	}
}

func TestAppendNote_TopicNotExist(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()

	if err := AppendNote(db, "khong-ton-tai", "note"); err == nil {
		t.Errorf("mong đợi lỗi khi chủ đề chưa tồn tại, nhưng không có lỗi")
	}
}

func TestRenderFull_IncludesStaticAndNotes(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()
	if err := db.Write(memdb.TopicKey("projects"), "# Công việc\n- Việc A"); err != nil {
		t.Fatal(err)
	}
	if err := AppendNote(db, "projects", "note mới"); err != nil {
		t.Fatal(err)
	}
	got, err := RenderFull(db, "projects")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "- Việc A") || !strings.Contains(got, "note mới") {
		t.Errorf("RenderFull thiếu nội dung: %q", got)
	}
}

func TestAppendNote_EmptyNote(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()
	if err := db.Write(memdb.TopicKey("log"), "# Ghi chú gần đây\n"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := AppendNote(db, "log", "   "); err == nil {
		t.Errorf("mong đợi lỗi khi ghi chú rỗng, nhưng không có lỗi")
	}
}

func TestRenderFull_IncludesTopicDiaryAcrossDays(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()
	if err := db.Write(memdb.TopicKey("projects"), "# Công việc\n"); err != nil {
		t.Fatal(err)
	}

	for _, e := range []struct{ date, text string }{
		{"2026-08-10", "10:00: việc projects ngày 10"},
		{"2026-08-09", "09:00: việc projects ngày 09"},
		{"2026-08-09", "11:00: việc projects ngày 09 buổi trưa"},
	} {
		if err := db.AddDiaryEntry(e.date, e.text, "projects"); err != nil {
			t.Fatal(err)
		}
	}
	// Dòng gắn chủ đề khác không được lọt vào.
	if err := db.AddDiaryEntry("2026-08-09", "12:00: việc personal", "personal"); err != nil {
		t.Fatal(err)
	}

	got, err := RenderFull(db, "projects")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Dấu ấn cảm xúc liên quan (mọi ngày)",
		"**2026-08-10**:",
		"- 10:00: việc projects ngày 10",
		"**2026-08-09**:",
		"- 09:00: việc projects ngày 09",
		"- 11:00: việc projects ngày 09 buổi trưa",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderFull thiếu %q trong:\n%s", want, got)
		}
	}
	if strings.Contains(got, "việc personal") {
		t.Errorf("RenderFull lọt dấu ấn của chủ đề khác:\n%s", got)
	}
	// Ngày mới phải đứng trước ngày cũ.
	if strings.Index(got, "2026-08-10") > strings.Index(got, "2026-08-09") {
		t.Errorf("dấu ấn phải nhóm ngày mới nhất trước:\n%s", got)
	}
}

func TestRenderFull_TopicDiaryLimitWithHint(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()
	if err := db.Write(memdb.TopicKey("log"), "# Log\n"); err != nil {
		t.Fatal(err)
	}

	total := TopicDiaryBulletLimit + 3
	for i := 1; i <= total; i++ {
		if err := db.AddDiaryEntry("2026-08-05", fmt.Sprintf("%02d:00: log-entry-%02d", i, i), "log"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := RenderFull(db, "log")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "còn 3 dấu ấn") {
		t.Errorf("phải có hint số dấu ấn bị cắt:\n%s", got)
	}
	if !strings.Contains(got, "recall_memory(date)") {
		t.Errorf("hint phải gợi recall_memory theo ngày:\n%s", got)
	}
	if !strings.Contains(got, "log-entry-01") {
		t.Errorf("bullet cũ nhất trong hạn mức phải hiện:\n%s", got)
	}
	if strings.Contains(got, "log-entry-53") {
		t.Errorf("bullet vượt hạn mức không nên hiện:\n%s", got)
	}
}

func TestRenderFull_NoDiarySectionWhenTopicUntagged(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()
	if err := db.Write(memdb.TopicKey("projects"), "# Công việc\n"); err != nil {
		t.Fatal(err)
	}
	got, err := RenderFull(db, "projects")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, diaryByTopicHeading) {
		t.Errorf("chủ đề chưa có dấu ấn thì không nên có mục diary:\n%s", got)
	}
}
