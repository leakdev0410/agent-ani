// Package memsearch exposes SQLite FTS5 memory recall to the chat model.
package memsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
)

const (
	ToolName                   = "search_memory"
	resultLimit                = 8
	embeddingDimensions        = 1536
	hybridCandidateLimit       = 32
	lexicalRankWeight          = 0.35
	semanticRankWeight         = 0.30
	cosineSimilarityWeight     = 0.25
	recencyWeight              = 0.07
	sourceTypeImportanceWeight = 0.03
)

// EmbeddingClient is the small query-time boundary used for hybrid recall.
// The OpenRouter client implements it, while tests can exercise retrieval
// deterministically without making a network request.
type EmbeddingClient interface {
	Embed(context.Context, string, int, []string) ([][]float32, error)
}

// Tool registers semantic-like keyword recall only after there is indexed data
// to search. Search syntax is sanitized by memdb.SearchMemory.
func Tool(db *memdb.DB) (openrouter.Tool, bool, error) {
	count, err := db.SearchIndexCount()
	if err != nil {
		return openrouter.Tool{}, false, err
	}
	if count == 0 {
		return openrouter.Tool{}, false, nil
	}
	return openrouter.Tool{
		Type: "function",
		Function: openrouter.ToolFunction{
			Name:        ToolName,
			Description: "Tìm các đoạn ký ức liên quan theo từ khóa khi không biết chính xác ngày hoặc chủ đề cần nhớ. Chỉ gọi khi thật sự cần nhớ lại thông tin cũ.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Các từ khóa cụ thể cần tìm trong ký ức."},
				},
				"required": []string{"query"},
			},
		},
	}, true, nil
}

type args struct {
	Query string `json:"query"`
}

// HybridSearch combines a bounded FTS candidate list with committed vectors.
// FTS is intentionally fetched first and is returned unchanged if semantic
// retrieval is disabled or any embedding configuration, API, or vector check
// fails; recall must never make a chat fail or silently reorder FTS results.
func HybridSearch(ctx context.Context, db *memdb.DB, embedder EmbeddingClient, model, query string, limit int) ([]memdb.SearchResult, error) {
	if db == nil {
		return nil, fmt.Errorf("memsearch: database is nil")
	}
	fts, err := db.SearchMemory(query, limit)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return fts, nil
	}
	if embedder == nil || strings.TrimSpace(model) == "" {
		return hybridFallback(fts), nil
	}

	vectors, err := embedder.Embed(ctx, model, embeddingDimensions, []string{query})
	if err != nil || len(vectors) != 1 || !validVector(vectors[0]) {
		return hybridFallback(fts), nil
	}
	candidates, err := db.SearchEmbeddingCandidatesLimit(model, hybridCandidateLimit)
	if err != nil {
		return hybridFallback(fts), nil
	}
	if len(candidates) == 0 {
		return hybridFallback(fts), nil
	}

	ftsCandidates, err := db.SearchMemory(query, boundedCandidateLimit(limit))
	if err != nil {
		return hybridFallback(fts), nil
	}
	semantic, ok := semanticResults(vectors[0], candidates)
	if !ok {
		return hybridFallback(fts), nil
	}
	return fuseResults(ftsCandidates, semantic, limit), nil
}

func hybridFallback(fts []memdb.SearchResult) []memdb.SearchResult {
	// The message intentionally excludes the query, source text, API response,
	// and vectors. These are memory content and must not enter operational logs.
	log.Print("semantic memory search unavailable; using FTS-only results")
	return fts
}

func boundedCandidateLimit(limit int) int {
	if limit <= 0 {
		return 0
	}
	if limit > hybridCandidateLimit/4 {
		return hybridCandidateLimit
	}
	return limit * 4
}

type semanticResult struct {
	result     memdb.SearchResult
	sourceKey  string
	similarity float64
}

func semanticResults(query []float32, candidates []memdb.EmbeddingCandidate) ([]semanticResult, bool) {
	results := make([]semanticResult, 0, len(candidates))
	for _, candidate := range candidates {
		similarity, ok := cosineSimilarity(query, candidate.Vector)
		if !ok {
			return nil, false
		}
		results = append(results, semanticResult{
			result:     memdb.SearchResult{SourceType: candidate.SourceType, SourceRef: candidate.SourceRef, Topic: candidate.Topic, Text: candidate.Text, OccurredAt: candidate.OccurredAt},
			sourceKey:  candidate.SourceKey,
			similarity: similarity,
		})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].similarity != results[j].similarity {
			return results[i].similarity > results[j].similarity
		}
		return results[i].sourceKey < results[j].sourceKey
	})
	return results, true
}

type fusedResult struct {
	result    memdb.SearchResult
	sourceKey string
	score     float64
}

func fuseResults(lexical []memdb.SearchResult, semantic []semanticResult, limit int) []memdb.SearchResult {
	merged := make(map[string]*fusedResult, len(lexical)+len(semantic))
	for index, result := range lexical {
		key := result.SourceType + ":" + result.SourceRef
		item := merged[key]
		if item == nil {
			item = &fusedResult{result: result, sourceKey: key}
			merged[key] = item
		}
		item.score += lexicalRankWeight * normalizedRank(index, len(lexical))
	}
	for index, result := range semantic {
		item := merged[result.sourceKey]
		if item == nil {
			item = &fusedResult{result: result.result, sourceKey: result.sourceKey}
			merged[result.sourceKey] = item
		}
		item.score += semanticRankWeight*normalizedRank(index, len(semantic)) + cosineSimilarityWeight*((result.similarity+1)/2)
	}
	items := make([]fusedResult, 0, len(merged))
	for _, item := range merged {
		item.score += recencyWeight*recencyScore(item.result.OccurredAt, time.Now()) + sourceTypeImportanceWeight*sourceTypeImportance(item.result.SourceType)
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].sourceKey < items[j].sourceKey
	})
	if limit > len(items) {
		limit = len(items)
	}
	out := make([]memdb.SearchResult, 0, limit)
	for _, item := range items[:limit] {
		out = append(out, item.result)
	}
	return out
}

func normalizedRank(index, count int) float64 {
	if count <= 1 {
		return 1
	}
	return 1 - float64(index)/float64(count-1)
}

func cosineSimilarity(left, right []float32) (float64, bool) {
	if len(left) == 0 || len(left) != len(right) || !validVector(left) || !validVector(right) {
		return 0, false
	}
	var dot, leftNorm, rightNorm float64
	for i := range left {
		l, r := float64(left[i]), float64(right[i])
		dot += l * r
		leftNorm += l * l
		rightNorm += r * r
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0, false
	}
	return dot / math.Sqrt(leftNorm*rightNorm), true
}

func validVector(vector []float32) bool {
	if len(vector) == 0 {
		return false
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}

func recencyScore(occurredAt string, now time.Time) float64 {
	when, err := time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		when, err = time.Parse("2006-01-02", occurredAt)
		if err != nil {
			return 0
		}
	}
	age := now.Sub(when).Hours() / 24
	if age <= 0 {
		return 1
	}
	if age >= 365 {
		return 0
	}
	return 1 - age/365
}

func sourceTypeImportance(sourceType string) float64 {
	switch sourceType {
	case "observation":
		return 1
	case "topic_note":
		return .8
	case "diary":
		return .6
	case "topic":
		return .4
	default:
		return 0
	}
}

// Executor searches FTS5 and returns source-labelled snippets for the model to
// decide whether a more precise recall tool is needed.
func Executor(db *memdb.DB) openrouter.ToolExecutor {
	return executor(db, nil, "")
}

// ExecutorWithEmbeddings uses hybrid recall for tool calls when an optional
// embedding model is configured. HybridSearch preserves FTS-only behavior if
// the embedding service or its durable state is unavailable.
func ExecutorWithEmbeddings(db *memdb.DB, embedder EmbeddingClient, model string) openrouter.ToolExecutor {
	return executor(db, embedder, model)
}

func executor(db *memdb.DB, embedder EmbeddingClient, model string) openrouter.ToolExecutor {
	return func(name, argumentsJSON string) (string, error) {
		if name != ToolName {
			return "", fmt.Errorf("memsearch: tool lạ %q", name)
		}
		var a args
		if err := json.Unmarshal([]byte(argumentsJSON), &a); err != nil {
			return "", fmt.Errorf("memsearch: tham số không hợp lệ: %w", err)
		}
		query := strings.TrimSpace(a.Query)
		if query == "" {
			return "", fmt.Errorf("memsearch: query không được rỗng")
		}

		db.Note("🔧 Model gọi search_memory — đang tìm ký ức liên quan")
		results, err := HybridSearch(context.Background(), db, embedder, model, query, resultLimit)
		if err != nil {
			return "", err
		}
		if len(results) == 0 {
			return "Không tìm thấy ký ức phù hợp.", nil
		}

		var b strings.Builder
		b.WriteString("Kết quả tìm ký ức:\n")
		for _, result := range results {
			fmt.Fprintf(&b, "\n- [%s | %s | %s] %s", result.SourceType, result.Topic, result.SourceRef, result.Text)
		}
		return b.String(), nil
	}
}
