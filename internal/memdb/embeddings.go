package memdb

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	maxEmbeddingSourceKeyBytes  = 256
	maxEmbeddingSourceTextBytes = 512 * 1024
	maxEmbeddingModelBytes      = 256
	maxEmbeddingVectorValues    = 4096
	maxEmbeddingVectorBytes     = maxEmbeddingVectorValues * 4
)

// EmbeddingSource is pending work sourced exclusively from committed memory.
type EmbeddingSource struct {
	ID            int64
	SourceKey     string
	ContentDigest string
	Text          string
}

// EmbeddingCandidate is a durable vector and the committed source text it
// represents. Callers rank these candidates in a later retrieval layer.
type EmbeddingCandidate struct {
	SourceKey     string
	ContentDigest string
	Text          string
	Model         string
	Vector        []float32
	SourceType    string
	SourceRef     string
	Topic         string
	OccurredAt    string
}

// EnqueueEmbeddingSource records only committed source text. A new digest for
// an existing source atomically replaces queued work and invalidates its stale
// vector; matching work is left untouched.
func (db *DB) EnqueueEmbeddingSource(sourceKey, contentDigest, text string) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("memdb: begin enqueue embedding: %w", err)
	}
	defer tx.Rollback()
	if err := db.EnqueueEmbeddingSourceTx(tx, sourceKey, contentDigest, text); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memdb: commit enqueue embedding: %w", err)
	}
	db.trace("WRITE", "embedding_queue:"+sourceKey, len(text))
	return nil
}

// EnqueueEmbeddingSourceTx appends committed-memory embedding work to an
// existing transaction. Its caller writes the matching committed source in
// that transaction, so source mutation, queue work, and job deletion share
// one commit boundary.
func (db *DB) EnqueueEmbeddingSourceTx(tx *sql.Tx, sourceKey, contentDigest, text string) error {
	return db.enqueueEmbeddingSourceTx(tx, sourceKey, contentDigest, text, "")
}

// enqueueEmbeddingSourceTx is the shared committed-source queue operation.
// targetModel is set only by startup synchronization; ordinary committed
// writes remain model-agnostic because the active indexer consumes them.
func (db *DB) enqueueEmbeddingSourceTx(tx *sql.Tx, sourceKey, contentDigest, text, targetModel string) error {
	if tx == nil {
		return fmt.Errorf("memdb: enqueue embedding transaction is nil")
	}
	if err := validateEmbeddingSourceKey(sourceKey); err != nil {
		return err
	}
	if len(text) == 0 || len(text) > maxEmbeddingSourceTextBytes {
		return fmt.Errorf("memdb: embedding source text is invalid")
	}
	committedText, err := committedEmbeddingText(tx, sourceKey)
	if err != nil {
		return err
	}
	if text != committedText {
		return fmt.Errorf("memdb: embedding source text is not the committed source")
	}
	if contentDigest != sha256Text(text) {
		return fmt.Errorf("memdb: embedding source content digest does not match")
	}

	var queuedDigest string
	err = tx.QueryRow("SELECT content_sha256 FROM embedding_queue WHERE source_key = ?", sourceKey).Scan(&queuedDigest)
	switch {
	case err == nil && queuedDigest == contentDigest:
		// A queue row means this source must be reindexed. During a configured
		// model startup, invalidate any prior vector even if an interrupted
		// earlier attempt left both rows behind.
		if targetModel != "" {
			if _, err := tx.Exec("DELETE FROM memory_embeddings WHERE source_key = ?", sourceKey); err != nil {
				return fmt.Errorf("memdb: delete embedding pending reindex: %w", err)
			}
		}
		return nil
	case err == nil:
		if _, err := tx.Exec("UPDATE embedding_queue SET content_sha256 = ?, text = ?, created_at = ? WHERE source_key = ?", contentDigest, text, time.Now().UTC().Format(time.RFC3339Nano), sourceKey); err != nil {
			return fmt.Errorf("memdb: replace embedding queue work: %w", err)
		}
	case err == sql.ErrNoRows:
		var storedDigest, storedModel string
		err = tx.QueryRow("SELECT content_sha256, model FROM memory_embeddings WHERE source_key = ?", sourceKey).Scan(&storedDigest, &storedModel)
		if err == nil && storedDigest == contentDigest && (targetModel == "" || storedModel == targetModel) {
			return nil
		}
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("memdb: inspect stored embedding: %w", err)
		}
		if _, err := tx.Exec("INSERT INTO embedding_queue (source_key, content_sha256, text, created_at) VALUES (?, ?, ?, ?)", sourceKey, contentDigest, text, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("memdb: add embedding queue work: %w", err)
		}
	default:
		return fmt.Errorf("memdb: inspect embedding queue: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM memory_embeddings WHERE source_key = ?", sourceKey); err != nil {
		return fmt.Errorf("memdb: delete stale embedding: %w", err)
	}
	return nil
}

// synchronizeEmbeddingSourcesTx makes the durable queue exactly reflect the
// committed FTS sources visible at startup. It is called while rebuilding the
// FTS index, so old databases and chunked topic files are backfilled in the
// same transaction and retries cannot duplicate work.
func (db *DB) synchronizeEmbeddingSourcesTx(tx *sql.Tx, embeddingModel string) error {
	if tx == nil {
		return fmt.Errorf("memdb: synchronize embedding transaction is nil")
	}
	if _, err := tx.Exec(`DELETE FROM embedding_queue
		WHERE NOT EXISTS (
			SELECT 1 FROM memory_search
			WHERE memory_search.source_type || ':' || memory_search.source_ref = embedding_queue.source_key
		)`); err != nil {
		return fmt.Errorf("memdb: remove stale embedding queue work: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM memory_embeddings
		WHERE NOT EXISTS (
			SELECT 1 FROM memory_search
			WHERE memory_search.source_type || ':' || memory_search.source_ref = memory_embeddings.source_key
		)`); err != nil {
		return fmt.Errorf("memdb: remove stale embeddings: %w", err)
	}

	rows, err := tx.Query("SELECT source_type, source_ref, text FROM memory_search ORDER BY source_type, source_ref")
	if err != nil {
		return fmt.Errorf("memdb: read committed sources for embedding sync: %w", err)
	}
	var sources []EmbeddingSource
	for rows.Next() {
		var sourceType, sourceRef string
		var source EmbeddingSource
		if err := rows.Scan(&sourceType, &sourceRef, &source.Text); err != nil {
			rows.Close()
			return fmt.Errorf("memdb: scan committed source for embedding sync: %w", err)
		}
		source.SourceKey = sourceType + ":" + sourceRef
		source.ContentDigest = sha256Text(source.Text)
		sources = append(sources, source)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("memdb: close committed sources for embedding sync: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("memdb: read committed sources for embedding sync: %w", err)
	}
	for _, source := range sources {
		if err := db.enqueueEmbeddingSourceTx(tx, source.SourceKey, source.ContentDigest, source.Text, embeddingModel); err != nil {
			return fmt.Errorf("memdb: queue committed source %q: %w", source.SourceKey, err)
		}
	}
	return nil
}

func (db *DB) NextEmbeddingSource() (EmbeddingSource, bool, error) {
	var source EmbeddingSource
	err := db.sql.QueryRow("SELECT id, source_key, content_sha256, text FROM embedding_queue ORDER BY id LIMIT 1").Scan(&source.ID, &source.SourceKey, &source.ContentDigest, &source.Text)
	if err == sql.ErrNoRows {
		return EmbeddingSource{}, false, nil
	}
	if err != nil {
		return EmbeddingSource{}, false, fmt.Errorf("memdb: read next embedding queue work: %w", err)
	}
	db.trace("READ", fmt.Sprintf("embedding_queue:%d", source.ID), len(source.Text))
	return source, true, nil
}

// EmbeddingIndexCounts returns private-safe operational totals for durable
// semantic indexing work. It never reads source text or vector values.
func (db *DB) EmbeddingIndexCounts() (queued, indexed int, err error) {
	err = db.sql.QueryRow(`SELECT
		(SELECT COUNT(*) FROM embedding_queue),
		(SELECT COUNT(*) FROM memory_embeddings)`).Scan(&queued, &indexed)
	if err != nil {
		return 0, 0, fmt.Errorf("memdb: count embedding index state: %w", err)
	}
	db.trace("READ", "embedding_index(count)", queued+indexed)
	return queued, indexed, nil
}

// OldestEmbeddingQueueCreatedAt returns only the oldest retryable queue age;
// it intentionally does not expose a source key, text, digest, or vector.
func (db *DB) OldestEmbeddingQueueCreatedAt() (time.Time, bool, error) {
	var raw string
	err := db.sql.QueryRow("SELECT created_at FROM embedding_queue ORDER BY id LIMIT 1").Scan(&raw)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("memdb: read oldest embedding queue age: %w", err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("memdb: parse oldest embedding queue age: %w", err)
	}
	return createdAt, true, nil
}

// StoreEmbedding persists one completed vector and removes exactly its queue
// row in the same transaction, leaving failed work retryable.
func (db *DB) StoreEmbedding(id int64, expectedDigest, model string, vector []float32) error {
	if strings.TrimSpace(model) == "" || len(model) > maxEmbeddingModelBytes {
		return fmt.Errorf("memdb: embedding model is invalid")
	}
	encoded, err := encodeEmbeddingVector(vector)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("memdb: begin store embedding: %w", err)
	}
	defer tx.Rollback()

	var source EmbeddingSource
	err = tx.QueryRow("SELECT id, source_key, content_sha256, text FROM embedding_queue WHERE id = ? AND content_sha256 = ?", id, expectedDigest).Scan(&source.ID, &source.SourceKey, &source.ContentDigest, &source.Text)
	if err == sql.ErrNoRows {
		return fmt.Errorf("memdb: embedding queue work %d does not exist", id)
	}
	if err != nil {
		return fmt.Errorf("memdb: read embedding queue work %d: %w", id, err)
	}
	if _, err := tx.Exec(`INSERT INTO memory_embeddings (source_key, content_sha256, text, model, vector, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_key) DO UPDATE SET
			content_sha256 = excluded.content_sha256, text = excluded.text, model = excluded.model,
			vector = excluded.vector, created_at = excluded.created_at`,
		source.SourceKey, source.ContentDigest, source.Text, model, encoded, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("memdb: store embedding %d: %w", id, err)
	}
	result, err := tx.Exec("DELETE FROM embedding_queue WHERE id = ? AND content_sha256 = ?", id, expectedDigest)
	if err != nil {
		return fmt.Errorf("memdb: delete embedding queue work %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memdb: check delete embedding queue work %d: %w", id, err)
	}
	if affected != 1 {
		return fmt.Errorf("memdb: embedding queue work %d is no longer deletable", id)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memdb: commit store embedding %d: %w", id, err)
	}
	db.trace("WRITE", fmt.Sprintf("memory_embeddings:%d", id), len(encoded))
	return nil
}

func (db *DB) SearchEmbeddingCandidates(model string) ([]EmbeddingCandidate, error) {
	return db.searchEmbeddingCandidates(model, 0)
}

// SearchEmbeddingCandidatesLimit returns a deterministic bounded subset for
// query-time similarity work. A non-positive limit returns no candidates.
func (db *DB) SearchEmbeddingCandidatesLimit(model string, limit int) ([]EmbeddingCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	return db.searchEmbeddingCandidates(model, limit)
}

func (db *DB) searchEmbeddingCandidates(model string, limit int) ([]EmbeddingCandidate, error) {
	query := `SELECT e.source_key, e.content_sha256, e.text, e.model,
		length(e.vector), CASE WHEN length(e.vector) BETWEEN 4 AND ? THEN e.vector ELSE NULL END,
		s.source_type, s.source_ref, s.topic, COALESCE(d.entry_date, o.observed_at, n.noted_at, '')
		FROM memory_embeddings AS e
		JOIN memory_search AS s
			ON e.source_key = s.source_type || ':' || s.source_ref
			AND e.text = s.text
		LEFT JOIN diary_entries AS d
			ON s.source_type = 'diary' AND s.source_ref = d.id || ':' || d.entry_date
		LEFT JOIN observations AS o
			ON s.source_type = 'observation' AND s.source_ref = o.kind || ':' || o.id
		LEFT JOIN topic_notes AS n
			ON s.source_type = 'topic_note' AND s.source_ref = n.id || ':' || n.noted_at
		WHERE e.model = ?
		ORDER BY e.source_key`
	args := []any{maxEmbeddingVectorBytes, model}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("memdb: read embedding candidates: %w", err)
	}
	defer rows.Close()

	var candidates []EmbeddingCandidate
	for rows.Next() {
		var candidate EmbeddingCandidate
		var encoded []byte
		var vectorBytes int
		if err := rows.Scan(&candidate.SourceKey, &candidate.ContentDigest, &candidate.Text, &candidate.Model, &vectorBytes, &encoded, &candidate.SourceType, &candidate.SourceRef, &candidate.Topic, &candidate.OccurredAt); err != nil {
			return nil, fmt.Errorf("memdb: scan embedding candidate: %w", err)
		}
		if candidate.ContentDigest != sha256Text(candidate.Text) {
			return nil, fmt.Errorf("memdb: embedding candidate content digest is stale")
		}
		if vectorBytes < 4 || vectorBytes > maxEmbeddingVectorBytes || vectorBytes%4 != 0 {
			return nil, fmt.Errorf("memdb: embedding candidate vector bytes are invalid")
		}
		vector, err := decodeEmbeddingVector(encoded)
		if err != nil {
			return nil, fmt.Errorf("memdb: decode embedding candidate: %w", err)
		}
		candidate.Vector = vector
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memdb: read embedding candidates: %w", err)
	}
	db.trace("READ", "memory_embeddings", len(candidates))
	return candidates, nil
}

func sha256Text(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}

func validateEmbeddingSourceKey(sourceKey string) error {
	if len(sourceKey) == 0 || len(sourceKey) > maxEmbeddingSourceKeyBytes {
		return fmt.Errorf("memdb: embedding source key is invalid")
	}
	separator := strings.IndexByte(sourceKey, ':')
	if separator < 1 || separator == len(sourceKey)-1 {
		return fmt.Errorf("memdb: embedding source key is invalid")
	}
	switch sourceKey[:separator] {
	case "diary", "observation", "topic_note", "topic":
	default:
		return fmt.Errorf("memdb: embedding source kind is not committed memory")
	}
	sourceType, sourceRef := sourceKey[:separator], sourceKey[separator+1:]
	for _, r := range sourceKey {
		if r == ' ' && sourceType == "topic_note" && isCanonicalTopicNoteTimestampRef(sourceRef) {
			continue
		}
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != ':' && r != '_' && r != '-' && r != '.' && r != '/' {
			return fmt.Errorf("memdb: embedding source key is not canonical")
		}
	}
	return nil
}

// isCanonicalTopicNoteTimestampRef accepts the timestamp spelling memtopic
// uses when it becomes part of the committed topic_note source key.
func isCanonicalTopicNoteTimestampRef(sourceRef string) bool {
	id, notedAt, ok := strings.Cut(sourceRef, ":")
	if !ok || len(id) == 0 || id[0] == '0' {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	parsed, err := time.Parse("2006-01-02 15:04", notedAt)
	return err == nil && parsed.Format("2006-01-02 15:04") == notedAt
}

func committedEmbeddingText(tx *sql.Tx, sourceKey string) (string, error) {
	separator := strings.IndexByte(sourceKey, ':')
	sourceType, sourceRef := sourceKey[:separator], sourceKey[separator+1:]
	var text string
	err := tx.QueryRow("SELECT text FROM memory_search WHERE source_type = ? AND source_ref = ?", sourceType, sourceRef).Scan(&text)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("memdb: embedding source is not committed memory")
	}
	if err != nil {
		return "", fmt.Errorf("memdb: read committed embedding source: %w", err)
	}
	return text, nil
}

func encodeEmbeddingVector(vector []float32) ([]byte, error) {
	if len(vector) == 0 || len(vector) > maxEmbeddingVectorValues {
		return nil, fmt.Errorf("memdb: embedding vector is invalid")
	}
	buf := bytes.NewBuffer(make([]byte, 0, len(vector)*4))
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("memdb: embedding vector is not finite")
		}
		if err := binary.Write(buf, binary.LittleEndian, value); err != nil {
			return nil, fmt.Errorf("memdb: encode embedding vector: %w", err)
		}
	}
	return buf.Bytes(), nil
}

func decodeEmbeddingVector(encoded []byte) ([]float32, error) {
	if len(encoded) == 0 || len(encoded) > maxEmbeddingVectorBytes || len(encoded)%4 != 0 {
		return nil, fmt.Errorf("invalid vector bytes")
	}
	vector := make([]float32, len(encoded)/4)
	if err := binary.Read(bytes.NewReader(encoded), binary.LittleEndian, &vector); err != nil {
		return nil, err
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("vector contains a non-finite value")
		}
	}
	return vector, nil
}
