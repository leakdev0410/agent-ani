package config

import "testing"

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("ANI_TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("ANI_ALLOWED_CHAT_ID", "42")
}

func TestDefaultMemoryPathsUseSQLiteDB(t *testing.T) {
	setRequired(t)
	t.Setenv("ANI_MEMORY_DIR", "")
	t.Setenv("ANI_MEMORY_DB", "")
	t.Setenv("ANI_OFFSET_FILE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryDir != "./memory" {
		t.Fatalf("MemoryDir=%q, want ./memory", cfg.MemoryDir)
	}
	if cfg.MemoryDBPath != "./memory.db" {
		t.Fatalf("MemoryDBPath=%q, want ./memory.db", cfg.MemoryDBPath)
	}
	if cfg.OffsetFile != "offset.txt" {
		t.Fatalf("OffsetFile=%q, want offset.txt", cfg.OffsetFile)
	}
}

func TestAllowedChatIDIsRequiredAndPositive(t *testing.T) {
	t.Setenv("ANI_TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	for _, value := range []string{"", "0", "-1", "not-a-number"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ANI_ALLOWED_CHAT_ID", value)
			if _, err := Load(); err == nil {
				t.Fatalf("ANI_ALLOWED_CHAT_ID=%q phải bị từ chối", value)
			}
		})
	}
	t.Setenv("ANI_ALLOWED_CHAT_ID", "4242")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowedChatID != 4242 {
		t.Fatalf("AllowedChatID=%d, want 4242", cfg.AllowedChatID)
	}
}

// This fails if optional semantic retrieval is accidentally enabled by default
// or if its model selection cannot be supplied through the documented env var.
func TestOptionalEmbeddingModelConfiguration(t *testing.T) {
	setRequired(t)
	t.Setenv("OPENROUTER_EMBEDDING_MODEL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenRouterEmbeddingModel != "" {
		t.Fatalf("empty optional embedding model = %q, want disabled", cfg.OpenRouterEmbeddingModel)
	}

	t.Setenv("OPENROUTER_EMBEDDING_MODEL", "openai/text-embedding-3-small")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenRouterEmbeddingModel != "openai/text-embedding-3-small" {
		t.Fatalf("OpenRouterEmbeddingModel = %q", cfg.OpenRouterEmbeddingModel)
	}
}
