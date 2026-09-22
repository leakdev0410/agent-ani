package memdb

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddingSourceChangedDigestReplacesStaleQueuedWork(t *testing.T) {
	db := newEmbeddingTestDB(t)
	sourceKey, diaryID := addCommittedDiarySource(t, db, "first")
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("first"), "first"); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	if _, err := db.sql.Exec("UPDATE diary_entries SET text = ? WHERE id = ?", "changed", diaryID); err != nil {
		t.Fatalf("replace committed diary text: %v", err)
	}
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("changed"), "changed"); err != nil {
		t.Fatalf("enqueue changed: %v", err)
	}

	source, ok, err := db.NextEmbeddingSource()
	if err != nil {
		t.Fatalf("NextEmbeddingSource: %v", err)
	}
	if !ok {
		t.Fatal("NextEmbeddingSource returned no source")
	}
	if source.SourceKey != sourceKey || source.ContentDigest != digestForTest("changed") || source.Text != "changed" {
		t.Fatalf("source = %+v, want changed durable source", source)
	}
	var count int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM embedding_queue").Scan(&count); err != nil {
		t.Fatalf("count embedding queue: %v", err)
	}
	if count != 1 {
		t.Fatalf("queue count = %d, want 1", count)
	}
}

func TestEmbeddingSourceStoreDeletesQueueAndMakesCandidateReadable(t *testing.T) {
	db := newEmbeddingTestDB(t)
	sourceKey, _ := addCommittedDiarySource(t, db, "likes tea")
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("likes tea"), "likes tea"); err != nil {
		t.Fatalf("EnqueueEmbeddingSource: %v", err)
	}
	source, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok {
		t.Fatalf("NextEmbeddingSource = (%+v, %v, %v)", source, ok, err)
	}

	if err := db.StoreEmbedding(source.ID, source.ContentDigest, "text-embedding-test", []float32{0.25, -0.5}); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if _, ok, err := db.NextEmbeddingSource(); err != nil || ok {
		t.Fatalf("queue should be empty after StoreEmbedding: ok=%v err=%v", ok, err)
	}
	candidates, err := db.SearchEmbeddingCandidates("text-embedding-test")
	if err != nil {
		t.Fatalf("SearchEmbeddingCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	candidate := candidates[0]
	if candidate.SourceKey != sourceKey || candidate.ContentDigest != digestForTest("likes tea") || candidate.Text != "likes tea" || candidate.Model != "text-embedding-test" {
		t.Fatalf("candidate = %+v", candidate)
	}
	if len(candidate.Vector) != 2 || candidate.Vector[0] != 0.25 || candidate.Vector[1] != -0.5 {
		t.Fatalf("candidate vector = %v", candidate.Vector)
	}
}

// This fails if startup rebuild cannot synchronize the canonical source key
// emitted for topic notes whose committed timestamp uses the application
// format "2006-01-02 15:04".
func TestRebuildSearchIndexQueuesTopicNoteWithSpaceSeparatedTimestamp(t *testing.T) {
	db := newEmbeddingTestDB(t)
	const (
		note    = "call the dentist"
		notedAt = "2026-08-15 11:49"
	)
	if err := db.AddTopicNote("personal", note, notedAt); err != nil {
		t.Fatalf("AddTopicNote: %v", err)
	}

	if err := db.RebuildSearchIndexForEmbeddingModel("embedding-test"); err != nil {
		t.Fatalf("RebuildSearchIndexForEmbeddingModel: %v", err)
	}
	source, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok {
		t.Fatalf("NextEmbeddingSource = (%+v, %v, %v)", source, ok, err)
	}
	if got, want := source.SourceKey, "topic_note:1:2026-08-15 11:49"; got != want {
		t.Fatalf("queued source key = %q, want %q", got, want)
	}
	if source.ContentDigest != digestForTest(note) || source.Text != note {
		t.Fatalf("queued topic note = %+v, want committed text and digest", source)
	}
}

func TestValidateEmbeddingSourceKeyOnlyAllowsCanonicalTopicNoteTimestampSpaces(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{
			name:   "canonical committed topic note",
			source: "topic_note:100:2026-08-15 11:49",
		},
		{
			name:    "transient memory job",
			source:  "memory_jobs:1",
			wantErr: true,
		},
		{
			name:    "topic note control character",
			source:  "topic_note:100:2026-08-15 11:49\n",
			wantErr: true,
		},
		{
			name:    "topic note noncanonical spaced suffix",
			source:  "topic_note:100:2026-08-15 11:49 extra",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEmbeddingSourceKey(tt.source)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateEmbeddingSourceKey(%q) error = %v, wantErr %v", tt.source, err, tt.wantErr)
			}
		})
	}
}

// This fails if reopening an indexed database with a different configured
// embedding model leaves same-digest vectors unqueued or eligible for recall.
func TestRebuildSearchIndexForEmbeddingModelRequeuesAfterModelChangeAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const text = "committed source remains unchanged"
	const modelA = "embedding-model-a"
	const modelB = "embedding-model-b"
	if err := db.AddDiaryEntry("2026-08-26", text, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RebuildSearchIndexForEmbeddingModel(modelA); err != nil {
		t.Fatalf("initial model-A startup rebuild: %v", err)
	}
	first, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok {
		t.Fatalf("initial model-A queue work = (%+v, %v, %v)", first, ok, err)
	}
	if err := db.StoreEmbedding(first.ID, first.ContentDigest, modelA, []float32{1, 0}); err != nil {
		t.Fatalf("store model-A vector: %v", err)
	}
	if candidates, err := db.SearchEmbeddingCandidates(modelA); err != nil || len(candidates) != 1 {
		t.Fatalf("model-A candidate before restart = %+v, %v", candidates, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.RebuildSearchIndexForEmbeddingModel(modelB); err != nil {
		t.Fatalf("model-B startup rebuild: %v", err)
	}
	if candidates, err := db.SearchEmbeddingCandidates(modelA); err != nil || len(candidates) != 0 {
		t.Fatalf("old-model candidates survive model change: %+v, %v", candidates, err)
	}
	if candidates, err := db.SearchEmbeddingCandidates(modelB); err != nil || len(candidates) != 0 {
		t.Fatalf("new-model candidate appeared before reindex: %+v, %v", candidates, err)
	}
	requeued, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok || requeued.SourceKey != "diary:1:2026-08-26" || requeued.ContentDigest != digestForTest(text) || requeued.Text != text {
		t.Fatalf("model-change queue work = (%+v, %v, %v)", requeued, ok, err)
	}
	if err := db.StoreEmbedding(requeued.ID, requeued.ContentDigest, modelB, []float32{0, 1}); err != nil {
		t.Fatalf("store model-B vector: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.RebuildSearchIndexForEmbeddingModel(modelB); err != nil {
		t.Fatalf("same-model restart rebuild: %v", err)
	}
	if _, ok, err := db.NextEmbeddingSource(); err != nil || ok {
		t.Fatalf("same-model reopen queued duplicate work: ok=%v err=%v", ok, err)
	}
	candidates, err := db.SearchEmbeddingCandidates(modelB)
	if err != nil || len(candidates) != 1 || candidates[0].Text != text || candidates[0].Model != modelB {
		t.Fatalf("model-B candidate after reindex = %+v, %v", candidates, err)
	}
}

func TestStoreEmbeddingRollsBackWhenQueueDeleteFails(t *testing.T) {
	db := newEmbeddingTestDB(t)
	sourceKey, _ := addCommittedDiarySource(t, db, "retry after delete failure")
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("retry after delete failure"), "retry after delete failure"); err != nil {
		t.Fatal(err)
	}
	source, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok {
		t.Fatalf("NextEmbeddingSource = (%+v, %v, %v)", source, ok, err)
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER fail_embedding_queue_delete BEFORE DELETE ON embedding_queue
		BEGIN SELECT RAISE(ABORT, 'forced queue delete failure'); END`); err != nil {
		t.Fatal(err)
	}

	if err := db.StoreEmbedding(source.ID, source.ContentDigest, "text-embedding-test", []float32{0.25, -0.5}); err == nil {
		t.Fatal("StoreEmbedding succeeded despite forced queue delete failure")
	}
	if candidates, err := db.SearchEmbeddingCandidates("text-embedding-test"); err != nil || len(candidates) != 0 {
		t.Fatalf("failed store made a vector visible: candidates=%+v err=%v", candidates, err)
	}
	retry, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok || retry.ID != source.ID || retry.ContentDigest != source.ContentDigest {
		t.Fatalf("failed store did not retain retryable queue work: source=%+v ok=%v err=%v", retry, ok, err)
	}
}

func TestEmbeddingSourceRejectsInFlightVectorAfterDigestReplacement(t *testing.T) {
	db := newEmbeddingTestDB(t)
	sourceKey, diaryID := addCommittedDiarySource(t, db, "old committed text")
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("old committed text"), "old committed text"); err != nil {
		t.Fatalf("enqueue old source: %v", err)
	}
	old, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok {
		t.Fatalf("read old source: source=%+v ok=%v err=%v", old, ok, err)
	}
	if _, err := db.sql.Exec("UPDATE diary_entries SET text = ? WHERE id = ?", "new committed text", diaryID); err != nil {
		t.Fatalf("replace committed diary text: %v", err)
	}
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("new committed text"), "new committed text"); err != nil {
		t.Fatalf("enqueue replacement source: %v", err)
	}

	if err := db.StoreEmbedding(old.ID, old.ContentDigest, "text-embedding-test", []float32{0.25, -0.5}); err == nil {
		t.Fatal("StoreEmbedding accepted an obsolete in-flight vector")
	}
	if candidates, err := db.SearchEmbeddingCandidates("text-embedding-test"); err != nil || len(candidates) != 0 {
		t.Fatalf("obsolete vector was persisted: candidates=%+v err=%v", candidates, err)
	}
	replacement, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok || replacement.ContentDigest != digestForTest("new committed text") || replacement.Text != "new committed text" {
		t.Fatalf("replacement work was not retained: source=%+v ok=%v err=%v", replacement, ok, err)
	}
}

func TestEmbeddingSourceRejectsDigestThatDoesNotMatchCommittedText(t *testing.T) {
	db := newEmbeddingTestDB(t)
	sourceKey, _ := addCommittedDiarySource(t, db, "committed text")
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("different"), "committed text"); err == nil {
		t.Fatal("EnqueueEmbeddingSource accepted a digest unrelated to the committed text")
	}
}

func TestEmbeddingSourceRejectsTransientMemoryJobContent(t *testing.T) {
	db := newEmbeddingTestDB(t)
	payload := "private transient payload"
	jobID, err := db.CreateMemoryJob([]byte(payload))
	if err != nil {
		t.Fatalf("CreateMemoryJob: %v", err)
	}
	if err := db.StoreExtraction(jobID, `{"private":"transient extraction"}`); err != nil {
		t.Fatalf("StoreExtraction: %v", err)
	}

	if err := db.EnqueueEmbeddingSource("memory_jobs:1", digestForTest(payload), payload); err == nil {
		t.Fatal("EnqueueEmbeddingSource accepted transient memory job payload")
	}
	if _, ok, err := db.NextEmbeddingSource(); err != nil || ok {
		t.Fatalf("transient content entered embedding queue: ok=%v err=%v", ok, err)
	}
	if candidates, err := db.SearchEmbeddingCandidates("text-embedding-test"); err != nil || len(candidates) != 0 {
		t.Fatalf("transient content entered stored embeddings: candidates=%+v err=%v", candidates, err)
	}
}

func TestEmbeddingSourceRejectsTextNotReadFromCommittedSource(t *testing.T) {
	db := newEmbeddingTestDB(t)
	text := "uncommitted imitation"
	if err := db.EnqueueEmbeddingSource("diary:404:2026-08-26", digestForTest(text), text); err == nil {
		t.Fatal("EnqueueEmbeddingSource accepted text not owned by a committed source")
	}
}

func TestEmbeddingSourceRejectsOversizedPersistedInputs(t *testing.T) {
	db := newEmbeddingTestDB(t)
	text := "committed source"
	if err := db.EnqueueEmbeddingSource("diary:"+strings.Repeat("1", 1024), digestForTest(text), text); err == nil {
		t.Fatal("EnqueueEmbeddingSource accepted an oversized source key")
	}
	if err := db.EnqueueEmbeddingSource("diary:1:2026-08-26", digestForTest(strings.Repeat("x", 600*1024)), strings.Repeat("x", 600*1024)); err == nil {
		t.Fatal("EnqueueEmbeddingSource accepted oversized source text")
	}
	sourceKey, _ := addCommittedDiarySource(t, db, text)
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest(text), text); err != nil {
		t.Fatalf("enqueue bounded source: %v", err)
	}
	source, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok {
		t.Fatalf("NextEmbeddingSource: source=%+v ok=%v err=%v", source, ok, err)
	}
	if err := db.StoreEmbedding(source.ID, source.ContentDigest, strings.Repeat("m", 1024), []float32{0.1}); err == nil {
		t.Fatal("StoreEmbedding accepted an oversized model ID")
	}
	if err := db.StoreEmbedding(source.ID, source.ContentDigest, "text-embedding-test", make([]float32, 5000)); err == nil {
		t.Fatal("StoreEmbedding accepted an oversized vector")
	}
	if _, ok, err := db.NextEmbeddingSource(); err != nil || !ok {
		t.Fatalf("rejected persisted input removed queued work: ok=%v err=%v", ok, err)
	}
}

func TestEmbeddingSourceChangedDigestDeletesPreviouslyStoredVector(t *testing.T) {
	db := newEmbeddingTestDB(t)
	sourceKey, diaryID := addCommittedDiarySource(t, db, "old")
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("old"), "old"); err != nil {
		t.Fatalf("enqueue old: %v", err)
	}
	old, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok {
		t.Fatalf("read old source: source=%+v ok=%v err=%v", old, ok, err)
	}
	if err := db.StoreEmbedding(old.ID, old.ContentDigest, "text-embedding-test", []float32{0.1}); err != nil {
		t.Fatalf("store old embedding: %v", err)
	}
	if _, err := db.sql.Exec("UPDATE diary_entries SET text = ? WHERE id = ?", "new", diaryID); err != nil {
		t.Fatalf("replace committed diary text: %v", err)
	}
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("new"), "new"); err != nil {
		t.Fatalf("enqueue new: %v", err)
	}
	if candidates, err := db.SearchEmbeddingCandidates("text-embedding-test"); err != nil || len(candidates) != 0 {
		t.Fatalf("stale vector was retained: candidates=%+v err=%v", candidates, err)
	}
}

// This fails if an embedding whose stored text no longer matches the committed
// FTS source remains eligible for semantic retrieval.
func TestSearchEmbeddingCandidatesExcludesStaleCommittedSources(t *testing.T) {
	db := newEmbeddingTestDB(t)
	sourceKey, diaryID := addCommittedDiarySource(t, db, "old committed text")
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("old committed text"), "old committed text"); err != nil {
		t.Fatal(err)
	}
	source, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok {
		t.Fatalf("NextEmbeddingSource = (%+v, %v, %v)", source, ok, err)
	}
	if err := db.StoreEmbedding(source.ID, source.ContentDigest, "embedding-test", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := db.InTx(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE diary_entries SET text = ? WHERE id = ?", "new committed text", diaryID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	candidates, err := db.SearchEmbeddingCandidates("embedding-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("stale candidates remain visible: %+v", candidates)
	}
}

// This fails if a query-time caller cannot bound vector reads deterministically
// before doing in-process similarity ranking.
func TestSearchEmbeddingCandidatesLimitBoundsBySourceKey(t *testing.T) {
	db := newEmbeddingTestDB(t)
	for _, text := range []string{"first", "second"} {
		sourceKey, _ := addCommittedDiarySource(t, db, text)
		if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest(text), text); err != nil {
			t.Fatal(err)
		}
		source, ok, err := db.NextEmbeddingSource()
		if err != nil || !ok {
			t.Fatalf("NextEmbeddingSource = (%+v, %v, %v)", source, ok, err)
		}
		if err := db.StoreEmbedding(source.ID, source.ContentDigest, "embedding-test", []float32{1, 0}); err != nil {
			t.Fatal(err)
		}
	}

	candidates, err := db.SearchEmbeddingCandidatesLimit("embedding-test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(candidates), 1; got != want {
		t.Fatalf("candidate count = %d, want %d", got, want)
	}
	if got, want := candidates[0].SourceKey, "diary:1:2026-08-26"; got != want {
		t.Fatalf("first bounded candidate = %q, want %q", got, want)
	}
}

// A corrupt durable row must be rejected without materializing an unbounded
// vector into the process. The read boundary must apply the same value limit
// as StoreEmbedding, and the decoder must reject it as a second defense.
func TestSearchEmbeddingCandidatesLimitRejectsOversizedPersistedVector(t *testing.T) {
	db := newEmbeddingTestDB(t)
	sourceKey, _ := addCommittedDiarySource(t, db, "bounded vector source")
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest("bounded vector source"), "bounded vector source"); err != nil {
		t.Fatal(err)
	}
	source, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok {
		t.Fatalf("NextEmbeddingSource = (%+v, %v, %v)", source, ok, err)
	}
	if err := db.StoreEmbedding(source.ID, source.ContentDigest, "embedding-test", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec("UPDATE memory_embeddings SET vector = zeroblob(?) WHERE source_key = ?", (maxEmbeddingVectorValues+1)*4, sourceKey); err != nil {
		t.Fatal(err)
	}

	if _, err := db.SearchEmbeddingCandidatesLimit("embedding-test", 1); err == nil {
		t.Fatal("SearchEmbeddingCandidatesLimit accepted an oversized persisted vector")
	}
}

func TestEmbeddingSourceVerboseTraceDoesNotContainSourceText(t *testing.T) {
	db := newEmbeddingTestDB(t)
	var trace bytes.Buffer
	db.logger.SetOutput(&trace)
	db.SetVerbose(true)
	secret := "committed embedding text must not be logged"
	sourceKey, _ := addCommittedDiarySource(t, db, secret)
	if err := db.EnqueueEmbeddingSource(sourceKey, digestForTest(secret), secret); err != nil {
		t.Fatalf("EnqueueEmbeddingSource: %v", err)
	}
	if strings.Contains(trace.String(), secret) {
		t.Fatalf("verbose trace exposed source text: %q", trace.String())
	}
}

func newEmbeddingTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func digestForTest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmtSHA256(sum)
}

func addCommittedDiarySource(t *testing.T, db *DB, text string) (string, int64) {
	t.Helper()
	const entryDate = "2026-08-26"
	if err := db.AddDiaryEntry(entryDate, text, ""); err != nil {
		t.Fatalf("AddDiaryEntry: %v", err)
	}
	var id int64
	if err := db.sql.QueryRow("SELECT id FROM diary_entries ORDER BY id DESC LIMIT 1").Scan(&id); err != nil {
		t.Fatalf("read diary ID: %v", err)
	}
	return fmt.Sprintf("diary:%d:%s", id, entryDate), id
}
