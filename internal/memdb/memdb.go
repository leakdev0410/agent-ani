// Package memdb là lớp lưu trữ SQLite thay cho việc đọc/ghi trực tiếp các file .md trong
// ani-memory/. Mỗi khối trước đây là 1 file riêng (memory.md, memory/log.md, memory/personal.md,
// memory/preferences.md, memory/projects.md, skills/*/SKILL.md) giờ là 1 blob text trong bảng
// memory_files, khoá bằng 1 key ngữ nghĩa (VD "core/memory", "topic/log", "skills/ani-telegram")
// — KHÔNG còn mang đuôi ".md" hay hình dạng đường dẫn file, để không ai lầm tưởng đây vẫn là
// đọc/ghi trực tiếp trên đĩa (nó không phải — mọi thứ nằm trong memory.db).
package memdb

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Các key cố định trong bảng memory_files — ngữ nghĩa, không phải đường dẫn file. Dùng hằng số
// thay vì chuỗi rải rác khắp nơi để đổi tên 1 chỗ là đổi khắp code.
const (
	KeyCoreMemory  = "core/memory"  // trí nhớ cốt lõi + trạng thái hiện tại (working memory)
	KeySkillsIndex = "skills/index" // mục lục skills (luôn nạp, nhẹ)
)

// TopicKey dựng key cho 1 chủ đề trong memory/ cũ (VD "log", "personal", "preferences",
// "projects") → "topic/log", "topic/personal"...
func TopicKey(name string) string { return "topic/" + name }

// SkillKey dựng key cho nội dung đầy đủ 1 skill (VD "ani-telegram") → "skills/ani-telegram".
func SkillKey(name string) string { return "skills/" + name }

// topicPrefix và skillPrefix dùng nội bộ để liệt kê (ListUnder) và tách tên ngắn từ key.
const (
	topicPrefix = "topic/"
	skillPrefix = "skills/"
)

type DB struct {
	sql                                     *sql.DB
	verbose                                 bool
	logger                                  *log.Logger
	memoryJobCommitErrorForTest             func(*sql.Tx) error
	memoryJobCaptureExtractionErrorForTest  func() error
	memoryJobFinalizeExtractionErrorForTest func() error
}

// Open mở (tạo nếu chưa có) SQLite tại path và đảm bảo schema tồn tại.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1) // 1 tiến trình ghi tuần tự, giống hệt cách main.go dùng file trước đây

	schema := `
	-- Văn xuôi tĩnh (quy tắc, hướng dẫn viết tay, ít khi đổi) — key ngữ nghĩa, không phải path.
	-- "core/memory" giờ là TEMPLATE (có {{PLACEHOLDER}}), không còn là blob chứa hết mọi thứ.
	CREATE TABLE IF NOT EXISTS memory_files (
		name TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	-- Trạng thái "sống" — mỗi field 1 cột, thay cho việc dò prefix trong text.
	CREATE TABLE IF NOT EXISTS core_state (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		last_message_note TEXT NOT NULL DEFAULT '',
		sleep_note TEXT NOT NULL DEFAULT '',
		emotional_state TEXT NOT NULL DEFAULT '',
		mang1 TEXT NOT NULL DEFAULT '',
		mang2 TEXT NOT NULL DEFAULT '',
		mang3 TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	);

	-- Mỗi bullet "Dấu ấn cảm xúc" = 1 row, thay cho chuỗi markdown nhúng trong core/memory.
	-- topic = chủ đề dài hạn gắn kèm (projects/personal/preferences/log), rỗng = không gắn —
	-- để recall_topic nạp được dấu ấn theo chủ đề thay vì chỉ theo ngày.
	CREATE TABLE IF NOT EXISTS diary_entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		entry_date TEXT NOT NULL,
		text TEXT NOT NULL,
		topic TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_diary_entries_date ON diary_entries(entry_date);

	-- Mỗi ghi nhận văn phong/sở thích = 1 row. kind: 'tone' | 'preference'.
	CREATE TABLE IF NOT EXISTS observations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		text TEXT NOT NULL,
		observed_at TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_observations_kind ON observations(kind);

	-- Ghi chú tự động MỚI vào chủ đề (projects/personal/preferences/log) — mỗi note = 1 row,
	-- thay cho việc chèn text dưới heading "## Ghi chú tự động từ Telegram". Nội dung viết tay/
	-- cũ hơn của mỗi chủ đề vẫn giữ dạng blob trong memory_files (key topic/<tên>).
	CREATE TABLE IF NOT EXISTS topic_notes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		topic TEXT NOT NULL,
		text TEXT NOT NULL,
		noted_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_topic_notes_topic ON topic_notes(topic);

	-- Cấu hình nhỏ, đổi lúc chạy (VD "current_model" ghi bởi lệnh Telegram /model) — key-value
	-- đơn giản, thay cho việc thêm cột riêng vào core_state cho từng thứ vặt vãnh.
	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	-- Chỉ mục tìm kiếm toàn văn cho auto-recall. source_ref giữ định danh row/chunk gốc để
	-- kết quả có thể giải thích được; các cột metadata không tham gia token hoá.
	CREATE VIRTUAL TABLE IF NOT EXISTS memory_search USING fts5(
		source_type UNINDEXED,
		source_ref UNINDEXED,
		topic UNINDEXED,
		text,
		tokenize = 'unicode61 remove_diacritics 2'
	);

	`
	if _, err := sqlDB.Exec(schema); err != nil {
		return nil, err
	}
	// Migration cho DB cũ: bảng diary_entries tạo từ trước tính năng "nhớ theo chủ đề" chưa có
	// cột topic. CREATE TABLE IF NOT EXISTS không thêm cột vào bảng đã tồn tại, nên phải ALTER
	// có điều kiện ở đây; index theo topic cũng phải đợi sau khi cột chắc chắn tồn tại.
	if err := ensureDiaryTopicColumn(sqlDB); err != nil {
		return nil, err
	}
	if err := ensureDurableMemorySchema(sqlDB); err != nil {
		return nil, err
	}
	if err := discardLegacyBlockedMemoryJobs(sqlDB); err != nil {
		return nil, err
	}
	if err := ensureSearchTriggers(sqlDB); err != nil {
		return nil, err
	}
	// Logger riêng, ghi thẳng ra stdout — KHÔNG dùng logger mặc định của package "log", vì
	// main.go tắt tiếng logger mặc định (log.SetOutput(io.Discard)) khi không có cờ -log. Nhờ
	// vậy -logdb bật lên vẫn thấy log dù -log không bật, và ngược lại.
	return &DB{sql: sqlDB, logger: log.New(os.Stdout, "", log.LstdFlags)}, nil
}

// ensureSearchTriggers chạy sau migration cột diary.topic để DB cũ không bị lỗi khi SQLite kiểm
// tra NEW.topic lúc tạo trigger.
func ensureSearchTriggers(sqlDB *sql.DB) error {
	const triggers = `
	CREATE TRIGGER IF NOT EXISTS diary_search_insert AFTER INSERT ON diary_entries BEGIN
		INSERT INTO memory_search(source_type, source_ref, topic, text)
		VALUES ('diary', NEW.id || ':' || NEW.entry_date, NEW.topic, NEW.text);
	END;
	CREATE TRIGGER IF NOT EXISTS diary_search_update AFTER UPDATE ON diary_entries BEGIN
		DELETE FROM memory_search WHERE source_type = 'diary' AND source_ref = OLD.id || ':' || OLD.entry_date;
		INSERT INTO memory_search(source_type, source_ref, topic, text)
		VALUES ('diary', NEW.id || ':' || NEW.entry_date, NEW.topic, NEW.text);
	END;
	CREATE TRIGGER IF NOT EXISTS diary_search_delete AFTER DELETE ON diary_entries BEGIN
		DELETE FROM memory_search WHERE source_type = 'diary' AND source_ref = OLD.id || ':' || OLD.entry_date;
	END;

	CREATE TRIGGER IF NOT EXISTS observation_search_insert AFTER INSERT ON observations BEGIN
		INSERT INTO memory_search(source_type, source_ref, topic, text)
		VALUES ('observation', NEW.kind || ':' || NEW.id, NEW.kind, NEW.text);
	END;
	CREATE TRIGGER IF NOT EXISTS observation_search_update AFTER UPDATE ON observations BEGIN
		DELETE FROM memory_search WHERE source_type = 'observation' AND source_ref = OLD.kind || ':' || OLD.id;
		INSERT INTO memory_search(source_type, source_ref, topic, text)
		VALUES ('observation', NEW.kind || ':' || NEW.id, NEW.kind, NEW.text);
	END;
	CREATE TRIGGER IF NOT EXISTS observation_search_delete AFTER DELETE ON observations BEGIN
		DELETE FROM memory_search WHERE source_type = 'observation' AND source_ref = OLD.kind || ':' || OLD.id;
	END;

	CREATE TRIGGER IF NOT EXISTS topic_note_search_insert AFTER INSERT ON topic_notes BEGIN
		INSERT INTO memory_search(source_type, source_ref, topic, text)
		VALUES ('topic_note', NEW.id || ':' || NEW.noted_at, NEW.topic, NEW.text);
	END;
	CREATE TRIGGER IF NOT EXISTS topic_note_search_update AFTER UPDATE ON topic_notes BEGIN
		DELETE FROM memory_search WHERE source_type = 'topic_note' AND source_ref = OLD.id || ':' || OLD.noted_at;
		INSERT INTO memory_search(source_type, source_ref, topic, text)
		VALUES ('topic_note', NEW.id || ':' || NEW.noted_at, NEW.topic, NEW.text);
	END;
	CREATE TRIGGER IF NOT EXISTS topic_note_search_delete AFTER DELETE ON topic_notes BEGIN
		DELETE FROM memory_search WHERE source_type = 'topic_note' AND source_ref = OLD.id || ':' || OLD.noted_at;
	END;`
	if _, err := sqlDB.Exec(triggers); err != nil {
		return fmt.Errorf("memdb: tạo trigger đồng bộ tìm kiếm lỗi: %w", err)
	}
	return nil
}

// ensureDiaryTopicColumn thêm cột topic vào diary_entries nếu DB cũ chưa có (ALTER TABLE ADD
// COLUMN trong SQLite là an toàn, giữ nguyên dữ liệu cũ), rồi tạo index theo topic. Hàm tách
// riêng để có thể test migration trên bản DB cũ.
func ensureDiaryTopicColumn(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query("PRAGMA table_info(diary_entries)")
	if err != nil {
		return fmt.Errorf("memdb: đọc cấu trúc diary_entries lỗi: %w", err)
	}
	hasTopic := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dflt, pk any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("memdb: quét cấu trúc diary_entries lỗi: %w", err)
		}
		if name == "topic" {
			hasTopic = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("memdb: đọc cấu trúc diary_entries lỗi: %w", err)
	}

	if !hasTopic {
		if _, err := sqlDB.Exec("ALTER TABLE diary_entries ADD COLUMN topic TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("memdb: thêm cột topic vào diary_entries lỗi: %w", err)
		}
	}
	if _, err := sqlDB.Exec("CREATE INDEX IF NOT EXISTS idx_diary_entries_topic ON diary_entries(topic)"); err != nil {
		return fmt.Errorf("memdb: tạo index diary_entries(topic) lỗi: %w", err)
	}
	return nil
}

// SetVerbose bật/tắt log chi tiết mỗi lần đọc/ghi memory DB (tên file, kích thước, và nơi
// trong code gọi tới) — dùng cho cờ -logdb.
func (db *DB) SetVerbose(v bool) {
	db.verbose = v
}

func (db *DB) trace(op, name string, size int) {
	if !db.verbose {
		return
	}
	db.logger.Printf("[memory] %-7s %-20s %-40s %6d bytes  (gọi từ %s)",
		op, name, friendlyLabel(name), size, callerLocation())
}

// Note in 1 dòng log ngữ nghĩa tự do (không gắn với 1 key cụ thể) — dùng cho các mốc như "đang
// load tính cách gốc", "model gọi tool load_skill(...)" — cùng kênh với trace() (chỉ hiện khi
// -logdb bật, cùng logger riêng không phụ thuộc -log).
func (db *DB) Note(format string, args ...any) {
	if !db.verbose {
		return
	}
	// skip=2: 0=callerLocationSkip, 1=Note, 2=nơi gọi Note thực sự.
	db.logger.Printf("[memory] %s  (gọi từ %s)", fmt.Sprintf(format, args...), callerLocationSkip(2))
}

// friendlyLabel dịch 1 key kỹ thuật sang mô tả ngữ nghĩa dễ đọc trong log — thuần cosmetic,
// không ảnh hưởng logic đọc/ghi.
func friendlyLabel(name string) string {
	switch {
	case name == KeyCoreMemory:
		return "trí nhớ cốt lõi + trạng thái (working memory)"
	case name == KeySkillsIndex:
		return "mục lục skills"
	case strings.HasPrefix(name, topicPrefix):
		return "chủ đề: " + strings.TrimPrefix(name, topicPrefix)
	case strings.HasPrefix(name, skillPrefix):
		return "skill: " + strings.TrimPrefix(name, skillPrefix)
	default:
		return name
	}
}

// callerLocation trả về "file:dòng" của nơi ĐÃ GỌI hàm Read/Write/Exists (không phải trace tự
// nó) — skip=3: 0=callerLocationSkip, 1=trace, 2=Read/Write/Exists, 3=nơi gọi thực sự.
func callerLocation() string {
	return callerLocationSkip(3)
}

// callerLocationSkip trả về "file:dòng" đi lên skip khung ngăn xếp kể từ chính nó — xem
// runtime.Caller: skip=0 là chính callerLocationSkip, skip=1 là nơi gọi callerLocationSkip.
func callerLocationSkip(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "?"
	}
	if idx := strings.Index(file, "ani-telegram"); idx >= 0 {
		file = file[idx:]
	}
	return fmt.Sprintf("%s:%d", file, line)
}

func (db *DB) Close() error {
	return db.sql.Close()
}

// InTx runs fn in one transaction without exposing the underlying sql.DB.
func (db *DB) InTx(fn func(*sql.Tx) error) error {
	if fn == nil {
		return fmt.Errorf("memdb: transaction callback is nil")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("memdb: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memdb: commit transaction: %w", err)
	}
	return nil
}

// Read trả về nội dung đã lưu cho name, hoặc "" nếu chưa từng ghi (không phải
// lỗi — giống os.ReadFile khi file chưa tồn tại).
func (db *DB) Read(name string) (string, error) {
	var content string
	err := db.sql.QueryRow("SELECT content FROM memory_files WHERE name = ?", name).Scan(&content)
	if err == sql.ErrNoRows {
		db.trace("READ", name, 0)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("memdb: đọc %s lỗi: %w", name, err)
	}
	db.trace("READ", name, len(content))
	return content, nil
}

// Exists báo name đã từng được ghi chưa — dùng để phân biệt "chưa có" với "có nhưng rỗng".
func (db *DB) Exists(name string) (bool, error) {
	var n int
	err := db.sql.QueryRow("SELECT 1 FROM memory_files WHERE name = ?", name).Scan(&n)
	if err == sql.ErrNoRows {
		db.trace("EXISTS?", name, 0)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("memdb: kiểm tra %s lỗi: %w", name, err)
	}
	db.trace("EXISTS?", name, 1)
	return true, nil
}

// Write upsert nội dung cho name.
func (db *DB) Write(name, content string) error {
	_, err := db.sql.Exec(
		`INSERT INTO memory_files (name, content, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET content = excluded.content, updated_at = excluded.updated_at`,
		name, content, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("memdb: ghi %s lỗi: %w", name, err)
	}
	db.trace("WRITE", name, len(content))
	return nil
}

// ListUnder trả về tên (đã sắp xếp) của mọi file đã lưu có tiền tố prefix,
// giống os.ReadDir(topicDir) + lọc *.md + sort trước đây trong persona.go.
func (db *DB) ListUnder(prefix string) ([]string, error) {
	rows, err := db.sql.Query("SELECT name FROM memory_files WHERE name LIKE ? ORDER BY name", prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("memdb: liệt kê %s* lỗi: %w", prefix, err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, rows.Err()
}

// IsSeeded báo DB đã từng được nạp trí nhớ cốt lõi chưa — dùng để quyết định có chạy import 1
// lần từ .md cũ hay không (tránh ghi đè dữ liệu mới hơn trong DB mỗi lần khởi động lại bot).
func (db *DB) IsSeeded() (bool, error) {
	return db.Exists(KeyCoreMemory)
}

// HasSkills báo skills đã được nạp vào DB chưa — tách riêng khỏi IsSeeded vì skills được thêm
// sau, để bot cũ (đã seed memory core từ trước) vẫn tự nạp bổ sung skills ở lần khởi động tiếp
// theo mà không phải xoá/seed lại từ đầu.
func (db *DB) HasSkills() (bool, error) {
	return db.Exists(KeySkillsIndex)
}

// SkillNames trả về tên ngắn của mọi skill đã lưu trong DB, đã sắp xếp — dùng để mô tả tham số
// hợp lệ cho tool load_skill.
func (db *DB) SkillNames() ([]string, error) {
	names, err := db.ListUnder(skillPrefix)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, name := range names {
		if name == KeySkillsIndex {
			continue
		}
		result = append(result, strings.TrimPrefix(name, skillPrefix))
	}
	return result, nil
}

// TopicNames trả về tên ngắn của mọi chủ đề (topic/*), đã sắp xếp.
func (db *DB) TopicNames() ([]string, error) {
	names, err := db.ListUnder(topicPrefix)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, name := range names {
		short := strings.TrimPrefix(name, topicPrefix)
		if short == "" || short == name {
			continue
		}
		result = append(result, short)
	}
	return result, nil
}
