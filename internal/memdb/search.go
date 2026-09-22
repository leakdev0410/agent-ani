package memdb

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	searchChunkRunes   = 420
	searchChunkOverlap = 80
	maxSearchTerms     = 24
)

// SearchResult là một đoạn memory liên quan đã tìm thấy bằng SQLite FTS5.
type SearchResult struct {
	SourceType string
	SourceRef  string
	Topic      string
	Text       string
	OccurredAt string
}

// SearchIndexCount returns how many searchable memory fragments currently exist.
// It is used to avoid advertising recall by search before the database contains
// any indexed memory at all.
func (db *DB) SearchIndexCount() (int, error) {
	var count int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM memory_search").Scan(&count); err != nil {
		return 0, fmt.Errorf("memdb: đếm memory_search lỗi: %w", err)
	}
	db.trace("READ", "memory_search(count)", count)
	return count, nil
}

// RebuildSearchIndex dựng lại toàn bộ chỉ mục từ dữ liệu nguồn. Hàm chạy trong transaction nên
// nếu một nguồn bị lỗi thì chỉ mục cũ không bị bỏ dở ở trạng thái nửa vời.
func (db *DB) RebuildSearchIndex() error {
	return db.rebuildSearchIndexForEmbeddingModel("")
}

// RebuildSearchIndexForEmbeddingModel rebuilds FTS and synchronizes all
// committed sources with the configured embedding model in one transaction.
// If the model changed while the source text did not, old vectors are
// invalidated and exactly one durable retryable queue item is retained.
func (db *DB) RebuildSearchIndexForEmbeddingModel(model string) error {
	return db.rebuildSearchIndexForEmbeddingModel(strings.TrimSpace(model))
}

func (db *DB) rebuildSearchIndexForEmbeddingModel(embeddingModel string) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("memdb: bắt đầu rebuild chỉ mục lỗi: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM memory_search"); err != nil {
		return fmt.Errorf("memdb: xoá chỉ mục cũ lỗi: %w", err)
	}

	var items []SearchResult
	rows, err := tx.Query("SELECT id, entry_date, topic, text FROM diary_entries ORDER BY id")
	if err != nil {
		return fmt.Errorf("memdb: đọc diary để lập chỉ mục lỗi: %w", err)
	}
	for rows.Next() {
		var id int64
		var entryDate, topic, body string
		if err := rows.Scan(&id, &entryDate, &topic, &body); err != nil {
			rows.Close()
			return err
		}
		items = append(items, SearchResult{
			SourceType: "diary",
			SourceRef:  strconv.FormatInt(id, 10) + ":" + entryDate,
			Topic:      topic,
			Text:       body,
		})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = tx.Query("SELECT id, kind, text FROM observations ORDER BY id")
	if err != nil {
		return fmt.Errorf("memdb: đọc observations để lập chỉ mục lỗi: %w", err)
	}
	for rows.Next() {
		var id int64
		var kind, body string
		if err := rows.Scan(&id, &kind, &body); err != nil {
			rows.Close()
			return err
		}
		items = append(items, SearchResult{
			SourceType: "observation",
			SourceRef:  kind + ":" + strconv.FormatInt(id, 10),
			Topic:      kind,
			Text:       body,
		})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = tx.Query("SELECT id, topic, text, noted_at FROM topic_notes ORDER BY id")
	if err != nil {
		return fmt.Errorf("memdb: đọc topic notes để lập chỉ mục lỗi: %w", err)
	}
	for rows.Next() {
		var id int64
		var topic, body, notedAt string
		if err := rows.Scan(&id, &topic, &body, &notedAt); err != nil {
			rows.Close()
			return err
		}
		items = append(items, SearchResult{
			SourceType: "topic_note",
			SourceRef:  strconv.FormatInt(id, 10) + ":" + notedAt,
			Topic:      topic,
			Text:       body,
		})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = tx.Query("SELECT name, content FROM memory_files WHERE name LIKE 'topic/%' ORDER BY name")
	if err != nil {
		return fmt.Errorf("memdb: đọc topic blobs để lập chỉ mục lỗi: %w", err)
	}
	for rows.Next() {
		var name, content string
		if err := rows.Scan(&name, &content); err != nil {
			rows.Close()
			return err
		}
		topic := strings.TrimPrefix(name, topicPrefix)
		for i, chunk := range splitSearchChunks(content) {
			items = append(items, SearchResult{
				SourceType: "topic",
				SourceRef:  name + ":" + strconv.Itoa(i),
				Topic:      topic,
				Text:       chunk,
			})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	stmt, err := tx.Prepare("INSERT INTO memory_search(source_type, source_ref, topic, text) VALUES (?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("memdb: chuẩn bị ghi chỉ mục lỗi: %w", err)
	}
	for _, item := range items {
		if strings.TrimSpace(item.Text) == "" {
			continue
		}
		if _, err := stmt.Exec(item.SourceType, item.SourceRef, item.Topic, item.Text); err != nil {
			stmt.Close()
			return fmt.Errorf("memdb: ghi chỉ mục %s/%s lỗi: %w", item.SourceType, item.SourceRef, err)
		}
	}
	if err := stmt.Close(); err != nil {
		return err
	}
	if err := db.synchronizeEmbeddingSourcesTx(tx, embeddingModel); err != nil {
		return fmt.Errorf("memdb: synchronize embedding sources after rebuild: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memdb: commit chỉ mục tìm kiếm lỗi: %w", err)
	}
	db.trace("REINDEX", "memory_search", len(items))
	return nil
}

// SearchMemory tìm các đoạn liên quan nhất. Query được chuyển thành danh sách token đã quote,
// không đưa nguyên văn người dùng vào cú pháp MATCH nên ký tự đặc biệt không thể làm hỏng FTS.
func (db *DB) SearchMemory(query string, limit int) ([]SearchResult, error) {
	match := searchMatchQuery(query)
	if match == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := db.sql.Query(
		`SELECT memory_search.source_type, memory_search.source_ref, memory_search.topic, memory_search.text,
		        COALESCE(d.entry_date, o.observed_at, n.noted_at, '')
		 FROM memory_search
		 LEFT JOIN diary_entries AS d
		   ON memory_search.source_type = 'diary' AND memory_search.source_ref = d.id || ':' || d.entry_date
		 LEFT JOIN observations AS o
		   ON memory_search.source_type = 'observation' AND memory_search.source_ref = o.kind || ':' || o.id
		 LEFT JOIN topic_notes AS n
		   ON memory_search.source_type = 'topic_note' AND memory_search.source_ref = n.id || ':' || n.noted_at
		 WHERE memory_search MATCH ?
		 ORDER BY bm25(memory_search), memory_search.rowid DESC LIMIT ?`,
		match, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memdb: tìm memory lỗi: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var item SearchResult
		if err := rows.Scan(&item.SourceType, &item.SourceRef, &item.Topic, &item.Text, &item.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	db.trace("SEARCH", "memory_search", len(out))
	return out, rows.Err()
}

func searchMatchQuery(query string) string {
	stopWords := map[string]bool{
		"anh": true, "em": true, "của": true, "cho": true, "với": true, "những": true,
		"này": true, "đó": true, "đang": true, "được": true, "không": true, "một": true,
		"là": true, "và": true, "thì": true, "the": true, "and": true, "that": true,
		"this": true, "you": true, "are": true, "was": true,
	}
	seen := map[string]bool{}
	var tokens []string
	for _, token := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if utf8.RuneCountInString(token) < 2 || stopWords[token] || seen[token] {
			continue
		}
		seen[token] = true
		tokens = append(tokens, `"`+strings.ReplaceAll(token, `"`, `""`)+`"`)
		if len(tokens) == maxSearchTerms {
			break
		}
	}
	sort.Strings(tokens)
	return strings.Join(tokens, " OR ")
}

func splitSearchChunks(text string) []string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return nil
	}
	var chunks []string
	for start := 0; start < len(runes); {
		end := start + searchChunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[start:end])))
		if end == len(runes) {
			break
		}
		start = end - searchChunkOverlap
	}
	return chunks
}
