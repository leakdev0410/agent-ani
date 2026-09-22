package memdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	MemoryJobPendingExtraction = "pending_extraction"
	MemoryJobReadyToApply      = "ready_to_apply"
	MemoryJobBlocked           = "blocked"

	// MaxMemoryJobPayloadBytes is the maximum durable journal input. Callers
	// compact transient history before writing; CreateMemoryJob remains the
	// database-side defence-in-depth check.
	MaxMemoryJobPayloadBytes    = 512 * 1024
	maxMemoryJobExtractionBytes = 512 * 1024
)

// MemoryJob is durable work that has not yet been committed into the memory
// tables. Payload and extraction JSON are temporary and deleted after apply.
type MemoryJob struct {
	ID               int64
	State            string
	Payload          []byte
	ExtractionJSON   string
	ExtractionSHA256 string
	DeliveryPending  bool
	DeliverySequence int
	DeliveryInflight int
}

// DiscardMemoryJob permanently removes failed transient journal work. It
// never touches committed memory tables and accepts only actionable states.
func (db *DB) DiscardMemoryJob(id int64) error {
	return db.DiscardMemoryJobContext(context.Background(), id)
}

func (db *DB) DiscardMemoryJobContext(ctx context.Context, id int64) error {
	result, err := db.sql.ExecContext(ctx,
		"DELETE FROM memory_jobs WHERE id = ? AND state IN (?, ?)",
		id, MemoryJobPendingExtraction, MemoryJobReadyToApply,
	)
	if err != nil {
		return fmt.Errorf("memdb: discard memory job %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memdb: inspect discarded memory job %d: %w", id, err)
	}
	if affected != 1 {
		return fmt.Errorf("memdb: memory job %d cannot be discarded", id)
	}
	db.trace("DELETE", fmt.Sprintf("memory_jobs:%d", id), 0)
	return nil
}

// discardLegacyBlockedMemoryJobs clears diagnostics retained by older binary
// versions. Those rows contain transient chat data and must not survive once
// failed work is configured to be discarded.
func discardLegacyBlockedMemoryJobs(sqlDB *sql.DB) error {
	if _, err := sqlDB.Exec("DELETE FROM memory_jobs WHERE state = ?", MemoryJobBlocked); err != nil {
		return fmt.Errorf("memdb: discard legacy blocked memory jobs: %w", err)
	}
	return nil
}

// SetMemoryJobCommitErrorForTest installs a one-database commit seam used to
// verify rollback/recovery after all job writes and deletion have succeeded.
// It must only be used by tests; callers restore the prior seam with the
// returned function.
func (db *DB) SetMemoryJobCommitErrorForTest(inject func(*sql.Tx) error) func() {
	previous := db.memoryJobCommitErrorForTest
	db.memoryJobCommitErrorForTest = inject
	return func() { db.memoryJobCommitErrorForTest = previous }
}

// SetMemoryJobFinalizeExtractionErrorForTest makes the capture -> ready
// transition fail after the exact response is durable. It exists solely to
// prove restart recovery never asks the model for that response again.
func (db *DB) SetMemoryJobFinalizeExtractionErrorForTest(inject func() error) func() {
	previous := db.memoryJobFinalizeExtractionErrorForTest
	db.memoryJobFinalizeExtractionErrorForTest = inject
	return func() { db.memoryJobFinalizeExtractionErrorForTest = previous }
}

// SetMemoryJobCaptureExtractionErrorForTest makes the first persistence step
// fail before it changes the durable job. It exists solely to prove callers
// discard that pending job instead of repeating an already-observed model call.
func (db *DB) SetMemoryJobCaptureExtractionErrorForTest(inject func() error) func() {
	previous := db.memoryJobCaptureExtractionErrorForTest
	db.memoryJobCaptureExtractionErrorForTest = inject
	return func() { db.memoryJobCaptureExtractionErrorForTest = previous }
}

func (db *DB) CreateMemoryJob(payload []byte) (int64, error) {
	if len(payload) > MaxMemoryJobPayloadBytes {
		return 0, fmt.Errorf("memdb: memory job payload vượt %d bytes", MaxMemoryJobPayloadBytes)
	}
	result, err := db.sql.Exec(
		"INSERT INTO memory_jobs (state, payload, created_at) VALUES (?, ?, ?)",
		MemoryJobPendingExtraction, payload, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, fmt.Errorf("memdb: tạo memory job lá»—i: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("memdb: lấy ID memory job lá»—i: %w", err)
	}
	db.trace("WRITE", fmt.Sprintf("memory_jobs:%d", id), len(payload))
	return id, nil
}

// NextMemoryJob returns the oldest actionable job. The filter is defensive
// for legacy blocked rows created before Open removes them.
func (db *DB) NextMemoryJob() (MemoryJob, bool, error) {
	var job MemoryJob
	err := db.sql.QueryRow(
		"SELECT id, state, payload, extraction_json, extraction_sha256, delivery_pending, delivery_sequence, delivery_inflight FROM memory_jobs WHERE state <> 'blocked' ORDER BY id LIMIT 1",
	).Scan(&job.ID, &job.State, &job.Payload, &job.ExtractionJSON, &job.ExtractionSHA256, &job.DeliveryPending, &job.DeliverySequence, &job.DeliveryInflight)
	if err == sql.ErrNoRows {
		return MemoryJob{}, false, nil
	}
	if err != nil {
		return MemoryJob{}, false, fmt.Errorf("memdb: đọc memory job kế tiếp lá»—i: %w", err)
	}
	db.trace("READ", fmt.Sprintf("memory_jobs:%d", job.ID), len(job.Payload)+len(job.ExtractionJSON))
	return job, true, nil
}

// StoreExtraction is the compatibility operation for callers that want to
// capture an extraction and make it ready in one call. The capture itself is
// durable before the state transition, so a failure while finalizing is safely
// recoverable without another extraction request.
func (db *DB) StoreExtraction(id int64, raw string) error {
	if err := db.CaptureMemoryJobExtraction(id, raw); err != nil {
		return err
	}
	return db.FinalizeMemoryJobExtraction(id)
}

// CaptureMemoryJobExtraction writes an exact first model response while the
// job remains pending. It never overwrites a captured response.
func (db *DB) CaptureMemoryJobExtraction(id int64, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("memdb: extraction memory job is empty")
	}
	if len(raw) > maxMemoryJobExtractionBytes {
		return fmt.Errorf("memdb: extraction memory job vượt %d bytes", maxMemoryJobExtractionBytes)
	}
	if db.memoryJobCaptureExtractionErrorForTest != nil {
		if err := db.memoryJobCaptureExtractionErrorForTest(); err != nil {
			return fmt.Errorf("memdb: injected capture extraction %d: %w", id, err)
		}
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
	result, err := db.sql.Exec(
		`UPDATE memory_jobs
		 SET extraction_json = ?, extraction_sha256 = ?
		 WHERE id = ? AND state = ? AND extraction_json = '' AND delivery_pending = 0`,
		raw, digest, id, MemoryJobPendingExtraction,
	)
	if err != nil {
		return fmt.Errorf("memdb: capture extraction memory job %d lá»—i: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memdb: kiểm tra memory job %d lá»—i: %w", id, err)
	}
	if affected != 1 {
		return fmt.Errorf("memdb: memory job %d cannot capture extraction", id)
	}
	db.trace("WRITE", fmt.Sprintf("memory_jobs:%d", id), len(raw))
	return nil
}

// FinalizeMemoryJobExtraction changes a pending job with an already captured
// response into ready_to_apply. It never writes or regenerates extraction JSON.
func (db *DB) FinalizeMemoryJobExtraction(id int64) error {
	if db.memoryJobFinalizeExtractionErrorForTest != nil {
		if err := db.memoryJobFinalizeExtractionErrorForTest(); err != nil {
			return fmt.Errorf("memdb: injected finalize extraction %d: %w", id, err)
		}
	}
	result, err := db.sql.Exec(
		`UPDATE memory_jobs SET state = ?
		 WHERE id = ? AND state = ? AND extraction_json <> '' AND extraction_sha256 <> '' AND delivery_pending = 0`,
		MemoryJobReadyToApply, id, MemoryJobPendingExtraction,
	)
	if err != nil {
		return fmt.Errorf("memdb: finalize extraction memory job %d lá»—i: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memdb: inspect finalized memory job %d: %w", id, err)
	}
	if affected != 1 {
		return fmt.Errorf("memdb: memory job %d cannot finalize extraction", id)
	}
	db.trace("WRITE", fmt.Sprintf("memory_jobs:%d", id), 0)
	return nil
}

// ApplyAndDeleteMemoryJob runs committed-memory writes and removal of the
// ready job in one transaction. Any callback, SQL, or commit error rolls the
// transaction back, retaining the complete ready job for recovery.
func (db *DB) ApplyAndDeleteMemoryJob(id int64, apply func(*sql.Tx, string) error) error {
	if apply == nil {
		return fmt.Errorf("memdb: callback apply memory job rỗng")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("memdb: bắt đầu apply memory job %d lá»—i: %w", id, err)
	}
	defer tx.Rollback()

	var raw string
	err = tx.QueryRow(
		"SELECT extraction_json FROM memory_jobs WHERE id = ? AND state = ? AND delivery_pending = 0", id, MemoryJobReadyToApply,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return fmt.Errorf("memdb: memory job %d không sẵn sàng để apply", id)
	}
	if err != nil {
		return fmt.Errorf("memdb: đọc memory job %d để apply lá»—i: %w", id, err)
	}
	if err := apply(tx, raw); err != nil {
		return fmt.Errorf("memdb: apply memory job %d lá»—i: %w", id, err)
	}
	result, err := tx.Exec("DELETE FROM memory_jobs WHERE id = ? AND state = ? AND delivery_pending = 0", id, MemoryJobReadyToApply)
	if err != nil {
		return fmt.Errorf("memdb: xoá memory job %d lá»—i: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memdb: kiểm tra xoá memory job %d lá»—i: %w", id, err)
	}
	if affected != 1 {
		return fmt.Errorf("memdb: memory job %d không còn sẵn sàng để xoá", id)
	}
	if db.memoryJobCommitErrorForTest != nil {
		if err := db.memoryJobCommitErrorForTest(tx); err != nil {
			return fmt.Errorf("memdb: injected commit apply memory job %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memdb: commit apply memory job %d lá»—i: %w", id, err)
	}
	db.trace("DELETE", fmt.Sprintf("memory_jobs:%d", id), len(raw))
	return nil
}

func (db *DB) MemoryJobCounts() (pending, ready, blocked int, err error) {
	err = db.sql.QueryRow(`SELECT
		COALESCE(SUM(state = 'pending_extraction'), 0),
		COALESCE(SUM(state = 'ready_to_apply'), 0),
		COALESCE(SUM(state = 'blocked'), 0)
		FROM memory_jobs`).Scan(&pending, &ready, &blocked)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("memdb: đếm memory jobs lá»—i: %w", err)
	}
	db.trace("READ", "memory_jobs(count)", pending+ready+blocked)
	return pending, ready, blocked, nil
}

// OldestMemoryJobCreatedAt returns only operational age metadata for the
// oldest actionable job. It never returns a payload or extraction response.
func (db *DB) OldestMemoryJobCreatedAt() (time.Time, bool, error) {
	var raw string
	err := db.sql.QueryRow("SELECT created_at FROM memory_jobs WHERE state <> 'blocked' ORDER BY id LIMIT 1").Scan(&raw)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("memdb: read oldest memory job age: %w", err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("memdb: parse oldest memory job age: %w", err)
	}
	return createdAt, true, nil
}
