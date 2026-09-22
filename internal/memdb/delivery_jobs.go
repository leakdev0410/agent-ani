package memdb

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (db *DB) CreateHeldMemoryJob(payload []byte) (int64, error) {
	return db.CreateHeldMemoryJobContext(context.Background(), payload)
}

func (db *DB) CreateHeldMemoryJobContext(ctx context.Context, payload []byte) (int64, error) {
	if len(payload) > MaxMemoryJobPayloadBytes {
		return 0, fmt.Errorf("memdb: held memory job payload exceeds %d bytes", MaxMemoryJobPayloadBytes)
	}
	result, err := db.sql.ExecContext(ctx, `INSERT INTO memory_jobs (state, payload, delivery_pending, created_at) VALUES (?, ?, 1, ?)`, MemoryJobPendingExtraction, payload, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("memdb: create held memory job: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("memdb: read held memory job ID: %w", err)
	}
	db.trace("WRITE", fmt.Sprintf("memory_jobs:%d", id), len(payload))
	return id, nil
}

func (db *DB) MarkMemoryDeliveryAttempt(id int64, sequence int) error {
	return db.MarkMemoryDeliveryAttemptContext(context.Background(), id, sequence)
}

func (db *DB) MarkMemoryDeliveryAttemptContext(ctx context.Context, id int64, sequence int) error {
	if sequence <= 0 {
		return fmt.Errorf("memdb: invalid memory delivery sequence %d", sequence)
	}
	result, err := db.sql.ExecContext(ctx, `UPDATE memory_jobs SET delivery_inflight = ? WHERE id = ? AND state = ? AND extraction_json = '' AND delivery_pending = 1 AND delivery_inflight IN (0, ?) AND delivery_sequence + 1 = ?`, sequence, id, MemoryJobPendingExtraction, sequence, sequence)
	if err != nil {
		return fmt.Errorf("memdb: mark memory delivery attempt %d/%d: %w", id, sequence, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memdb: inspect memory delivery attempt %d/%d: %w", id, sequence, err)
	}
	if affected != 1 {
		return fmt.Errorf("memdb: memory delivery attempt %d/%d is not next", id, sequence)
	}
	return nil
}

func (db *DB) RecordMemoryDeliveryPrefix(id int64, sequence int, payload []byte) error {
	return db.RecordMemoryDeliveryPrefixContext(context.Background(), id, sequence, payload)
}

func (db *DB) RecordMemoryDeliveryPrefixContext(ctx context.Context, id int64, sequence int, payload []byte) error {
	if sequence <= 0 {
		return fmt.Errorf("memdb: invalid memory delivery sequence %d", sequence)
	}
	if len(payload) > MaxMemoryJobPayloadBytes {
		return fmt.Errorf("memdb: memory delivery payload exceeds %d bytes", MaxMemoryJobPayloadBytes)
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memdb: begin memory delivery ACK %d/%d: %w", id, sequence, err)
	}
	defer tx.Rollback()
	var current []byte
	var confirmed, inflight int
	err = tx.QueryRowContext(ctx, `SELECT payload, delivery_sequence, delivery_inflight FROM memory_jobs WHERE id = ? AND state = ? AND extraction_json = '' AND delivery_pending = 1`, id, MemoryJobPendingExtraction).Scan(&current, &confirmed, &inflight)
	if err == sql.ErrNoRows {
		return fmt.Errorf("memdb: held memory job %d cannot record delivery ACK", id)
	}
	if err != nil {
		return fmt.Errorf("memdb: read memory delivery ACK %d/%d: %w", id, sequence, err)
	}
	if sequence == confirmed {
		if !bytes.Equal(current, payload) {
			return fmt.Errorf("memdb: memory delivery ACK %d/%d payload mismatch", id, sequence)
		}
		return nil
	}
	if sequence < confirmed || sequence != confirmed+1 || inflight != sequence {
		return fmt.Errorf("memdb: memory delivery ACK %d/%d is not active", id, sequence)
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET payload = ?, delivery_sequence = ?, delivery_inflight = 0 WHERE id = ? AND state = ? AND extraction_json = '' AND delivery_pending = 1 AND delivery_sequence = ? AND delivery_inflight = ?`, payload, sequence, id, MemoryJobPendingExtraction, confirmed, sequence)
	if err != nil {
		return fmt.Errorf("memdb: record memory delivery ACK %d/%d: %w", id, sequence, err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		if err != nil {
			return fmt.Errorf("memdb: inspect memory delivery ACK %d/%d: %w", id, sequence, err)
		}
		return fmt.Errorf("memdb: memory delivery ACK %d/%d changed concurrently", id, sequence)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memdb: commit memory delivery ACK %d/%d: %w", id, sequence, err)
	}
	return nil
}

func (db *DB) FinalizeMemoryDelivery(id int64, payload []byte) error {
	return db.FinalizeMemoryDeliveryContext(context.Background(), id, payload)
}

func (db *DB) FinalizeMemoryDeliveryContext(ctx context.Context, id int64, payload []byte) error {
	if len(payload) > MaxMemoryJobPayloadBytes {
		return fmt.Errorf("memdb: finalized memory delivery payload exceeds %d bytes", MaxMemoryJobPayloadBytes)
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memdb: begin finalize memory delivery %d: %w", id, err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET payload = ?, delivery_pending = 0, delivery_inflight = 0 WHERE id = ? AND state = ? AND extraction_json = '' AND delivery_pending = 1`, payload, id, MemoryJobPendingExtraction)
	if err != nil {
		return fmt.Errorf("memdb: finalize memory delivery %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		if err != nil {
			return fmt.Errorf("memdb: inspect finalized memory delivery %d: %w", id, err)
		}
		return fmt.Errorf("memdb: held memory job %d cannot be finalized", id)
	}
	if db.memoryJobCommitErrorForTest != nil {
		if err := db.memoryJobCommitErrorForTest(tx); err != nil {
			return fmt.Errorf("memdb: injected finalize memory delivery %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memdb: commit finalized memory delivery %d: %w", id, err)
	}
	return nil
}

func (db *DB) HeldMemoryJobs() ([]MemoryJob, error) {
	return db.HeldMemoryJobsContext(context.Background())
}

func (db *DB) HeldMemoryJobsContext(ctx context.Context) ([]MemoryJob, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT id, state, payload, extraction_json, extraction_sha256, delivery_pending, delivery_sequence, delivery_inflight FROM memory_jobs WHERE delivery_pending = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("memdb: list held memory jobs: %w", err)
	}
	defer rows.Close()
	var jobs []MemoryJob
	for rows.Next() {
		var job MemoryJob
		if err := rows.Scan(&job.ID, &job.State, &job.Payload, &job.ExtractionJSON, &job.ExtractionSHA256, &job.DeliveryPending, &job.DeliverySequence, &job.DeliveryInflight); err != nil {
			return nil, fmt.Errorf("memdb: scan held memory job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memdb: list held memory jobs: %w", err)
	}
	return jobs, nil
}
