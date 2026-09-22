package memdb

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestOpen_MigratesOldDiarySchema dựng 1 DB với schema cũ (diary_entries chưa có cột topic),
// ghi sẵn 1 row, rồi Open() — phải tự ALTER thêm cột topic mà giữ nguyên dữ liệu cũ.
func TestOpen_MigratesOldDiarySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("mở db legacy: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE diary_entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			entry_date TEXT NOT NULL,
			text TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		INSERT INTO diary_entries (entry_date, text, created_at) VALUES ('2026-08-10', '10:00: dữ liệu cũ', '2026-08-10T00:00:00Z');
	`); err != nil {
		t.Fatalf("dựng schema cũ: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("đóng db legacy: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() phải tự migrate schema cũ: %v", err)
	}
	defer db.Close()

	entries, err := db.DiaryEntriesForDate("2026-08-10")
	if err != nil {
		t.Fatalf("đọc dữ liệu cũ sau migrate: %v", err)
	}
	if len(entries) != 1 || entries[0] != "10:00: dữ liệu cũ" {
		t.Errorf("dữ liệu cũ bị mất sau migrate: %v", entries)
	}

	untagged, err := db.DiaryEntriesWithoutTopic()
	if err != nil {
		t.Fatalf("DiaryEntriesWithoutTopic: %v", err)
	}
	if len(untagged) != 1 {
		t.Errorf("row cũ phải có topic rỗng, được %d row", len(untagged))
	}

	if err := db.UpdateDiaryTopic(untagged[0].ID, "log"); err != nil {
		t.Fatalf("UpdateDiaryTopic: %v", err)
	}
	byTopic, err := db.DiaryEntriesForTopic("log", 0)
	if err != nil {
		t.Fatalf("DiaryEntriesForTopic: %v", err)
	}
	if len(byTopic) != 1 || byTopic[0].Text != "10:00: dữ liệu cũ" {
		t.Errorf("query theo topic sau migrate sai: %+v", byTopic)
	}
}

// TestOpen_NoDoubleMigration chạy Open 2 lần trên cùng 1 DB đã migrate — lần 2 không được lỗi
// hay thêm cột trùng.
func TestOpen_NoDoubleMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open lần 1: %v", err)
	}
	if err := db.AddDiaryEntry("2026-08-10", "09:00: entry", "projects"); err != nil {
		t.Fatalf("ghi entry: %v", err)
	}
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open lần 2 (DB đã có cột topic): %v", err)
	}
	defer db2.Close()

	count, err := db2.DiaryCountForTopic("projects")
	if err != nil {
		t.Fatalf("DiaryCountForTopic: %v", err)
	}
	if count != 1 {
		t.Errorf("mong đợi 1 entry topic projects, được %d", count)
	}
}

func TestOpen_DurableMemorySchemaIsPresentAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open first time: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close first time: %v", err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatalf("Open second time: %v", err)
	}
	defer db.Close()

	rows, err := db.sql.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name IN ('memory_jobs', 'embedding_queue', 'memory_embeddings')")
	if err != nil {
		t.Fatalf("query durable schema: %v", err)
	}
	defer rows.Close()
	var tables int
	for rows.Next() {
		tables++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("scan durable schema: %v", err)
	}
	if tables != 3 {
		t.Fatalf("durable tables = %d, want 3", tables)
	}
}

// TestUpdateSettings_WritesAllKeysAtomically ghi nhiều key cùng lúc rồi đọc lại — phải khớp đúng
// từng key, và key cũ không liên quan không bị đụng vào.
func TestUpdateSettings_WritesAllKeysAtomically(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.SetSetting("untouched", "still-here"); err != nil {
		t.Fatalf("seed untouched: %v", err)
	}

	if err := db.UpdateSettings(map[string]string{
		"proactive_plan":       `[{"at":"2026-08-25T21:00:00Z"}]`,
		"proactive_chat_id":    "555",
		"proactive_unanswered": "2",
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	for key, want := range map[string]string{
		"proactive_plan":       `[{"at":"2026-08-25T21:00:00Z"}]`,
		"proactive_chat_id":    "555",
		"proactive_unanswered": "2",
		"untouched":            "still-here",
	} {
		got, ok, err := db.GetSetting(key)
		if err != nil {
			t.Fatalf("GetSetting(%s): %v", key, err)
		}
		if !ok || got != want {
			t.Errorf("GetSetting(%s) = (%q, %v), want (%q, true)", key, got, ok, want)
		}
	}
}

// TestUpdateSettings_EmptyMapIsNoop gọi với map rỗng không được lỗi, không được đụng gì.
func TestUpdateSettings_EmptyMapIsNoop(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.UpdateSettings(nil); err != nil {
		t.Fatalf("UpdateSettings(nil) phải no-op, được lỗi: %v", err)
	}
	if err := db.UpdateSettings(map[string]string{}); err != nil {
		t.Fatalf("UpdateSettings({}) phải no-op, được lỗi: %v", err)
	}
}
