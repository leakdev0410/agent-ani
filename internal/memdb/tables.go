package memdb

import (
	"database/sql"
	"fmt"
	"time"
)

// ─── core_state ────────────────────────────────────────────────────────────

// CoreState là trạng thái "sống" của Ani — trước đây là các dòng rải rác trong core/memory,
// giờ là cột thật trong bảng core_state.
type CoreState struct {
	LastMessageNote string
	SleepNote       string
	EmotionalState  string
	Mang1           string
	Mang2           string
	Mang3           string
}

// GetCoreState đọc core_state (id=1) — trả về CoreState rỗng nếu chưa từng ghi (chưa seed).
func (db *DB) GetCoreState() (CoreState, error) {
	var s CoreState
	err := db.sql.QueryRow(
		`SELECT last_message_note, sleep_note, emotional_state, mang1, mang2, mang3
		 FROM core_state WHERE id = 1`,
	).Scan(&s.LastMessageNote, &s.SleepNote, &s.EmotionalState, &s.Mang1, &s.Mang2, &s.Mang3)
	if err == sql.ErrNoRows {
		db.trace("READ", "core_state", 0)
		return CoreState{}, nil
	}
	if err != nil {
		return CoreState{}, fmt.Errorf("memdb: đọc core_state lỗi: %w", err)
	}
	db.trace("READ", "core_state", len(s.LastMessageNote)+len(s.EmotionalState)+len(s.Mang1)+len(s.Mang2)+len(s.Mang3))
	return s, nil
}

// SaveCoreState upsert toàn bộ core_state (id=1) trong 1 lần — thay cho việc dò prefix rồi
// thay dòng trong text.
func (db *DB) SaveCoreState(s CoreState) error {
	_, err := db.sql.Exec(
		`INSERT INTO core_state (id, last_message_note, sleep_note, emotional_state, mang1, mang2, mang3, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   last_message_note = excluded.last_message_note,
		   sleep_note         = excluded.sleep_note,
		   emotional_state    = excluded.emotional_state,
		   mang1              = excluded.mang1,
		   mang2              = excluded.mang2,
		   mang3              = excluded.mang3,
		   updated_at         = excluded.updated_at`,
		s.LastMessageNote, s.SleepNote, s.EmotionalState, s.Mang1, s.Mang2, s.Mang3,
		time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("memdb: ghi core_state lỗi: %w", err)
	}
	db.trace("WRITE", "core_state", len(s.LastMessageNote)+len(s.EmotionalState)+len(s.Mang1)+len(s.Mang2)+len(s.Mang3))
	return nil
}

// HasCoreState báo core_state đã từng được ghi chưa — dùng để quyết định seed lần đầu.
func (db *DB) HasCoreState() (bool, error) {
	var n int
	err := db.sql.QueryRow("SELECT 1 FROM core_state WHERE id = 1").Scan(&n)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("memdb: kiểm tra core_state lỗi: %w", err)
	}
	return true, nil
}

// ─── diary_entries ─────────────────────────────────────────────────────────

// DiaryEntry là 1 bullet trong "Dấu ấn cảm xúc".
type DiaryEntry struct {
	EntryDate string // "YYYY-MM-DD"
	Text      string // "HH:MM: nội dung", nguyên văn không có dấu "- " ở đầu
}

// AddDiaryEntry thêm 1 bullet mới — INSERT đơn giản, không cần dò heading/vị trí trong text nữa.
// topic rỗng nghĩa là không gắn chủ đề (giữ nguyên như trước khi có tính năng gọi nhớ theo chủ đề).
func (db *DB) AddDiaryEntry(entryDate, text, topic string) error {
	_, err := db.sql.Exec(
		"INSERT INTO diary_entries (entry_date, text, topic, created_at) VALUES (?, ?, ?, ?)",
		entryDate, text, topic, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("memdb: ghi diary_entries lỗi: %w", err)
	}
	db.trace("WRITE", "diary_entries", len(text))
	return nil
}

// DiaryTopicEntry là 1 bullet nhật ký có kèm ngày + ID — dùng khi lọc theo chủ đề (recall theo
// chủ đề) và khi backfill topic cho dữ liệu cũ.
type DiaryTopicEntry struct {
	ID        int64
	EntryDate string
	Text      string
}

// DiaryEntriesForTopic trả về tối đa limit bullet gắn chủ đề topic (limit <= 0 = không giới hạn),
// ngày mới nhất trước, trong cùng ngày theo đúng thứ tự đã ghi (id ASC).
func (db *DB) DiaryEntriesForTopic(topic string, limit int) ([]DiaryTopicEntry, error) {
	query := `SELECT id, entry_date, text FROM diary_entries WHERE topic = ?
	          ORDER BY entry_date DESC, id ASC`
	args := []any{topic}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("memdb: đọc diary_entries(topic=%s) lỗi: %w", topic, err)
	}
	defer rows.Close()
	var out []DiaryTopicEntry
	size := 0
	for rows.Next() {
		var e DiaryTopicEntry
		if err := rows.Scan(&e.ID, &e.EntryDate, &e.Text); err != nil {
			return nil, err
		}
		out = append(out, e)
		size += len(e.Text)
	}
	db.trace("READ", "diary_entries(topic="+topic+")", size)
	return out, rows.Err()
}

// DiaryCountForTopic đếm tổng bullet gắn chủ đề topic — dùng cho hint "còn N dấu ấn" khi
// recall_topic phải cắt bớt, và cho mục lục chủ đề trong system prompt.
func (db *DB) DiaryCountForTopic(topic string) (int, error) {
	var n int
	err := db.sql.QueryRow("SELECT COUNT(*) FROM diary_entries WHERE topic = ?", topic).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("memdb: đếm diary_entries(topic=%s) lỗi: %w", topic, err)
	}
	db.trace("READ", "diary_entries(topic="+topic+",count)", n)
	return n, nil
}

// DiaryEntriesWithoutTopic trả về mọi bullet chưa gắn chủ đề (topic rỗng) theo đúng thứ tự đã
// ghi — dùng cho backfill 1 lần bằng model (flag -backfill-topics).
func (db *DB) DiaryEntriesWithoutTopic() ([]DiaryTopicEntry, error) {
	rows, err := db.sql.Query(
		"SELECT id, entry_date, text FROM diary_entries WHERE topic = '' ORDER BY id ASC")
	if err != nil {
		return nil, fmt.Errorf("memdb: đọc diary_entries chưa tag lỗi: %w", err)
	}
	defer rows.Close()
	var out []DiaryTopicEntry
	size := 0
	for rows.Next() {
		var e DiaryTopicEntry
		if err := rows.Scan(&e.ID, &e.EntryDate, &e.Text); err != nil {
			return nil, err
		}
		out = append(out, e)
		size += len(e.Text)
	}
	db.trace("READ", "diary_entries(chưa tag)", size)
	return out, rows.Err()
}

// UpdateDiaryTopic gán chủ đề cho 1 bullet đã có — dùng cho backfill.
func (db *DB) UpdateDiaryTopic(id int64, topic string) error {
	_, err := db.sql.Exec("UPDATE diary_entries SET topic = ? WHERE id = ?", topic, id)
	if err != nil {
		return fmt.Errorf("memdb: gán topic cho diary_entries(%d) lỗi: %w", id, err)
	}
	db.trace("WRITE", fmt.Sprintf("diary_entries(%d).topic", id), len(topic))
	return nil
}

// DiaryTopicCounts đếm bullet theo từng chủ đề đã gắn (bỏ qua topic rỗng) — dùng cho mục lục
// chủ đề trong system prompt để model biết chủ đề nào có dấu ấn cảm xúc đáng gọi recall_topic.
func (db *DB) DiaryTopicCounts() (map[string]int, error) {
	rows, err := db.sql.Query("SELECT topic, COUNT(*) FROM diary_entries WHERE topic != '' GROUP BY topic")
	if err != nil {
		return nil, fmt.Errorf("memdb: đếm diary theo chủ đề lỗi: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var topic string
		var n int
		if err := rows.Scan(&topic, &n); err != nil {
			return nil, err
		}
		counts[topic] = n
	}
	db.trace("READ", "diary_entries(theo chủ đề)", len(counts))
	return counts, rows.Err()
}

// DiaryDates trả về mọi ngày có entry, mới nhất trước (DISTINCT, ORDER BY DESC).
func (db *DB) DiaryDates() ([]string, error) {
	rows, err := db.sql.Query("SELECT DISTINCT entry_date FROM diary_entries ORDER BY entry_date DESC")
	if err != nil {
		return nil, fmt.Errorf("memdb: liệt kê diary_dates lỗi: %w", err)
	}
	defer rows.Close()
	var dates []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		dates = append(dates, d)
	}
	db.trace("READ", "diary_entries(dates)", len(dates))
	return dates, rows.Err()
}

// DiaryEntriesForDate trả về mọi entry của đúng 1 ngày, theo đúng thứ tự đã ghi (id ASC — thứ
// tự thời gian trong ngày).
func (db *DB) DiaryEntriesForDate(date string) ([]string, error) {
	rows, err := db.sql.Query("SELECT text FROM diary_entries WHERE entry_date = ? ORDER BY id ASC", date)
	if err != nil {
		return nil, fmt.Errorf("memdb: đọc diary_entries(%s) lỗi: %w", date, err)
	}
	defer rows.Close()
	var texts []string
	size := 0
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		texts = append(texts, t)
		size += len(t)
	}
	db.trace("READ", "diary_entries("+date+")", size)
	return texts, rows.Err()
}

// ─── observations ──────────────────────────────────────────────────────────

const (
	ObservationTone       = "tone"
	ObservationPreference = "preference"
)

// AddObservation thêm 1 ghi nhận văn phong/sở thích mới. observedAt có thể rỗng nếu không rõ
// ngày (một số bullet lịch sử không có tag ngày).
func (db *DB) AddObservation(kind, text, observedAt string) error {
	_, err := db.sql.Exec(
		"INSERT INTO observations (kind, text, observed_at, created_at) VALUES (?, ?, ?, ?)",
		kind, text, observedAt, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("memdb: ghi observations lỗi: %w", err)
	}
	db.trace("WRITE", "observations:"+kind, len(text))
	return nil
}

// Observation là 1 ghi nhận văn phong/sở thích đã lưu.
type Observation struct {
	Text       string
	ObservedAt string
}

// Observations trả về mọi ghi nhận của 1 kind, theo đúng thứ tự đã ghi (id ASC).
func (db *DB) Observations(kind string) ([]Observation, error) {
	rows, err := db.sql.Query("SELECT text, observed_at FROM observations WHERE kind = ? ORDER BY id ASC", kind)
	if err != nil {
		return nil, fmt.Errorf("memdb: đọc observations(%s) lỗi: %w", kind, err)
	}
	defer rows.Close()
	var out []Observation
	size := 0
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.Text, &o.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
		size += len(o.Text)
	}
	db.trace("READ", "observations:"+kind, size)
	return out, rows.Err()
}

// ─── topic_notes ───────────────────────────────────────────────────────────

// AddTopicNote thêm 1 ghi chú tự động mới vào chủ đề topic (VD "projects", "log").
func (db *DB) AddTopicNote(topic, text, notedAt string) error {
	_, err := db.sql.Exec(
		"INSERT INTO topic_notes (topic, text, noted_at) VALUES (?, ?, ?)",
		topic, text, notedAt,
	)
	if err != nil {
		return fmt.Errorf("memdb: ghi topic_notes(%s) lỗi: %w", topic, err)
	}
	db.trace("WRITE", "topic_notes:"+topic, len(text))
	return nil
}

// TopicNote là 1 ghi chú tự động đã lưu cho 1 chủ đề.
type TopicNote struct {
	Text    string
	NotedAt string
}

// TopicNotes trả về mọi ghi chú tự động của 1 chủ đề, theo đúng thứ tự đã ghi (id ASC).
func (db *DB) TopicNotes(topic string) ([]TopicNote, error) {
	rows, err := db.sql.Query("SELECT text, noted_at FROM topic_notes WHERE topic = ? ORDER BY id ASC", topic)
	if err != nil {
		return nil, fmt.Errorf("memdb: đọc topic_notes(%s) lỗi: %w", topic, err)
	}
	defer rows.Close()
	var out []TopicNote
	size := 0
	for rows.Next() {
		var n TopicNote
		if err := rows.Scan(&n.Text, &n.NotedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
		size += len(n.Text)
	}
	db.trace("READ", "topic_notes:"+topic, size)
	return out, rows.Err()
}

// ─── settings ──────────────────────────────────────────────────────────────

// GetSetting đọc 1 giá trị cấu hình theo key (VD "current_model") — ok=false nếu chưa từng ghi.
func (db *DB) GetSetting(key string) (string, bool, error) {
	var value string
	err := db.sql.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		db.trace("READ", "settings:"+key, 0)
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("memdb: đọc settings(%s) lỗi: %w", key, err)
	}
	db.trace("READ", "settings:"+key, len(value))
	return value, true, nil
}

// SetSetting upsert 1 giá trị cấu hình theo key.
func (db *DB) SetSetting(key, value string) error {
	_, err := db.sql.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("memdb: ghi settings(%s) lỗi: %w", key, err)
	}
	db.trace("WRITE", "settings:"+key, len(value))
	return nil
}

// UpdateSettings upsert nhiều key/value trong 1 transaction — dùng khi vài key phải cùng đổi
// một lúc (VD kế hoạch chủ động nhắn: plan + chat_id + unanswered), tránh trạng thái nửa vời nếu
// tiến trình chết giữa chừng lúc ghi từng key riêng lẻ.
func (db *DB) UpdateSettings(changes map[string]string) error {
	if len(changes) == 0 {
		return nil
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("memdb: mở transaction settings lỗi: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	stmt, err := tx.Prepare(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("memdb: chuẩn bị ghi settings lỗi: %w", err)
	}
	defer stmt.Close()

	for key, value := range changes {
		if _, err := stmt.Exec(key, value, now); err != nil {
			return fmt.Errorf("memdb: ghi settings(%s) lỗi: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memdb: commit settings lỗi: %w", err)
	}
	for key, value := range changes {
		db.trace("WRITE", "settings:"+key, len(value))
	}
	return nil
}
