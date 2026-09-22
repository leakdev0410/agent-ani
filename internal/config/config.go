// Package config nạp cấu hình từ biến môi trường (và file .env nếu có),
// không phụ thuộc thư viện ngoài.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"ani-telegram/internal/limits"
)

type Config struct {
	BotToken                 string // ANI_TELEGRAM_BOT_TOKEN
	OpenRouterAPIKey         string // OPENROUTER_API_KEY
	OpenRouterModel          string // OPENROUTER_MODEL, mặc định "deepseek/deepseek-chat" — đổi được lúc chạy qua lệnh Telegram /model
	OpenRouterEmbeddingModel string // OPENROUTER_EMBEDDING_MODEL, tùy chọn; rỗng = chỉ dùng FTS5
	OpenRouterBaseURL        string // OPENROUTER_BASE_URL, mặc định "https://openrouter.ai/api/v1"
	MemoryDir                string // ANI_MEMORY_DIR, mặc định "./memory" — chỉ dùng để seed memory.db 1 lần khi DB còn trống
	MemoryDBPath             string // ANI_MEMORY_DB, mặc định "./memory.db" — SQLite, nguồn thật của bộ nhớ
	OffsetFile               string // ANI_OFFSET_FILE, mặc định "offset.txt" — offset Telegram getUpdates
	PersonaPath              string // ANI_PERSONA_PATH, rỗng = dùng persona nhúng sẵn (internal/persona/ani.txt). ANI_GROK_PROMPT_PATH vẫn được nhận như alias cũ.
	PollTimeoutSec           int    // thời gian long-poll Telegram getUpdates
	AllowedUserID            int64  // ANI_ALLOWED_USER_ID, mặc định 1 (giá trị test — đổi thành ID Telegram thật của anh); chỉ user này được bot trả lời
	AllowedChatID            int64  // ANI_ALLOWED_CHAT_ID, bắt buộc; chỉ private chat này được bot xử lý/gửi proactive
	Limits                   limits.Limits
}

// Load đọc .env (nếu tồn tại) rồi đọc biến môi trường, trả lỗi rõ ràng nếu thiếu key bắt buộc.
func Load() (*Config, error) {
	loadDotEnvBestEffort(".env")

	lim := limits.Default()
	var err error
	if lim.TelegramResponseBytes, err = getenvPositiveInt64("ANI_MAX_TELEGRAM_RESPONSE_BYTES", lim.TelegramResponseBytes); err != nil {
		return nil, err
	}
	if lim.OpenRouterResponseBytes, err = getenvPositiveInt64("ANI_MAX_OPENROUTER_RESPONSE_BYTES", lim.OpenRouterResponseBytes); err != nil {
		return nil, err
	}
	if lim.ImageBytes, err = getenvPositiveInt64("ANI_MAX_IMAGE_BYTES", lim.ImageBytes); err != nil {
		return nil, err
	}
	if lim.SystemPromptBytes, err = getenvPositiveInt("ANI_MAX_SYSTEM_PROMPT_BYTES", lim.SystemPromptBytes); err != nil {
		return nil, err
	}
	if lim.ModelReplyBytes, err = getenvPositiveInt("ANI_MAX_MODEL_REPLY_BYTES", lim.ModelReplyBytes); err != nil {
		return nil, err
	}
	if lim.ToolResultBytes, err = getenvPositiveInt("ANI_MAX_TOOL_RESULT_BYTES", lim.ToolResultBytes); err != nil {
		return nil, err
	}
	if lim.ToolTotalBytes, err = getenvPositiveInt("ANI_MAX_TOOL_TOTAL_BYTES", lim.ToolTotalBytes); err != nil {
		return nil, err
	}
	if lim.ImportFileBytes, err = getenvPositiveInt64("ANI_MAX_IMPORT_FILE_BYTES", lim.ImportFileBytes); err != nil {
		return nil, err
	}

	allowedChatID, err := getenvRequiredPositiveInt64("ANI_ALLOWED_CHAT_ID")
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		BotToken:                 os.Getenv("ANI_TELEGRAM_BOT_TOKEN"),
		OpenRouterAPIKey:         os.Getenv("OPENROUTER_API_KEY"),
		OpenRouterModel:          getenvDefault("OPENROUTER_MODEL", "deepseek/deepseek-chat"),
		OpenRouterEmbeddingModel: strings.TrimSpace(os.Getenv("OPENROUTER_EMBEDDING_MODEL")),
		OpenRouterBaseURL:        getenvDefault("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
		MemoryDir:                getenvDefault("ANI_MEMORY_DIR", "./memory"),
		MemoryDBPath:             getenvDefault("ANI_MEMORY_DB", "./memory.db"),
		OffsetFile:               getenvDefault("ANI_OFFSET_FILE", "offset.txt"),
		PersonaPath:              firstNonEmpty(os.Getenv("ANI_PERSONA_PATH"), os.Getenv("ANI_GROK_PROMPT_PATH")),
		PollTimeoutSec:           getenvIntDefault("ANI_POLL_TIMEOUT_SEC", 30),
		AllowedUserID:            getenvInt64Default("ANI_ALLOWED_USER_ID", 1),
		AllowedChatID:            allowedChatID,
		Limits:                   lim,
	}

	var missing []string
	if cfg.BotToken == "" {
		missing = append(missing, "ANI_TELEGRAM_BOT_TOKEN")
	}
	if cfg.OpenRouterAPIKey == "" {
		missing = append(missing, "OPENROUTER_API_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("thiếu biến môi trường bắt buộc: %s (xem .env.example)", strings.Join(missing, ", "))
	}

	return cfg, nil
}

func getenvRequiredPositiveInt64(key string) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, fmt.Errorf("thiếu biến môi trường bắt buộc: %s (xem .env.example)", key)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s phải là số nguyên dương", key)
	}
	return n, nil
}

func getenvPositiveInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s phải là số nguyên dương", key)
	}
	return n, nil
}

func getenvPositiveInt64(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s phải là số nguyên dương", key)
	}
	return n, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvIntDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvInt64Default(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// loadDotEnvBestEffort đọc file .env dạng KEY=VALUE đơn giản, bỏ qua nếu file không tồn tại.
// Không ghi đè biến môi trường đã được set sẵn (VD từ shell hoặc CI).
func loadDotEnvBestEffort(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}
