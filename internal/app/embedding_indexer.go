package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
)

const embeddingVectorDimensions = 1536

// startOptionalEmbeddingIndexer starts durable semantic indexing only when an
// operator configured an embedding model. An empty model deliberately leaves
// the bot on its local FTS-only recall path.
func startOptionalEmbeddingIndexer(ctx context.Context, db *memdb.DB, llm *openrouter.Client, model string, wake <-chan struct{}) bool {
	if strings.TrimSpace(model) == "" {
		return false
	}
	go embeddingIndexer(ctx, db, llm, model, wake)
	return true
}

// embeddingIndexer drains durable, committed-memory embedding work. It does
// not read memory jobs: failed requests leave their queue row intact so a
// later wake or process restart can retry it safely.
func embeddingIndexer(ctx context.Context, db *memdb.DB, llm *openrouter.Client, model string, wake <-chan struct{}) {
	for {
		worked, err := processNextEmbeddingSource(ctx, db, llm, model)
		if err != nil {
			// Do not log source text or vectors. The queue row remains durable.
			log.Printf("embedding indexer: queued work remains for retry")
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-time.After(time.Minute):
		}
	}
}

func processNextEmbeddingSource(ctx context.Context, db *memdb.DB, llm *openrouter.Client, model string) (bool, error) {
	if db == nil || llm == nil {
		return false, fmt.Errorf("embedding indexer dependencies are nil")
	}
	source, ok, err := db.NextEmbeddingSource()
	if err != nil || !ok {
		return false, err
	}
	vectors, err := llm.Embed(ctx, model, embeddingVectorDimensions, []string{source.Text})
	if err != nil {
		return false, fmt.Errorf("embed queued source %d: %w", source.ID, err)
	}
	if len(vectors) != 1 {
		return false, fmt.Errorf("embed queued source %d: invalid vector count", source.ID)
	}
	if err := db.StoreEmbedding(source.ID, source.ContentDigest, model, vectors[0]); err != nil {
		return false, fmt.Errorf("store queued source %d: %w", source.ID, err)
	}
	return true, nil
}
