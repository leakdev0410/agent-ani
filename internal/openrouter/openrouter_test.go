package openrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ani-telegram/internal/limits"
)

func TestEncodeImageRequestUsesTempFileAndValidJSON(t *testing.T) {
	img, err := os.CreateTemp(t.TempDir(), "image-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = img.Write([]byte{1, 2, 3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	img.Close()
	c := NewClient("k", "http://example.test", "m")
	body, err := c.encodeRequest(chatRequest{Model: "m", Messages: []Message{{Role: "user", Content: NewImageContent("hello", img.Name(), "image/jpeg")}}, Temperature: .8, MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if body.path == "" || len(body.bytes) != 0 {
		t.Fatal("image request must be disk-backed")
	}
	r, err := body.Reader()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	if !strings.Contains(string(raw), "data:image/jpeg;base64,AQIDBAU=") {
		t.Fatalf("missing streamed data URI: %s", raw)
	}
}

func TestChatRetryStillUnlimitedUntilSuccess(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	old := retryInterval
	retryInterval = time.Millisecond
	defer func() { retryInterval = old }()
	c := NewClient("k", server.URL, "m")
	got, err := c.Chat(context.Background(), "system", nil, "hello", nil)
	if err != nil || got != "ok" {
		t.Fatalf("got %q err=%v", got, err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts=%d", attempts.Load())
	}
}

func TestChatJSONUsesLargeLimitAndRetriesIncompleteJSON(t *testing.T) {
	var attempts atomic.Int32
	var gotMaxTokens atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotMaxTokens.Store(int64(req.MaxTokens))
		content := `{"memory":"bị cắt`
		if attempts.Add(1) == 2 {
			content = `{"memory":"đầy đủ"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}},
		})
	}))
	defer server.Close()

	c := NewClient("k", server.URL, "m")
	got, err := c.ChatJSON(context.Background(), "system", nil, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"memory":"đầy đủ"}` {
		t.Fatalf("got %q", got)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d", attempts.Load())
	}
	const wantMaxTokens = 32 * 1024
	if gotMaxTokens.Load() != wantMaxTokens {
		t.Fatalf("max_tokens=%d, want %d", gotMaxTokens.Load(), wantMaxTokens)
	}
}

func TestChatJSONStopsAfterMalformedResponseBudget(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"cut":"off`}}},
		})
	}))
	defer server.Close()

	c := NewClient("k", server.URL, "m")
	if _, err := c.ChatJSON(context.Background(), "system", nil, "hello", nil); err == nil {
		t.Fatal("expected malformed JSON error")
	}
	if attempts.Load() != maxJSONAttempts {
		t.Fatalf("attempts=%d, want %d", attempts.Load(), maxJSONAttempts)
	}
}

func TestResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 20))) }))
	defer server.Close()
	lim := limits.Default()
	lim.OpenRouterResponseBytes = 10
	c := NewClient("k", server.URL, "m", lim)
	_, err := c.Chat(context.Background(), "s", nil, "u", nil)
	if err == nil {
		t.Fatal("expected bounded response error")
	}
}

func TestMetadataResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 20)))
	}))
	defer server.Close()
	lim := limits.Default()
	lim.OpenRouterResponseBytes = 10
	c := NewClient("k", server.URL, "m", lim)
	if _, _, err := c.ModelCapabilities("m"); err == nil {
		t.Fatal("expected bounded models response error")
	}
	if _, err := c.Providers("author/model"); err == nil {
		t.Fatal("expected bounded providers response error")
	}
}

func TestErrorsDoNotExposeProviderResponseBody(t *testing.T) {
	privateBody := "sentinel-private-provider-body"
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "api error", status: http.StatusBadRequest, body: `{"error":{"message":"` + privateBody + `"}}`},
		{name: "malformed success", status: http.StatusOK, body: `{"choices":"` + privateBody},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			c := NewClient("test-key", server.URL, "test-model")
			var err error
			if tt.status == http.StatusOK {
				body, encodeErr := c.encodeRequest(chatRequest{Model: "test-model", Messages: []Message{{Role: "user", Content: "hello"}}})
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				defer body.Close()
				_, err = c.doRequest(context.Background(), body)
			} else {
				_, err = c.Chat(context.Background(), "system", nil, "hello", nil)
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), privateBody) {
				t.Fatalf("error exposed provider response: %q", err)
			}
		})
	}
}

func TestEmbedRejectsDuplicateResponseIndexes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]},{"index":0,"embedding":[0.3,0.4]}]}`))
	}))
	defer server.Close()

	c := NewClient("test-key", server.URL, "test-model")
	if _, err := c.Embed(context.Background(), "test-model", 2, []string{"first", "second"}); err == nil {
		t.Fatal("Embed accepted duplicate response indexes and left one requested input without a vector")
	}
}

func TestEmbedStrictlyValidatesResponseIndexesAndVectors(t *testing.T) {
	tests := []struct {
		name     string
		response string
		inputs   []string
		want     [][]float32
		wantErr  bool
	}{
		{name: "missing index", response: `{"data":[{"embedding":[0.1,0.2]}]}`, inputs: []string{"only"}, wantErr: true},
		{name: "out of range index", response: `{"data":[{"index":1,"embedding":[0.1,0.2]}]}`, inputs: []string{"only"}, wantErr: true},
		{name: "wrong dimensions", response: `{"data":[{"index":0,"embedding":[0.1]}]}`, inputs: []string{"only"}, wantErr: true},
		{name: "non-finite numeric value", response: `{"data":[{"index":0,"embedding":[1e999,0.2]}]}`, inputs: []string{"only"}, wantErr: true},
		{name: "reordered indexes are restored to input order", response: `{"data":[{"index":1,"embedding":[2.1,2.2]},{"index":0,"embedding":[1.1,1.2]}]}`, inputs: []string{"first", "second"}, want: [][]float32{{1.1, 1.2}, {2.1, 2.2}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()

			got, err := NewClient("test-key", server.URL, "test-model").Embed(context.Background(), "test-model", 2, tt.inputs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Embed accepted malformed response: %s", tt.response)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("vector count = %d, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				if len(got[i]) != len(tt.want[i]) {
					t.Fatalf("vector %d = %v, want %v", i, got[i], tt.want[i])
				}
				for j := range tt.want[i] {
					if got[i][j] != tt.want[i][j] {
						t.Fatalf("vector %d value %d = %v, want %v", i, j, got[i][j], tt.want[i][j])
					}
				}
			}
		})
	}
}
