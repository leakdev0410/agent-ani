package memdb

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryJobStoreExtractionIsImmutable(t *testing.T) {
	db := newMemoryJobTestDB(t)

	payload := []byte(`{"chat_id":42,"message":"private"}`)
	jobID, err := db.CreateMemoryJob(payload)
	if err != nil {
		t.Fatalf("CreateMemoryJob: %v", err)
	}
	raw := `{"core":{"note":"remember this"}}`
	if err := db.StoreExtraction(jobID, raw); err != nil {
		t.Fatalf("StoreExtraction: %v", err)
	}

	job, ok, err := db.NextMemoryJob()
	if err != nil {
		t.Fatalf("NextMemoryJob: %v", err)
	}
	if !ok {
		t.Fatal("NextMemoryJob returned no job")
	}
	if job.ID != jobID || job.State != MemoryJobReadyToApply {
		t.Fatalf("job = %+v, want ready job %d", job, jobID)
	}
	if string(job.Payload) != string(payload) || job.ExtractionJSON != raw {
		t.Fatalf("stored job data changed: %+v", job)
	}
	wantDigest := sha256.Sum256([]byte(raw))
	if job.ExtractionSHA256 != strings.ToLower(fmtSHA256(wantDigest)) {
		t.Fatalf("digest = %q, want %x", job.ExtractionSHA256, wantDigest)
	}

	if err := db.StoreExtraction(jobID, `{"different":true}`); err == nil {
		t.Fatal("second StoreExtraction unexpectedly succeeded")
	}
	job, ok, err = db.NextMemoryJob()
	if err != nil || !ok {
		t.Fatalf("read job after rejected rewrite: ok=%v err=%v", ok, err)
	}
	if job.ExtractionJSON != raw || job.ExtractionSHA256 != strings.ToLower(fmtSHA256(wantDigest)) {
		t.Fatalf("extraction was rewritten: %+v", job)
	}
}

func TestMemoryJobNextSkipsBlockedHead(t *testing.T) {
	db := newMemoryJobTestDB(t)

	blockedID, err := db.CreateMemoryJob([]byte("first"))
	if err != nil {
		t.Fatalf("CreateMemoryJob(first): %v", err)
	}
	pendingID, err := db.CreateMemoryJob([]byte("second"))
	if err != nil {
		t.Fatalf("CreateMemoryJob(second): %v", err)
	}
	if _, err := db.sql.Exec("UPDATE memory_jobs SET state = ? WHERE id = ?", MemoryJobBlocked, blockedID); err != nil {
		t.Fatalf("block oldest job: %v", err)
	}

	job, ok, err := db.NextMemoryJob()
	if err != nil {
		t.Fatalf("NextMemoryJob: %v", err)
	}
	if !ok || job.ID != pendingID || job.State != MemoryJobPendingExtraction {
		t.Fatalf("NextMemoryJob = (%+v, %v), want pending job %d after blocked %d", job, ok, pendingID, blockedID)
	}

	pending, ready, blocked, err := db.MemoryJobCounts()
	if err != nil {
		t.Fatalf("MemoryJobCounts: %v", err)
	}
	if pending != 1 || ready != 0 || blocked != 1 {
		t.Fatalf("MemoryJobCounts = (%d, %d, %d), want (1, 0, 1)", pending, ready, blocked)
	}
}

func TestOldestMemoryJobCreatedAtSkipsBlockedRows(t *testing.T) {
	db := newMemoryJobTestDB(t)

	blockedID, err := db.CreateMemoryJob([]byte("blocked"))
	if err != nil {
		t.Fatalf("CreateMemoryJob(blocked): %v", err)
	}
	pendingID, err := db.CreateMemoryJob([]byte("pending"))
	if err != nil {
		t.Fatalf("CreateMemoryJob(pending): %v", err)
	}
	blockedAt := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	pendingAt := blockedAt.Add(time.Minute)
	if _, err := db.sql.Exec("UPDATE memory_jobs SET state = ?, created_at = ? WHERE id = ?", MemoryJobBlocked, blockedAt.Format(time.RFC3339Nano), blockedID); err != nil {
		t.Fatalf("block oldest job: %v", err)
	}
	if _, err := db.sql.Exec("UPDATE memory_jobs SET created_at = ? WHERE id = ?", pendingAt.Format(time.RFC3339Nano), pendingID); err != nil {
		t.Fatalf("set pending timestamp: %v", err)
	}

	got, ok, err := db.OldestMemoryJobCreatedAt()
	if err != nil || !ok {
		t.Fatalf("OldestMemoryJobCreatedAt = (%v, %v, %v), want pending timestamp", got, ok, err)
	}
	if !got.Equal(pendingAt) {
		t.Fatalf("OldestMemoryJobCreatedAt = %v, want %v", got, pendingAt)
	}
}

func TestOldestMemoryJobCreatedAtReturnsNoneForOnlyBlockedRows(t *testing.T) {
	db := newMemoryJobTestDB(t)

	blockedID, err := db.CreateMemoryJob([]byte("blocked"))
	if err != nil {
		t.Fatalf("CreateMemoryJob: %v", err)
	}
	if _, err := db.sql.Exec("UPDATE memory_jobs SET state = ? WHERE id = ?", MemoryJobBlocked, blockedID); err != nil {
		t.Fatalf("block job: %v", err)
	}

	if got, ok, err := db.OldestMemoryJobCreatedAt(); err != nil || ok || !got.IsZero() {
		t.Fatalf("OldestMemoryJobCreatedAt = (%v, %v, %v), want zero, false, nil", got, ok, err)
	}
}

func TestMemoryJobApplyAndDeleteIsAtomic(t *testing.T) {
	db := newMemoryJobTestDB(t)
	jobID := createReadyMemoryJob(t, db, `{"update":"once"}`)

	err := db.ApplyAndDeleteMemoryJob(jobID, func(tx *sql.Tx, raw string) error {
		if raw != `{"update":"once"}` {
			t.Fatalf("callback raw = %q", raw)
		}
		_, err := tx.Exec("INSERT INTO diary_entries (entry_date, text, topic, created_at) VALUES (?, ?, ?, ?)", "2026-08-26", "committed", "", "2026-08-26T00:00:00Z")
		return err
	})
	if err != nil {
		t.Fatalf("ApplyAndDeleteMemoryJob: %v", err)
	}

	assertMemoryJobRowCount(t, db, 0)
	var diaryCount int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM diary_entries").Scan(&diaryCount); err != nil {
		t.Fatalf("count diary entries: %v", err)
	}
	if diaryCount != 1 {
		t.Fatalf("diary count = %d, want 1", diaryCount)
	}
}

func TestMemoryJobApplyFailureRetainsJobAndRollsBackWrites(t *testing.T) {
	db := newMemoryJobTestDB(t)
	jobID := createReadyMemoryJob(t, db, `{"update":"retry"}`)

	err := db.ApplyAndDeleteMemoryJob(jobID, func(tx *sql.Tx, raw string) error {
		if _, err := tx.Exec("INSERT INTO diary_entries (entry_date, text, topic, created_at) VALUES (?, ?, ?, ?)", "2026-08-26", "must rollback", "", "2026-08-26T00:00:00Z"); err != nil {
			return err
		}
		return errors.New("forced callback failure")
	})
	if err == nil {
		t.Fatal("ApplyAndDeleteMemoryJob unexpectedly succeeded")
	}

	assertMemoryJobRowCount(t, db, 1)
	var diaryCount int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM diary_entries").Scan(&diaryCount); err != nil {
		t.Fatalf("count diary entries: %v", err)
	}
	if diaryCount != 0 {
		t.Fatalf("diary count = %d, want rollback to 0", diaryCount)
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.ID != jobID || job.ExtractionJSON != `{"update":"retry"}` {
		t.Fatalf("ready job was not retained: job=%+v ok=%v err=%v", job, ok, err)
	}
}

func TestMemoryJobApplyAndDeleteCommitsEmbeddingQueueInSameTransaction(t *testing.T) {
	db := newMemoryJobTestDB(t)
	jobID := createReadyMemoryJob(t, db, `{"update":"with embedding"}`)

	err := db.ApplyAndDeleteMemoryJob(jobID, func(tx *sql.Tx, raw string) error {
		result, err := tx.Exec("INSERT INTO diary_entries (entry_date, text, topic, created_at) VALUES (?, ?, ?, ?)", "2026-08-26", "committed source", "", "2026-08-26T00:00:00Z")
		if err != nil {
			return err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		return db.EnqueueEmbeddingSourceTx(tx, fmt.Sprintf("diary:%d:2026-08-26", id), digestForTest("committed source"), "committed source")
	})
	if err != nil {
		t.Fatalf("ApplyAndDeleteMemoryJob: %v", err)
	}
	assertMemoryJobRowCount(t, db, 0)
	var diaryCount int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM diary_entries").Scan(&diaryCount); err != nil || diaryCount != 1 {
		t.Fatalf("committed diary count = %d, err = %v; want 1, nil", diaryCount, err)
	}
	source, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok || source.SourceKey != "diary:1:2026-08-26" || source.Text != "committed source" {
		t.Fatalf("committed embedding source = %+v, ok=%v, err=%v", source, ok, err)
	}
}

func TestMemoryJobApplyRollbackDoesNotQueueEmbedding(t *testing.T) {
	db := newMemoryJobTestDB(t)
	jobID := createReadyMemoryJob(t, db, `{"update":"rollback embedding"}`)

	err := db.ApplyAndDeleteMemoryJob(jobID, func(tx *sql.Tx, raw string) error {
		result, err := tx.Exec("INSERT INTO diary_entries (entry_date, text, topic, created_at) VALUES (?, ?, ?, ?)", "2026-08-26", "rolled back source", "", "2026-08-26T00:00:00Z")
		if err != nil {
			return err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		if err := db.EnqueueEmbeddingSourceTx(tx, fmt.Sprintf("diary:%d:2026-08-26", id), digestForTest("rolled back source"), "rolled back source"); err != nil {
			return err
		}
		return errors.New("force whole apply rollback")
	})
	if err == nil {
		t.Fatal("ApplyAndDeleteMemoryJob unexpectedly succeeded")
	}
	assertMemoryJobRowCount(t, db, 1)
	var diaryCount int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM diary_entries").Scan(&diaryCount); err != nil || diaryCount != 0 {
		t.Fatalf("rolled back diary count = %d, err = %v; want 0, nil", diaryCount, err)
	}
	if _, ok, err := db.NextEmbeddingSource(); err != nil || ok {
		t.Fatalf("embedding queue must roll back with job: ok=%v err=%v", ok, err)
	}
}

func TestMemoryJobCreateRejectsPayloadOverBound(t *testing.T) {
	db := newMemoryJobTestDB(t)

	if _, err := db.CreateMemoryJob(make([]byte, 512*1024+1)); err == nil {
		t.Fatal("CreateMemoryJob accepted an unbounded payload")
	}
	assertMemoryJobRowCount(t, db, 0)
}

func TestMemoryJobDiscardRemovesTransientPayload(t *testing.T) {
	db := newMemoryJobTestDB(t)
	jobID, err := db.CreateMemoryJob([]byte("private failed payload"))
	if err != nil {
		t.Fatalf("CreateMemoryJob: %v", err)
	}

	if err := db.DiscardMemoryJob(jobID); err != nil {
		t.Fatalf("DiscardMemoryJob: %v", err)
	}
	assertMemoryJobRowCount(t, db, 0)
	if _, ok, err := db.NextMemoryJob(); err != nil || ok {
		t.Fatalf("NextMemoryJob after discard = (ok=%v, err=%v), want no job", ok, err)
	}
}

func TestOpenDiscardsLegacyBlockedMemoryJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := db.CreateMemoryJob([]byte("legacy private payload"))
	if err != nil {
		t.Fatalf("CreateMemoryJob: %v", err)
	}
	if _, err := db.sql.Exec("UPDATE memory_jobs SET state = ? WHERE id = ?", MemoryJobBlocked, jobID); err != nil {
		t.Fatalf("make legacy blocked job: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertMemoryJobRowCount(t, db, 0)
}

func TestMemoryJobStoreExtractionRejectsEmptyAndOversizedRawJSON(t *testing.T) {
	db := newMemoryJobTestDB(t)
	jobID, err := db.CreateMemoryJob([]byte("bounded payload"))
	if err != nil {
		t.Fatalf("CreateMemoryJob: %v", err)
	}
	if err := db.StoreExtraction(jobID, ""); err == nil {
		t.Fatal("StoreExtraction accepted an empty extraction")
	}
	if err := db.StoreExtraction(jobID, strings.Repeat("x", 512*1024+1)); err == nil {
		t.Fatal("StoreExtraction accepted an oversized extraction")
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.ID != jobID || job.State != MemoryJobPendingExtraction {
		t.Fatalf("invalid extraction changed job state: job=%+v ok=%v err=%v", job, ok, err)
	}
}

func TestMemoryJobSchemaRejectsUnknownState(t *testing.T) {
	db := newMemoryJobTestDB(t)
	if _, err := db.sql.Exec("INSERT INTO memory_jobs (state, payload, created_at) VALUES (?, ?, ?)", "unknown", []byte("payload"), "2026-08-26T00:00:00Z"); err == nil {
		t.Fatal("memory_jobs accepted an unknown state")
	}
}

func TestMemoryJobVerboseTraceDoesNotContainPayloadOrExtraction(t *testing.T) {
	db := newMemoryJobTestDB(t)
	var trace bytes.Buffer
	db.logger.SetOutput(&trace)
	db.SetVerbose(true)
	payload := []byte("private job payload must not be logged")
	raw := `{"private":"extraction must not be logged"}`
	jobID, err := db.CreateMemoryJob(payload)
	if err != nil {
		t.Fatalf("CreateMemoryJob: %v", err)
	}
	if err := db.StoreExtraction(jobID, raw); err != nil {
		t.Fatalf("StoreExtraction: %v", err)
	}
	if strings.Contains(trace.String(), string(payload)) || strings.Contains(trace.String(), raw) {
		t.Fatalf("verbose trace exposed transient memory data: %q", trace.String())
	}
}

func newMemoryJobTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createReadyMemoryJob(t *testing.T, db *DB, raw string) int64 {
	t.Helper()
	id, err := db.CreateMemoryJob([]byte("bounded private payload"))
	if err != nil {
		t.Fatalf("CreateMemoryJob: %v", err)
	}
	if err := db.StoreExtraction(id, raw); err != nil {
		t.Fatalf("StoreExtraction: %v", err)
	}
	return id
}

func assertMemoryJobRowCount(t *testing.T, db *DB, want int) {
	t.Helper()
	var got int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM memory_jobs").Scan(&got); err != nil {
		t.Fatalf("count memory jobs: %v", err)
	}
	if got != want {
		t.Fatalf("memory job count = %d, want %d", got, want)
	}
}

func fmtSHA256(sum [sha256.Size]byte) string {
	return fmt.Sprintf("%x", sum)
}
