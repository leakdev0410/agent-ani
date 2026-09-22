package memdb

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSearchMemory_RebuildsAllMemorySources(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.AddDiaryEntry("2026-08-22", "09:00: chốt deadline dự án Sao Mai", "projects"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddObservation(ObservationPreference, "Anh thích uống cà phê ít đường", "2026-08-22"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTopicNote("personal", "Cuộc hẹn khám răng vào sáng thứ hai", "2026-08-22T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(TopicKey("projects"), "Kế hoạch dài hạn: hoàn thiện sản phẩm Ani trước tháng chín."); err != nil {
		t.Fatal(err)
	}
	if err := db.RebuildSearchIndex(); err != nil {
		t.Fatalf("RebuildSearchIndex: %v", err)
	}

	tests := []struct {
		query      string
		sourceType string
		contains   string
	}{
		{"deadline Sao Mai", "diary", "Sao Mai"},
		{"ca phe ít đường", "observation", "cà phê"},
		{"khám răng", "topic_note", "khám răng"},
		{"sản phẩm Ani tháng chín", "topic", "sản phẩm Ani"},
	}
	for _, tc := range tests {
		results, err := db.SearchMemory(tc.query, 8)
		if err != nil {
			t.Fatalf("SearchMemory(%q): %v", tc.query, err)
		}
		if !hasSearchResult(results, tc.sourceType, tc.contains) {
			t.Errorf("SearchMemory(%q) không thấy %s chứa %q: %+v", tc.query, tc.sourceType, tc.contains, results)
		}
	}
}

func TestSearchMemory_TriggersKeepIndexCurrent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.RebuildSearchIndex(); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTopicNote("personal", "Nhắc gọi điện cho mẹ tối nay", "2026-08-22T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	results, err := db.SearchMemory("gọi điện cho mẹ", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSearchResult(results, "topic_note", "gọi điện cho mẹ") {
		t.Fatalf("row mới phải được trigger đưa vào FTS ngay: %+v", results)
	}

	if err := db.AddDiaryEntry("2026-08-22", "13:00: hoàn thành giao diện dashboard", ""); err != nil {
		t.Fatal(err)
	}
	entries, err := db.DiaryEntriesWithoutTopic()
	if err != nil || len(entries) != 1 {
		t.Fatalf("đọc diary mới: entries=%+v err=%v", entries, err)
	}
	if err := db.UpdateDiaryTopic(entries[0].ID, "projects"); err != nil {
		t.Fatal(err)
	}
	results, err = db.SearchMemory("giao diện dashboard", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].Topic != "projects" {
		t.Fatalf("trigger UPDATE phải cập nhật topic trong FTS: %+v", results)
	}
}

func TestRebuildSearchIndexQueuesEveryCommittedSourceWithoutDuplicates(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.AddDiaryEntry("2026-08-22", "diary source", "projects"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddObservation(ObservationPreference, "observation source", "2026-08-22"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTopicNote("personal", "topic note source", "2026-08-22T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(TopicKey("projects"), strings.Repeat("t", searchChunkRunes+1)); err != nil {
		t.Fatal(err)
	}

	if err := db.RebuildSearchIndex(); err != nil {
		t.Fatalf("first RebuildSearchIndex: %v", err)
	}
	want := searchableSourcesForTest(t, db)
	got := queuedEmbeddingSourcesForTest(t, db)
	if len(got) != len(want) {
		t.Fatalf("queued source count = %d, want every %d committed search source: got=%v want=%v", len(got), len(want), got, want)
	}
	for key, text := range want {
		gotSource, ok := got[key]
		if !ok || gotSource.Text != text || gotSource.ContentDigest != sha256Text(text) {
			t.Fatalf("queued source %q = %+v, want text/digest for %q", key, gotSource, text)
		}
	}
	if _, ok := got["topic:topic/projects:0"]; !ok {
		t.Fatalf("first chunked topic source was not queued: %v", got)
	}
	if _, ok := got["topic:topic/projects:1"]; !ok {
		t.Fatalf("second chunked topic source was not queued: %v", got)
	}

	if err := db.RebuildSearchIndex(); err != nil {
		t.Fatalf("second RebuildSearchIndex: %v", err)
	}
	got = queuedEmbeddingSourcesForTest(t, db)
	if len(got) != len(want) {
		t.Fatalf("second rebuild duplicated queue work: got=%v want=%v", got, want)
	}

	if _, err := db.sql.Exec("UPDATE diary_entries SET text = ? WHERE id = 1", "changed diary source"); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(TopicKey("projects"), "replacement topic chunk"); err != nil {
		t.Fatal(err)
	}
	if err := db.RebuildSearchIndex(); err != nil {
		t.Fatalf("rebuild after committed source changes: %v", err)
	}
	want = searchableSourcesForTest(t, db)
	got = queuedEmbeddingSourcesForTest(t, db)
	if len(got) != len(want) {
		t.Fatalf("changed rebuild left stale or duplicate queue work: got=%v want=%v", got, want)
	}
	for key, text := range want {
		gotSource, ok := got[key]
		if !ok || gotSource.Text != text || gotSource.ContentDigest != sha256Text(text) {
			t.Fatalf("changed queued source %q = %+v, want text/digest for %q", key, gotSource, text)
		}
	}
	if _, ok := got["topic:topic/projects:1"]; ok {
		t.Fatalf("removed topic chunk remained queued: %v", got)
	}
}

func TestSearchMemory_SanitizesFTSSyntax(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.AddDiaryEntry("2026-08-22", "deadline quan trọng", "projects"); err != nil {
		t.Fatal(err)
	}

	results, err := db.SearchMemory(`deadline" OR * (NEAR)`, 4)
	if err != nil {
		t.Fatalf("ký tự cú pháp từ người dùng không được làm hỏng MATCH: %v", err)
	}
	if !hasSearchResult(results, "diary", "deadline") {
		t.Fatalf("query đã chuẩn hoá vẫn phải tìm thấy memory: %+v", results)
	}
}

func TestSearchMatchQuery_LimitsWork(t *testing.T) {
	var words []string
	for i := 0; i < 100; i++ {
		words = append(words, "term"+strconv.Itoa(i))
	}
	match := searchMatchQuery(strings.Join(words, " "))
	if terms := strings.Count(match, `"`) / 2; terms != maxSearchTerms {
		t.Fatalf("MATCH phải bị giới hạn đúng %d term, được %d", maxSearchTerms, terms)
	}
}

func hasSearchResult(results []SearchResult, sourceType, contains string) bool {
	for _, result := range results {
		if result.SourceType == sourceType && strings.Contains(result.Text, contains) {
			return true
		}
	}
	return false
}

func searchableSourcesForTest(t *testing.T, db *DB) map[string]string {
	t.Helper()
	rows, err := db.sql.Query("SELECT source_type, source_ref, text FROM memory_search ORDER BY source_type, source_ref")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var sourceType, sourceRef, text string
		if err := rows.Scan(&sourceType, &sourceRef, &text); err != nil {
			t.Fatal(err)
		}
		result[fmt.Sprintf("%s:%s", sourceType, sourceRef)] = text
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func queuedEmbeddingSourcesForTest(t *testing.T, db *DB) map[string]EmbeddingSource {
	t.Helper()
	rows, err := db.sql.Query("SELECT id, source_key, content_sha256, text FROM embedding_queue ORDER BY source_key")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make(map[string]EmbeddingSource)
	for rows.Next() {
		var source EmbeddingSource
		if err := rows.Scan(&source.ID, &source.SourceKey, &source.ContentDigest, &source.Text); err != nil {
			t.Fatal(err)
		}
		if _, duplicate := result[source.SourceKey]; duplicate {
			t.Fatalf("duplicate queued source key %q", source.SourceKey)
		}
		result[source.SourceKey] = source
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
