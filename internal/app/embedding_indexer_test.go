package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
)

func TestEmbeddingIndexerStoresCommittedVectorAndDeletesQueueWork(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const text = "committed diary memory"
	if err := db.AddDiaryEntry("2026-08-26", text, ""); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	if err := db.EnqueueEmbeddingSource("diary:1:2026-08-26", digest, text); err != nil {
		t.Fatal(err)
	}

	served := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Dimensions int      `json:"dimensions"`
			Input      []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode embedding request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(request.Input) != 1 || request.Input[0] != text {
			t.Errorf("embedding input = %q, want the committed source", request.Input)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		vector := make([]float32, request.Dimensions)
		vector[0] = 0.25
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vector}}})
		close(served)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go embeddingIndexer(ctx, db, openrouter.NewClient("test-key", server.URL, "chat-model"), "embedding-model", make(chan struct{}))

	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("indexer did not call the embedding endpoint")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		candidates, err := db.SearchEmbeddingCandidates("embedding-model")
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) == 1 {
			candidate := candidates[0]
			if candidate.SourceKey != "diary:1:2026-08-26" || candidate.ContentDigest != digest || candidate.Text != text || len(candidate.Vector) == 0 || candidate.Vector[0] != 0.25 {
				t.Fatalf("stored candidate = %+v", candidate)
			}
			if _, ok, err := db.NextEmbeddingSource(); err != nil || ok {
				t.Fatalf("successful indexing must delete queue work: ok=%v err=%v", ok, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("indexer did not persist the embedding")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// This fails if an unset optional embedding model starts background semantic
// work instead of preserving the FTS-only runtime.
func TestStartOptionalEmbeddingIndexerIsDisabledWithoutModel(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if started := startOptionalEmbeddingIndexer(context.Background(), db, nil, "", make(chan struct{}, 1)); started {
		t.Fatal("empty embedding model must keep semantic indexing disabled")
	}
}

func TestEmbeddingIndexerFailureSurvivesDatabaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const text = "retryable committed memory"
	if err := db.AddDiaryEntry("2026-08-26", text, ""); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	if err := db.EnqueueEmbeddingSource("diary:1:2026-08-26", digest, text); err != nil {
		t.Fatal(err)
	}

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var request struct {
			Dimensions int `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode embedding request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		vector := make([]float32, request.Dimensions)
		vector[0] = 0.5
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vector}}})
	}))
	defer server.Close()
	llm := openrouter.NewClient("test-key", server.URL, "chat-model")

	if _, err := processNextEmbeddingSource(context.Background(), db, llm, "embedding-model"); err == nil {
		t.Fatal("failed embedding request must leave queue work recoverable")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if worked, err := processNextEmbeddingSource(context.Background(), db, llm, "embedding-model"); err != nil || !worked {
		t.Fatalf("reopened indexer = worked:%v err:%v", worked, err)
	}
	candidates, err := db.SearchEmbeddingCandidates("embedding-model")
	if err != nil || len(candidates) != 1 || candidates[0].Text != text {
		t.Fatalf("reopened index candidate = %+v err=%v", candidates, err)
	}
}

func TestEmbeddingIndexerRejectsInFlightVectorForReplacedDigest(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const oldText = "old committed diary memory"
	const newText = "new committed diary memory"
	if err := db.AddDiaryEntry("2026-08-26", oldText, ""); err != nil {
		t.Fatal(err)
	}
	oldDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(oldText)))
	if err := db.EnqueueEmbeddingSource("diary:1:2026-08-26", oldDigest, oldText); err != nil {
		t.Fatal(err)
	}

	requestStarted := make(chan struct{})
	allowResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Dimensions int `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode embedding request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		close(requestStarted)
		<-allowResponse
		vector := make([]float32, request.Dimensions)
		vector[0] = 0.75
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vector}}})
	}))
	defer server.Close()

	done := make(chan error, 1)
	go func() {
		_, err := processNextEmbeddingSource(context.Background(), db, openrouter.NewClient("test-key", server.URL, "chat-model"), "embedding-model")
		done <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("indexer did not request the old source")
	}
	if err := db.InTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE diary_entries SET text = ? WHERE id = 1", newText); err != nil {
			return err
		}
		newDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(newText)))
		return db.EnqueueEmbeddingSourceTx(tx, "diary:1:2026-08-26", newDigest, newText)
	}); err != nil {
		t.Fatal(err)
	}
	close(allowResponse)
	if err := <-done; err == nil {
		t.Fatal("old in-flight vector was accepted after the committed digest changed")
	}
	if candidates, err := db.SearchEmbeddingCandidates("embedding-model"); err != nil || len(candidates) != 0 {
		t.Fatalf("old vector was stored: candidates=%+v err=%v", candidates, err)
	}
	source, ok, err := db.NextEmbeddingSource()
	newDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(newText)))
	if err != nil || !ok || source.ContentDigest != newDigest || source.Text != newText {
		t.Fatalf("replacement queue work = %+v ok=%v err=%v", source, ok, err)
	}
}
