package memdb

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMemoryDeliveryContextCancelsWhileWaitingForDBConnection(t *testing.T) {
	db := newMemoryJobTestDB(t)
	id, err := db.CreateHeldMemoryJob([]byte("base"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.MarkMemoryDeliveryAttemptContext(ctx, id, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("MarkMemoryDeliveryAttemptContext error = %v, want context.Canceled", err)
	}
	if err := db.DiscardMemoryJobContext(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("DiscardMemoryJobContext error = %v, want context.Canceled", err)
	}
}

func TestDeliveryMigrationKeepsLegacyJobsExtractable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE memory_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		state TEXT NOT NULL CHECK (state IN ('pending_extraction', 'ready_to_apply', 'blocked')),
		payload BLOB NOT NULL,
		extraction_json TEXT NOT NULL DEFAULT '',
		extraction_sha256 TEXT NOT NULL DEFAULT '',
		failure_code TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	); INSERT INTO memory_jobs (state, payload, created_at) VALUES ('pending_extraction', 'legacy', '2026-09-14T00:00:00Z')`)
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy DB: %v", err)
	}
	defer db.Close()
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok {
		t.Fatalf("NextMemoryJob = (%+v, %v, %v)", job, ok, err)
	}
	if job.DeliveryPending || job.DeliverySequence != 0 || job.DeliveryInflight != 0 {
		t.Fatalf("legacy delivery defaults = pending:%v sequence:%d inflight:%d", job.DeliveryPending, job.DeliverySequence, job.DeliveryInflight)
	}
	if err := db.StoreExtraction(job.ID, `{"legacy":true}`); err != nil {
		t.Fatalf("legacy job must remain extractable: %v", err)
	}
}

func TestCreateHeldMemoryJobIsVisibleButNotExtractable(t *testing.T) {
	db := newMemoryJobTestDB(t)
	id, err := db.CreateHeldMemoryJob([]byte(`{"history":["real"]}`))
	if err != nil {
		t.Fatalf("CreateHeldMemoryJob: %v", err)
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.ID != id {
		t.Fatalf("NextMemoryJob = (%+v, %v, %v)", job, ok, err)
	}
	if !job.DeliveryPending || job.State != MemoryJobPendingExtraction || job.ExtractionJSON != "" {
		t.Fatalf("fresh held job = %+v", job)
	}
	if err := db.CaptureMemoryJobExtraction(id, `{"forbidden":true}`); err == nil {
		t.Fatal("CaptureMemoryJobExtraction accepted held job")
	}
	if _, err := db.sql.Exec("UPDATE memory_jobs SET extraction_json = 'captured', extraction_sha256 = 'digest' WHERE id = ?", id); err != nil {
		t.Fatalf("prepare captured held row: %v", err)
	}
	if err := db.FinalizeMemoryJobExtraction(id); err == nil {
		t.Fatal("FinalizeMemoryJobExtraction accepted held job")
	}
	if _, err := db.sql.Exec("UPDATE memory_jobs SET state = ? WHERE id = ?", MemoryJobReadyToApply, id); err != nil {
		t.Fatalf("prepare ready held row: %v", err)
	}
	called := false
	if err := db.ApplyAndDeleteMemoryJob(id, func(*sql.Tx, string) error { called = true; return nil }); err == nil {
		t.Fatal("ApplyAndDeleteMemoryJob accepted held job")
	}
	if called {
		t.Fatal("apply callback ran for held job")
	}
}

func TestHeldMemoryJobsReturnsOnlyHeldRowsInIDOrder(t *testing.T) {
	db := newMemoryJobTestDB(t)
	if _, err := db.CreateMemoryJob([]byte("legacy")); err != nil {
		t.Fatal(err)
	}
	first, err := db.CreateHeldMemoryJob([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateHeldMemoryJob([]byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := db.HeldMemoryJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != first || jobs[1].ID != second {
		t.Fatalf("HeldMemoryJobs = %+v", jobs)
	}
}

func TestMemoryDeliveryACKIsIdempotentOnlyForSameSequenceAndPayload(t *testing.T) {
	db := newMemoryJobTestDB(t)
	id, err := db.CreateHeldMemoryJob([]byte(`{"base":true}`))
	if err != nil {
		t.Fatal(err)
	}
	ack := []byte(`{"prefix":["one"]}`)
	if err := db.MarkMemoryDeliveryAttempt(id, 1); err != nil {
		t.Fatalf("MarkMemoryDeliveryAttempt(1): %v", err)
	}
	if err := db.MarkMemoryDeliveryAttempt(id, 1); err != nil {
		t.Fatalf("retry same inflight sequence must be idempotent: %v", err)
	}
	if err := db.RecordMemoryDeliveryPrefix(id, 1, ack); err != nil {
		t.Fatalf("RecordMemoryDeliveryPrefix(1): %v", err)
	}
	if err := db.RecordMemoryDeliveryPrefix(id, 1, append([]byte(nil), ack...)); err != nil {
		t.Fatalf("idempotent ACK: %v", err)
	}
	if err := db.RecordMemoryDeliveryPrefix(id, 1, []byte(`{"prefix":["changed"]}`)); err == nil {
		t.Fatal("same sequence accepted a different payload")
	}
	if err := db.RecordMemoryDeliveryPrefix(id, 0, ack); err == nil {
		t.Fatal("lower sequence ACK unexpectedly succeeded")
	}
	if err := db.MarkMemoryDeliveryAttempt(id, 3); err == nil {
		t.Fatal("attempt skipped confirmed sequence")
	}
	if err := db.MarkMemoryDeliveryAttempt(id, 2); err != nil {
		t.Fatalf("MarkMemoryDeliveryAttempt(2): %v", err)
	}
	ack2 := []byte(`{"prefix":["one","two"]}`)
	if err := db.RecordMemoryDeliveryPrefix(id, 2, ack2); err != nil {
		t.Fatalf("RecordMemoryDeliveryPrefix(2): %v", err)
	}
	job, _, err := db.NextMemoryJob()
	if err != nil {
		t.Fatal(err)
	}
	if job.DeliverySequence != 2 || job.DeliveryInflight != 0 || string(job.Payload) != string(ack2) {
		t.Fatalf("ACK state = %+v", job)
	}
}

func TestFinalizeMemoryDeliveryClearsGateAtomically(t *testing.T) {
	db := newMemoryJobTestDB(t)
	id, err := db.CreateHeldMemoryJob([]byte("base"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMemoryDeliveryAttempt(id, 1); err != nil {
		t.Fatal(err)
	}
	restore := db.SetMemoryJobCommitErrorForTest(func(*sql.Tx) error { return errors.New("forced") })
	if err := db.FinalizeMemoryDelivery(id, []byte("final")); err == nil {
		t.Fatal("FinalizeMemoryDelivery unexpectedly committed")
	}
	restore()
	job, _, err := db.NextMemoryJob()
	if err != nil {
		t.Fatal(err)
	}
	if !job.DeliveryPending || job.DeliveryInflight != 1 || string(job.Payload) != "base" {
		t.Fatalf("failed finalize partially changed row: %+v", job)
	}
	if err := db.FinalizeMemoryDelivery(id, []byte("final")); err != nil {
		t.Fatalf("FinalizeMemoryDelivery: %v", err)
	}
	job, _, err = db.NextMemoryJob()
	if err != nil {
		t.Fatal(err)
	}
	if job.DeliveryPending || job.DeliveryInflight != 0 || string(job.Payload) != "final" {
		t.Fatalf("finalized row = %+v", job)
	}
	if err := db.CaptureMemoryJobExtraction(id, `{"allowed":true}`); err != nil {
		t.Fatalf("finalized job should be extractable: %v", err)
	}
}
