package memsearch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"log"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ani-telegram/internal/memdb"
)

func TestTool_HiddenWhenIndexEmpty(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, ok, err := Tool(db)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("search tool must not be registered for an empty index")
	}
}

func TestExecutor_ReturnsOnlyRelevantMemory(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.AddDiaryEntry("2026-08-26", "10:00: deadline dự án Ani vào thứ Sáu", "projects"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddDiaryEntry("2026-08-26", "20:00: xem phim tối nay", "personal"); err != nil {
		t.Fatal(err)
	}

	got, err := Executor(db)(ToolName, `{"query":"deadline dự án Ani"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "deadline dự án Ani vào thứ Sáu") {
		t.Errorf("search result is missing the relevant entry: %q", got)
	}
	if strings.Contains(got, "xem phim tối nay") {
		t.Errorf("search result contains an unrelated entry: %q", got)
	}
	if !strings.Contains(got, "diary") || !strings.Contains(got, "projects") {
		t.Errorf("search result must identify its source and topic: %q", got)
	}
}

// This fails if semantic-only candidates cannot join the result set or a
// lexical candidate is discarded when semantic ranking is enabled.
func TestHybridSearchPromotesSemanticMatchAndKeepsLexicalMatch(t *testing.T) {
	db := newHybridSearchTestDB(t)
	if err := db.AddDiaryEntry("2026-08-20", "dự án cần mua laptop cho nhóm", "projects"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddObservation(memdb.ObservationPreference, "Anh thích máy tính di động gọn nhẹ", "2026-08-26"); err != nil {
		t.Fatal(err)
	}
	storeHybridVectors(t, db, "embedding-test", map[string][]float32{
		"diary:1:2026-08-20":       {0, 1},
		"observation:preference:1": {1, 0},
	})

	results, err := HybridSearch(context.Background(), db, hybridEmbedder{vector: []float32{1, 0}}, "embedding-test", "mua laptop", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sourceKeys(results), []string{"observation:preference:1", "diary:1:2026-08-20"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("HybridSearch source keys = %v, want %v", got, want)
	}
}

// This fails if the tool boundary bypasses hybrid retrieval even after an
// operator has explicitly enabled an embedding model.
func TestExecutorWithEmbeddingsUsesHybridResults(t *testing.T) {
	db := newHybridSearchTestDB(t)
	if err := db.AddDiaryEntry("2026-08-20", "dự án cần mua laptop cho nhóm", "projects"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddObservation(memdb.ObservationPreference, "Anh thích máy tính di động gọn nhẹ", "2026-08-26"); err != nil {
		t.Fatal(err)
	}
	storeHybridVectors(t, db, "embedding-test", map[string][]float32{
		"diary:1:2026-08-20":       {0, 1},
		"observation:preference:1": {1, 0},
	})

	got, err := ExecutorWithEmbeddings(db, hybridEmbedder{vector: []float32{1, 0}}, "embedding-test")(ToolName, `{"query":"mua laptop"}`)
	if err != nil {
		t.Fatal(err)
	}
	preference := strings.Index(got, "Anh thích máy tính di động gọn nhẹ")
	diary := strings.Index(got, "dự án cần mua laptop cho nhóm")
	if preference < 0 || diary < 0 || preference > diary {
		t.Fatalf("tool did not expose hybrid ranking: %q", got)
	}
}

// This fails if an embedding-side problem leaks a partial hybrid ordering
// instead of preserving the original FTS answer exactly.
func TestHybridSearchEmbeddingFailuresKeepOriginalFTSResults(t *testing.T) {
	db := newHybridSearchTestDB(t)
	if err := db.AddDiaryEntry("2026-08-26", "mua laptop cho dự án Ani", "projects"); err != nil {
		t.Fatal(err)
	}
	fts, err := db.SearchMemory("mua laptop", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(fts) == 0 {
		t.Fatal("test setup did not create an FTS result")
	}

	tests := []struct {
		name     string
		embedder hybridEmbedder
		model    string
	}{
		{name: "disabled embeddings", model: ""},
		{name: "missing stored vectors", embedder: hybridEmbedder{vector: []float32{1, 0}}, model: "embedding-test"},
		{name: "embedding API failure", embedder: hybridEmbedder{err: errors.New("offline")}, model: "embedding-test"},
		{name: "wrong vector count", embedder: hybridEmbedder{vectors: [][]float32{{1, 0}, {0, 1}}}, model: "embedding-test"},
		{name: "non-finite query vector", embedder: hybridEmbedder{vector: []float32{float32(math.NaN()), 0}}, model: "embedding-test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HybridSearch(context.Background(), db, tt.embedder, tt.model, "mua laptop", 4)
			if err != nil {
				t.Fatalf("HybridSearch returned an embedding failure: %v", err)
			}
			if !reflect.DeepEqual(got, fts) {
				t.Fatalf("HybridSearch = %+v, want unchanged FTS result %+v", got, fts)
			}
		})
	}
}

func TestHybridSearchLogsPrivateSafeFallbackForConfiguredNilClient(t *testing.T) {
	db := newHybridSearchTestDB(t)
	secretQuery := "query that must not enter the operational log"
	if err := db.AddDiaryEntry("2026-08-26", secretQuery, "private-topic"); err != nil {
		t.Fatal(err)
	}
	fts, err := db.SearchMemory(secretQuery, 4)
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	got, err := HybridSearch(context.Background(), db, nil, "embedding-test", secretQuery, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, fts) {
		t.Fatalf("HybridSearch = %+v, want unchanged FTS %+v", got, fts)
	}
	if message := output.String(); !strings.Contains(message, "semantic memory search unavailable") || strings.Contains(message, secretQuery) || strings.Contains(message, "private-topic") {
		t.Fatalf("fallback log is missing or exposed private memory: %q", message)
	}
}

// This fails if a configured semantic search with no committed vector
// candidates skips its private-safe operational fallback log or mutates the
// original FTS result slice.
func TestHybridSearchLogsPrivateSafeFallbackForMissingVectorCandidates(t *testing.T) {
	db := newHybridSearchTestDB(t)
	secretQuery := "query with no vector must not enter the operational log"
	if err := db.AddDiaryEntry("2026-08-26", secretQuery, "private-topic"); err != nil {
		t.Fatal(err)
	}
	fts, err := db.SearchMemory(secretQuery, 4)
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	got, err := HybridSearch(context.Background(), db, hybridEmbedder{vector: []float32{1, 0}}, "embedding-test", secretQuery, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, fts) {
		t.Fatalf("HybridSearch = %+v, want byte-for-byte and order unchanged FTS %+v", got, fts)
	}
	if message := output.String(); !strings.Contains(message, "semantic memory search unavailable") || strings.Contains(message, secretQuery) || strings.Contains(message, "private-topic") {
		t.Fatalf("fallback log is missing or exposed private memory: %q", message)
	}
}

func TestHybridSearchStoredVectorReadFailuresKeepOriginalFTSResults(t *testing.T) {
	for _, tt := range []struct {
		name   string
		vector []byte
	}{
		{name: "non-finite stored value", vector: float32Bytes(float32(math.NaN()), 0)},
		{name: "oversized stored blob", vector: make([]byte, 4097*4)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := newHybridSearchTestDB(t)
			const query = "candidate read failure must preserve fts"
			if err := db.AddDiaryEntry("2026-08-26", query, "projects"); err != nil {
				t.Fatal(err)
			}
			storeHybridVectors(t, db, "embedding-test", map[string][]float32{"diary:1:2026-08-26": {1, 0}})
			if err := db.InTx(func(tx *sql.Tx) error {
				_, err := tx.Exec("UPDATE memory_embeddings SET vector = ? WHERE source_key = ?", tt.vector, "diary:1:2026-08-26")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			fts, err := db.SearchMemory(query, 4)
			if err != nil {
				t.Fatal(err)
			}
			got, err := HybridSearch(context.Background(), db, hybridEmbedder{vector: []float32{1, 0}}, "embedding-test", query, 4)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, fts) {
				t.Fatalf("HybridSearch = %+v, want unchanged FTS %+v", got, fts)
			}
		})
	}
}

func TestEmbeddingCandidatesCarryCanonicalRecencyMetadata(t *testing.T) {
	db := newHybridSearchTestDB(t)
	if err := db.AddDiaryEntry("2025-08-27", "old diary candidate", "projects"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddDiaryEntry("2026-08-26", "new diary candidate", "projects"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddObservation(memdb.ObservationPreference, "new observation candidate", "2026-08-26T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTopicNote("projects", "new topic note candidate", "2026-08-26T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	storeHybridVectors(t, db, "embedding-test", map[string][]float32{
		"diary:1:2025-08-27":                {1, 0},
		"diary:2:2026-08-26":                {1, 0},
		"observation:preference:1":          {1, 0},
		"topic_note:1:2026-08-26T12:00:00Z": {1, 0},
	})

	candidates, err := db.SearchEmbeddingCandidatesLimit("embedding-test", 8)
	if err != nil {
		t.Fatal(err)
	}
	when := map[string]string{}
	for _, candidate := range candidates {
		when[candidate.SourceKey] = candidate.OccurredAt
	}
	if got, want := when["diary:1:2025-08-27"], "2025-08-27"; got != want {
		t.Fatalf("old diary occurred at = %q, want %q", got, want)
	}
	for _, key := range []string{"diary:2:2026-08-26", "observation:preference:1", "topic_note:1:2026-08-26T12:00:00Z"} {
		if when[key] == "" {
			t.Fatalf("candidate %q is missing temporal metadata", key)
		}
	}
	clock := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	if !(recencyScore(when["diary:2:2026-08-26"], clock) > recencyScore(when["diary:1:2025-08-27"], clock)) {
		t.Fatalf("new diary recency must exceed old diary: new=%v old=%v", recencyScore(when["diary:2:2026-08-26"], clock), recencyScore(when["diary:1:2025-08-27"], clock))
	}
	if recencyScore(when["observation:preference:1"], clock) == 0 || recencyScore(when["topic_note:1:2026-08-26T12:00:00Z"], clock) == 0 {
		t.Fatalf("timestamped observation and topic note must receive recency credit: %+v", when)
	}
}

// These exact vectors give cosine similarities 1 and 3/5. Opposite lexical
// and semantic ranks then yield the same complete fused score for both
// observation results, so source-key ordering is the only valid final rule.
func TestFuseResultsUsesSourceKeyForActualEqualScores(t *testing.T) {
	semantic, ok := semanticResults([]float32{1, 0}, []memdb.EmbeddingCandidate{
		{SourceKey: "observation:z", SourceType: "observation", SourceRef: "z", Text: "semantic first", Vector: []float32{1, 0}},
		{SourceKey: "observation:a", SourceType: "observation", SourceRef: "a", Text: "semantic second", Vector: []float32{3, 4}},
	})
	if !ok {
		t.Fatal("semanticResults rejected finite equal-score test vectors")
	}
	lexical := []memdb.SearchResult{
		{SourceType: "observation", SourceRef: "a", Text: "semantic second"},
		{SourceType: "observation", SourceRef: "z", Text: "semantic first"},
	}
	// Both results have the same source type and no timestamp, so their
	// source-importance and recency contributions are also identical.
	aScore := lexicalRankWeight + cosineSimilarityWeight*((semantic[1].similarity+1)/2)
	zScore := semanticRankWeight + cosineSimilarityWeight*((semantic[0].similarity+1)/2)
	if aScore != zScore {
		t.Fatalf("fixture must produce identical composite scores: a=%0.17g z=%0.17g", aScore, zScore)
	}
	for iteration := 0; iteration < 100; iteration++ {
		got := fuseResults(lexical, semantic, 2)
		if want := []string{"observation:a", "observation:z"}; !reflect.DeepEqual(sourceKeys(got), want) {
			t.Fatalf("equal-score source keys on iteration %d = %v, want %v", iteration, sourceKeys(got), want)
		}
	}
}

// This fails if equal hybrid scores produce database iteration order instead
// of a stable source-key ordering.
func TestHybridSearchUsesStableSourceKeyTieOrder(t *testing.T) {
	db := newHybridSearchTestDB(t)
	if err := db.AddObservation(memdb.ObservationTone, "semantic candidate alpha", "2026-08-26"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddObservation(memdb.ObservationPreference, "semantic candidate beta", "2026-08-26"); err != nil {
		t.Fatal(err)
	}
	storeHybridVectors(t, db, "embedding-test", map[string][]float32{
		"observation:tone:1":       {1, 0},
		"observation:preference:2": {1, 0},
	})

	got, err := HybridSearch(context.Background(), db, hybridEmbedder{vector: []float32{1, 0}}, "embedding-test", "unmatched query", 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"observation:preference:2", "observation:tone:1"}; !reflect.DeepEqual(sourceKeys(got), want) {
		t.Fatalf("tie source keys = %v, want %v", sourceKeys(got), want)
	}
}

type hybridEmbedder struct {
	vector  []float32
	vectors [][]float32
	err     error
}

func (e hybridEmbedder) Embed(_ context.Context, _ string, _ int, _ []string) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	if e.vectors != nil {
		return e.vectors, nil
	}
	return [][]float32{e.vector}, nil
}

func newHybridSearchTestDB(t *testing.T) *memdb.DB {
	t.Helper()
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func storeHybridVectors(t *testing.T, db *memdb.DB, model string, vectors map[string][]float32) {
	t.Helper()
	if err := db.RebuildSearchIndex(); err != nil {
		t.Fatal(err)
	}
	for {
		source, ok, err := db.NextEmbeddingSource()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return
		}
		vector, ok := vectors[source.SourceKey]
		if !ok {
			t.Fatalf("no test vector for %q", source.SourceKey)
		}
		if err := db.StoreEmbedding(source.ID, source.ContentDigest, model, vector); err != nil {
			t.Fatal(err)
		}
	}
}

func sourceKeys(results []memdb.SearchResult) []string {
	keys := make([]string, 0, len(results))
	for _, result := range results {
		keys = append(keys, result.SourceType+":"+result.SourceRef)
	}
	return keys
}

func float32Bytes(values ...float32) []byte {
	encoded := make([]byte, len(values)*4)
	for index, value := range values {
		binary.LittleEndian.PutUint32(encoded[index*4:], math.Float32bits(value))
	}
	return encoded
}
