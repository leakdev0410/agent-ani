package memdb

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ensureDurableMemorySchema applies only additive tables and indexes so an
// existing memory.db gains the job journal and embedding queue without
// rewriting or dropping any committed memory.
func ensureDurableMemorySchema(sqlDB *sql.DB) error {
	const schema = `
		CREATE TABLE IF NOT EXISTS memory_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			state TEXT NOT NULL CHECK (state IN ('pending_extraction', 'ready_to_apply', 'blocked')),
			payload BLOB NOT NULL,
			extraction_json TEXT NOT NULL DEFAULT '',
			extraction_sha256 TEXT NOT NULL DEFAULT '',
			failure_code TEXT NOT NULL DEFAULT '',
			delivery_pending INTEGER NOT NULL DEFAULT 0,
			delivery_sequence INTEGER NOT NULL DEFAULT 0,
			delivery_inflight INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_memory_jobs_state_id ON memory_jobs(state, id);

		CREATE TABLE IF NOT EXISTS embedding_queue (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_key TEXT NOT NULL UNIQUE,
			content_sha256 TEXT NOT NULL,
			text TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_embedding_queue_id ON embedding_queue(id);

		CREATE TABLE IF NOT EXISTS memory_embeddings (
			source_key TEXT PRIMARY KEY,
			content_sha256 TEXT NOT NULL,
			text TEXT NOT NULL,
			model TEXT NOT NULL,
			vector BLOB NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_memory_embeddings_model ON memory_embeddings(model);
	`
	if _, err := sqlDB.Exec(schema); err != nil {
		return fmt.Errorf("memdb: migrate durable memory schema lá»—i: %w", err)
	}
	return ensureMemoryDeliveryColumns(sqlDB)
}

func ensureMemoryDeliveryColumns(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query("PRAGMA table_info(memory_jobs)")
	if err != nil {
		return fmt.Errorf("memdb: inspect memory_jobs delivery columns: %w", err)
	}
	present := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var defaultValue, primaryKey any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("memdb: scan memory_jobs delivery columns: %w", err)
		}
		present[name] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("memdb: close memory_jobs schema rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("memdb: inspect memory_jobs delivery columns: %w", err)
	}
	columns := []struct{ name, statement string }{
		{"delivery_pending", "ALTER TABLE memory_jobs ADD COLUMN delivery_pending INTEGER NOT NULL DEFAULT 0"},
		{"delivery_sequence", "ALTER TABLE memory_jobs ADD COLUMN delivery_sequence INTEGER NOT NULL DEFAULT 0"},
		{"delivery_inflight", "ALTER TABLE memory_jobs ADD COLUMN delivery_inflight INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		if !present[column.name] {
			if _, err := sqlDB.Exec(column.statement); err != nil {
				return fmt.Errorf("memdb: add memory_jobs column %s: %w", column.name, err)
			}
		}
	}
	return nil
}

// LegacyCoreMigration là toàn bộ dữ liệu đã tách từ blob core/memory cũ. Nó nằm ở package
// memdb để việc ghi template + state + diary + observations có thể commit nguyên tử.
type LegacyCoreMigration struct {
	Template     string
	State        CoreState
	DiaryEntries []DiaryEntry
	Observations []SeedObservation
}

type SeedObservation struct {
	Kind       string
	Text       string
	ObservedAt string
}

// ApplyLegacyCoreMigration ghi migration đúng một lần. core_state(id=1) là migration gate; toàn
// bộ thao tác nằm trong một transaction để lỗi giữa chừng không tạo diary/observation trùng khi
// khởi động lại.
func (db *DB) ApplyLegacyCoreMigration(m LegacyCoreMigration) (bool, error) {
	if strings.TrimSpace(m.Template) == "" {
		return false, fmt.Errorf("memdb: template sau migration rỗng")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("memdb: bắt đầu migration core lỗi: %w", err)
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRow("SELECT 1 FROM core_state WHERE id = 1").Scan(&exists)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("memdb: kiểm tra migration core lỗi: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := tx.Exec(
		"UPDATE memory_files SET content = ?, updated_at = ? WHERE name = ?",
		m.Template, now, KeyCoreMemory,
	)
	if err != nil {
		return false, fmt.Errorf("memdb: ghi core template lỗi: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return false, fmt.Errorf("memdb: kiểm tra core template lỗi: %w", err)
		}
		return false, fmt.Errorf("memdb: không tìm thấy %s để migrate", KeyCoreMemory)
	}

	_, err = tx.Exec(
		`INSERT INTO core_state
		 (id, last_message_note, sleep_note, emotional_state, mang1, mang2, mang3, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?)`,
		m.State.LastMessageNote, m.State.SleepNote, m.State.EmotionalState,
		m.State.Mang1, m.State.Mang2, m.State.Mang3, now,
	)
	if err != nil {
		return false, fmt.Errorf("memdb: ghi core_state khi migrate lỗi: %w", err)
	}
	for _, entry := range m.DiaryEntries {
		if _, err := tx.Exec(
			"INSERT INTO diary_entries (entry_date, text, topic, created_at) VALUES (?, ?, '', ?)",
			entry.EntryDate, entry.Text, now,
		); err != nil {
			return false, fmt.Errorf("memdb: migrate diary %s lỗi: %w", entry.EntryDate, err)
		}
	}
	for _, observation := range m.Observations {
		if observation.Kind != ObservationTone && observation.Kind != ObservationPreference {
			return false, fmt.Errorf("memdb: observation kind không hợp lệ: %q", observation.Kind)
		}
		if _, err := tx.Exec(
			"INSERT INTO observations (kind, text, observed_at, created_at) VALUES (?, ?, ?, ?)",
			observation.Kind, observation.Text, observation.ObservedAt, now,
		); err != nil {
			return false, fmt.Errorf("memdb: migrate observation %s lỗi: %w", observation.Kind, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("memdb: commit migration core lỗi: %w", err)
	}
	db.trace("MIGRATE", KeyCoreMemory, len(m.Template))
	return true, nil
}
