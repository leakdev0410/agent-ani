// Package openrouter là client tối giản cho OpenRouter Chat Completions API
// (tương thích OpenAI /chat/completions), chỉ dùng thư viện chuẩn của Go.
package openrouter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ani-telegram/internal/limits"
	"ani-telegram/internal/safeio"
)

// Message.Content dùng any thay vì string để chứa được cả nội dung nhiều phần (text + ảnh) theo
// chuẩn content-parts của OpenAI-compatible API — chuỗi bình thường vẫn hợp lệ vì any giữ string
// marshal y hệt "content": "...".
type Message struct {
	Role       string     `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // chỉ có khi role="assistant" và model quyết định gọi tool
	ToolCallID string     `json:"tool_call_id,omitempty"` // bắt buộc khi role="tool" — khớp với ToolCall.ID tương ứng
}

// ContentPart là 1 phần nội dung trong Message.Content dạng mảng (dùng khi gửi kèm ảnh) — theo
// đúng chuẩn content-parts OpenAI-compatible: "text" hoặc "image_url".
type ContentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL.URL nhận cả URL thật lẫn data URI base64 (VD "data:image/jpeg;base64,...") — dùng data
// URI cho ảnh tải từ Telegram để không phải forward URL chứa bot token ra ngoài.
type ImageURL struct {
	URL string `json:"url"`
}

// ImageContent is an internal, streaming representation of a multimodal user
// message. It deliberately does not implement json.Marshaler: MarshalJSON
// would have to allocate the complete base64 data URI in memory.
type ImageContent struct {
	Text string
	Path string
	MIME string
}

func NewImageContent(text, path, mime string) ImageContent {
	if strings.TrimSpace(mime) == "" {
		mime = "application/octet-stream"
	}
	return ImageContent{Text: text, Path: path, MIME: mime}
}

// ToolCall là 1 lần model yêu cầu gọi function, theo đúng format OpenAI-compatible.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // luôn "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON string, cần tự parse
	} `json:"function"`
}

// Tool mô tả 1 function model có thể gọi (OpenAI-compatible function calling).
type Tool struct {
	Type     string       `json:"type"` // luôn "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"` // JSON Schema object
}

type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client

	mu       sync.RWMutex
	model    string // đổi được lúc chạy qua SetModel (lệnh Telegram /model)
	provider string // đổi được lúc chạy qua SetProvider (lệnh Telegram /provider) — "" = tự động,
	limits   limits.Limits
	// "nodata" = chỉ nhà cung cấp không lưu trữ data (data_collection: deny), khác thì là 1 provider
	// slug/tag cụ thể để ghim (order + allow_fallbacks=false)
}

func NewClient(apiKey, baseURL, model string, configured ...limits.Limits) *Client {
	lim := limits.Default()
	if len(configured) > 0 {
		lim = configured[0]
	}
	return &Client{
		apiKey:  apiKey,
		baseURL: baseURL,
		model:   model,
		limits:  lim,
		// 120s vì system prompt ngày càng dài (toàn bộ memory ghép vào), model cần thời gian
		// xử lý input dài hơn — 60s trước đây từng bị "context deadline exceeded" khi prompt
		// lớn/API đang tải cao.
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// Model trả về model đang dùng hiện tại (dùng cho lệnh Telegram "/model" không kèm tham số).
func (c *Client) Model() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.model
}

// SetModel đổi model đang dùng lúc chạy (dùng cho lệnh Telegram "/model <tên model>"), không cần
// khởi động lại — mọi lượt chat sau đó dùng model mới ngay.
func (c *Client) SetModel(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.model = model
}

// Provider trả về giá trị nhà cung cấp đang ghim ("" = tự động, "nodata" = chỉ nhà cung cấp
// không lưu trữ data) — dùng cho lệnh Telegram "/provider" không tham số.
func (c *Client) Provider() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.provider
}

// SetProvider đổi nhà cung cấp đang ghim lúc chạy (dùng cho lệnh Telegram "/provider <tag>"),
// không cần khởi động lại.
func (c *Client) SetProvider(provider string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.provider = provider
}

type responseFormat struct {
	Type string `json:"type"`
}

// providerPreferences là field "provider" trong request /chat/completions của OpenRouter — dùng
// để ghim đúng 1 nhà cung cấp (Order + AllowFallbacks=false) hoặc lọc theo chính sách lưu data
// (DataCollection="deny") — xem https://openrouter.ai/docs/features/provider-routing.
type providerPreferences struct {
	Order          []string `json:"order,omitempty"`
	AllowFallbacks *bool    `json:"allow_fallbacks,omitempty"`
	DataCollection string   `json:"data_collection,omitempty"` // "allow" | "deny"
}

type chatRequest struct {
	Model          string               `json:"model"`
	Messages       []Message            `json:"messages"`
	Temperature    float64              `json:"temperature"`
	ResponseFormat *responseFormat      `json:"response_format,omitempty"`
	Tools          []Tool               `json:"tools,omitempty"`
	Provider       *providerPreferences `json:"provider,omitempty"`
	MaxTokens      int                  `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type embeddingRequest struct {
	Input      []string             `json:"input"`
	Model      string               `json:"model"`
	Dimensions int                  `json:"dimensions,omitempty"`
	Provider   *providerPreferences `json:"provider,omitempty"`
}
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     *int      `json:"index"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed performs one bounded embedding request. Unlike chat retries, failures
// are returned to the persistent indexing worker, which applies disk-backed
// backoff without keeping a request resident in memory.
func (c *Client) Embed(ctx context.Context, model string, dimensions int, inputs []string) ([][]float32, error) {
	if model == "" || dimensions <= 0 || len(inputs) == 0 || len(inputs) > 32 {
		return nil, fmt.Errorf("openrouter: invalid embedding request")
	}
	raw, err := json.Marshal(embeddingRequest{Input: inputs, Model: model, Dimensions: dimensions, Provider: &providerPreferences{DataCollection: "deny"}})
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > c.limits.OpenRouterRequestBytes {
		return nil, &safeio.TooLargeError{Kind: "embedding request", Limit: c.limits.OpenRouterRequestBytes}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("HTTP-Referer", "https://github.com/ani-telegram")
	req.Header.Set("X-Title", "ani-telegram")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := safeio.ReadResponse(resp, c.limits.OpenRouterResponseBytes, "embedding response")
	if err != nil {
		return nil, err
	}
	var parsed embeddingResponse
	parseErr := json.Unmarshal(body, &parsed)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiError{StatusCode: resp.StatusCode}
	}
	if parseErr != nil {
		return nil, fmt.Errorf("openrouter embedding: parse lỗi: %w (response_bytes=%d)", parseErr, len(body))
	}
	if parsed.Error != nil {
		return nil, &apiError{StatusCode: resp.StatusCode}
	}
	if len(parsed.Data) != len(inputs) {
		return nil, fmt.Errorf("openrouter embedding: got %d vectors for %d inputs", len(parsed.Data), len(inputs))
	}
	out := make([][]float32, len(inputs))
	for _, item := range parsed.Data {
		if item.Index == nil || *item.Index < 0 || *item.Index >= len(out) || out[*item.Index] != nil || len(item.Embedding) != dimensions {
			return nil, fmt.Errorf("openrouter embedding: invalid vector")
		}
		for _, value := range item.Embedding {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, fmt.Errorf("openrouter embedding: invalid vector")
			}
		}
		out[*item.Index] = item.Embedding
	}
	for _, vector := range out {
		if vector == nil {
			return nil, fmt.Errorf("openrouter embedding: invalid vector")
		}
	}
	return out, nil
}

// maxToolTurns chặn vòng lặp tool-calling không bao giờ dừng nếu model cứ liên tục gọi tool —
// đủ rộng cho vài lần load_skill liên tiếp trong 1 lượt trả lời.
const maxToolTurns = 6

const (
	// JSON memory phải chép lại cả ba đoạn mang1-3 nên dài hơn reply chat thông thường. Mốc cũ
	// 8192 token vẫn có lượt trả về JSON cụt; tạm nâng 4 lần để phân biệt trần generation với
	// output hỏng từ model/provider. Đây chỉ là trần output; model vẫn tự dừng khi JSON hoàn tất.
	jsonCompletionTokens = 32768
	chatCompletionTokens = 4096

	// JSON mode chỉ đảm bảo nhà cung cấp cố trả JSON, không đảm bảo response không bị cắt hoặc model
	// không dừng sớm. Thử lại hữu hạn để một output hỏng nhất thời không làm mất memory của lượt đó.
	maxJSONAttempts = 3
)

// retryInterval là khoảng chờ giữa các lần thử lại khi gặp lỗi tạm thời (nhà cung cấp quá tải/
// rate limit/mạng chập chờn) — thử tới khi thành công hoặc bị huỷ qua ctx (lệnh Telegram /restart),
// không còn giới hạn số lần như trước.
var retryInterval = 60 * time.Second

// apiError là lỗi HTTP status khác 2xx từ OpenRouter — giữ lại StatusCode để isRetryable phân
// biệt lỗi tạm thời (nên thử lại) với lỗi vĩnh viễn (thử lại mãi cũng không bao giờ thành công).
type apiError struct {
	StatusCode int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("openrouter: lỗi API (status %d)", e.StatusCode)
}

// isRetryable quyết định lỗi có đáng thử lại hay không:
//   - lỗi mạng/transport (không phải *apiError, VD timeout, DNS, connection refused) — tạm thời,
//     retry.
//   - *apiError với status 429 (rate limit) / 500/502/503/504 (nhà cung cấp lỗi/quá tải) — tạm
//     thời, retry.
//   - *apiError với status khác (400 sai tham số/model không hỗ trợ vision.../401 sai API key/
//     402 hết credit/403/404 sai tên model...) — VĨNH VIỄN, thử lại mãi cũng không thành công,
//     phải báo lỗi ngay thay vì lặp vô hạn.
func isRetryable(err error) bool {
	if errors.Is(err, safeio.ErrBodyTooLarge) {
		return false
	}
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case 429, 500, 502, 503, 504:
			return true
		default:
			return false
		}
	}
	return true
}

// RetryObserver được gọi mỗi khi 1 lượt gọi API gặp lỗi tạm thời và chuẩn bị chờ retryInterval
// trước khi thử lại (KHÔNG gọi khi lỗi vĩnh viễn — lúc đó trả lỗi ngay, không retry) — dùng để
// cập nhật trạng thái sống cho lệnh Telegram /status. nil nghĩa là không cần theo dõi.
type RetryObserver func(attempt int, err error)

// Chat gửi system prompt + lịch sử hội thoại + tin nhắn mới nhất của anh, trả về câu trả lời của
// Ani. userContent là string bình thường, hoặc []ContentPart khi cần gửi kèm ảnh. Gặp lỗi tạm
// thời (nhà cung cấp quá tải/rate limit/mạng chập chờn) sẽ tự thử lại mỗi retryInterval cho tới
// khi thành công hoặc ctx bị huỷ (lệnh Telegram /restart) — không còn giới hạn số lần thử.
func (c *Client) Chat(ctx context.Context, systemPrompt string, history []Message, userContent any, onRetry RetryObserver) (string, error) {
	if err := c.validateInputs(systemPrompt, userContent); err != nil {
		return "", err
	}
	messages := buildMessages(systemPrompt, history, userContent)
	msg, err := c.complete(ctx, messages, nil, nil, onRetry)
	if err != nil {
		return "", err
	}
	return c.checkedContent(msg.Content)
}

// ChatJSON giống Chat nhưng bắt model trả về đúng 1 JSON object hợp lệ (dùng cho các lượt trích
// xuất dữ liệu để tự ghi nhớ, không phải trả lời chat bình thường) — không bao giờ cần gửi kèm
// ảnh nên userMessage vẫn là string thường.
func (c *Client) ChatJSON(ctx context.Context, systemPrompt string, history []Message, userMessage string, onRetry RetryObserver) (string, error) {
	if err := c.validateInputs(systemPrompt, userMessage); err != nil {
		return "", err
	}
	messages := buildMessages(systemPrompt, history, userMessage)
	format := &responseFormat{Type: "json_object"}
	var lastErr error
	for attempt := 1; attempt <= maxJSONAttempts; attempt++ {
		result, err := c.chat(ctx, messages, format, nil, onRetry)
		if err != nil {
			return "", err
		}
		if err := validateCompletion(result, false); err != nil {
			lastErr = err
			var invalid *CompletionError
			if !errors.As(err, &invalid) || (invalid.Kind != "empty_completion" && invalid.Kind != "truncated_completion") {
				return "", err
			}
			if attempt < maxJSONAttempts {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if onRetry != nil {
					onRetry(attempt, err)
				}
				if invalid.Kind == "truncated_completion" {
					messages = withCompletionRecovery(buildMessages(systemPrompt, history, userMessage))
				}
			}
			continue
		}
		content, err := c.checkedContent(result.Message.Content)
		if err != nil {
			return "", err
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(content), &object); err == nil && object != nil {
			return content, nil
		} else if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("giá trị gốc không phải JSON object")
		}
		if attempt < maxJSONAttempts && onRetry != nil {
			onRetry(attempt, fmt.Errorf("model trả JSON không hoàn chỉnh (%d bytes): %w", len(content), lastErr))
		}
	}
	return "", fmt.Errorf("openrouter: model trả JSON không hợp lệ sau %d lần: %w", maxJSONAttempts, lastErr)
}

// ToolExecutor thực thi 1 tool call theo tên + tham số JSON thô, trả về nội dung text để đưa
// lại cho model (VD nội dung 1 file skill).
type ToolExecutor func(name, argumentsJSON string) (string, error)

// ChatWithTools giống Chat nhưng cho model quyền gọi tool (VD load_skill) trước khi trả lời
// cuối cùng — nếu model trả về tool_calls, exec từng cái qua run rồi gửi kết quả lại, lặp tới
// khi model trả lời bằng text bình thường (không tool_calls nữa) hoặc hết maxToolTurns.
func (c *Client) ChatWithTools(ctx context.Context, systemPrompt string, history []Message, userContent any, tools []Tool, run ToolExecutor, onRetry RetryObserver) (string, error) {
	if err := c.validateInputs(systemPrompt, userContent); err != nil {
		return "", err
	}
	messages := buildMessages(systemPrompt, history, userContent)
	toolBytes, toolCalls := 0, 0
	seen := make(map[string]string)

	for turn := 0; turn < maxToolTurns; turn++ {
		msg, err := c.complete(ctx, messages, nil, tools, onRetry)
		if err != nil {
			return "", err
		}
		if len(msg.ToolCalls) == 0 {
			return c.checkedContent(msg.Content)
		}

		messages = append(messages, msg)
		for _, call := range msg.ToolCalls {
			toolCalls++
			key := call.Function.Name + "\x00" + canonicalJSON(call.Function.Arguments)
			result, cached := seen[key]
			var err error
			switch {
			case toolCalls > c.limits.ToolCalls:
				result = fmt.Sprintf(`{"error":"tool call budget exceeded","limit":%d}`, c.limits.ToolCalls)
			case cached:
				// Reusing a bounded prior result prevents repeated DB reads and repeated growth.
			case toolBytes >= c.limits.ToolTotalBytes:
				result = fmt.Sprintf(`{"error":"tool byte budget exceeded","limit":%d}`, c.limits.ToolTotalBytes)
			default:
				result, err = run(call.Function.Name, call.Function.Arguments)
				if err != nil {
					result = fmt.Sprintf(`{"error":%q}`, err.Error())
				}
				if len(result) > c.limits.ToolResultBytes {
					result = fmt.Sprintf(`{"error":"tool result too large; request a smaller page","limit":%d}`, c.limits.ToolResultBytes)
				}
				remaining := c.limits.ToolTotalBytes - toolBytes
				if len(result) > remaining {
					result = fmt.Sprintf(`{"error":"tool byte budget exceeded","remaining":%d}`, remaining)
				}
				seen[key] = result
			}
			toolBytes += len(result)
			messages = append(messages, Message{Role: "tool", ToolCallID: call.ID, Content: result})
		}
	}

	return "", fmt.Errorf("openrouter: vượt quá %d lượt gọi tool mà chưa có câu trả lời cuối", maxToolTurns)
}

func (c *Client) validateInputs(systemPrompt string, userContent any) error {
	if len(systemPrompt) > c.limits.SystemPromptBytes {
		return &safeio.TooLargeError{Kind: "system prompt", Limit: int64(c.limits.SystemPromptBytes)}
	}
	var text string
	switch v := userContent.(type) {
	case string:
		text = v
	case ImageContent:
		text = v.Text
	}
	if len(text) > c.limits.UserMessageBytes {
		return &safeio.TooLargeError{Kind: "user message", Limit: int64(c.limits.UserMessageBytes)}
	}
	return nil
}

func (c *Client) checkedContent(content any) (string, error) {
	s := contentText(content)
	if len(s) > c.limits.ModelReplyBytes {
		return "", &safeio.TooLargeError{Kind: "model reply", Limit: int64(c.limits.ModelReplyBytes)}
	}
	return s, nil
}

func canonicalJSON(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		if compact, err := json.Marshal(v); err == nil {
			return string(compact)
		}
	}
	return strings.TrimSpace(raw)
}

// contentText đọc content trả lời từ API ra string an toàn — response content luôn là string
// bình thường (kể cả khi request gửi lên là content-parts), nhưng có thể là nil khi assistant
// message chỉ có tool_calls, không có text.
func contentText(content any) string {
	s, _ := content.(string)
	return s
}

// buildProviderPreferences dịch giá trị Provider() nội bộ sang field "provider" thật của request
// — "" = không gửi field (OpenRouter tự load-balance như mặc định), "nodata" = chỉ dùng nhà cung
// cấp không lưu trữ data, còn lại = ghim đúng 1 nhà cung cấp (không fallback sang nhà khác).
func buildProviderPreferences(provider string) *providerPreferences {
	switch provider {
	case "":
		return nil
	case "nodata":
		return &providerPreferences{DataCollection: "deny"}
	default:
		noFallback := false
		return &providerPreferences{Order: []string{provider}, AllowFallbacks: &noFallback}
	}
}

func buildMessages(systemPrompt string, history []Message, userContent any) []Message {
	messages := make([]Message, 0, len(history)+2)
	messages = append(messages, Message{Role: "system", Content: systemPrompt})
	for _, message := range append(append([]Message(nil), history...), Message{Role: "user", Content: userContent}) {
		if message.Role == "user" && len(messages) > 0 && messages[len(messages)-1].Role == "user" {
			previous, previousOK := messages[len(messages)-1].Content.(string)
			switch current := message.Content.(type) {
			case string:
				if previousOK {
					messages[len(messages)-1].Content = previous + "\n" + current
					continue
				}
			case ImageContent:
				if previousOK {
					current.Text = previous + "\n" + current.Text
					messages[len(messages)-1].Content = current
					continue
				}
			}
		}
		messages = append(messages, message)
	}
	return messages
}

// chat gửi request, thử lại vô hạn mỗi retryInterval khi gặp lỗi tạm thời (isRetryable) cho tới
// khi thành công hoặc ctx bị huỷ — lỗi vĩnh viễn (model không hỗ trợ vision/tools, sai API key,
// sai tên model...) trả lỗi ngay, không retry. reqBody dựng lại mỗi lần thử để nếu anh đổi model/
// nhà cung cấp qua /model, /provider giữa lúc đang retry thì lần thử tiếp theo dùng giá trị mới
// ngay, không cần đợi hết vòng retry cũ.
func (c *Client) chat(ctx context.Context, messages []Message, format *responseFormat, tools []Tool, onRetry RetryObserver) (completionResult, error) {
	for attempt := 1; ; attempt++ {
		reqBody, err := c.encodeRequest(chatRequest{
			Model:          c.Model(),
			Messages:       messages,
			Temperature:    0.8,
			ResponseFormat: format,
			Tools:          tools,
			Provider:       buildProviderPreferences(c.Provider()),
			MaxTokens:      completionLimit(format),
		})
		if err != nil {
			return completionResult{}, err
		}

		result, err := c.doRequest(ctx, reqBody)
		reqBody.Close()
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return completionResult{}, ctx.Err()
		}
		if !isRetryable(err) {
			return completionResult{}, err
		}

		if onRetry != nil {
			onRetry(attempt, err)
		}
		select {
		case <-ctx.Done():
			return completionResult{}, ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

func completionLimit(format *responseFormat) int {
	if format != nil {
		return jsonCompletionTokens
	}
	return chatCompletionTokens
}

type encodedRequest struct {
	bytes  []byte
	path   string
	length int64
}

func (b *encodedRequest) Reader() (io.ReadCloser, error) {
	if b.path != "" {
		return os.Open(b.path)
	}
	return io.NopCloser(bytes.NewReader(b.bytes)), nil
}
func (b *encodedRequest) Close() {
	if b.path != "" {
		_ = os.Remove(b.path)
	}
}

func (c *Client) encodeRequest(req chatRequest) (*encodedRequest, error) {
	var estimate int64 = 1024
	for _, m := range req.Messages {
		switch v := m.Content.(type) {
		case string:
			estimate += int64(len(v)) + 128
		case ImageContent:
			estimate += int64(len(v.Text)) + 128
			if info, err := os.Stat(v.Path); err == nil {
				estimate += (info.Size() + 2) / 3 * 4
			}
		}
	}
	if estimate > c.limits.OpenRouterRequestBytes {
		return nil, &safeio.TooLargeError{Kind: "openrouter request", Limit: c.limits.OpenRouterRequestBytes}
	}
	hasImage := false
	for _, m := range req.Messages {
		if _, ok := m.Content.(ImageContent); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		raw, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) > c.limits.OpenRouterRequestBytes {
			return nil, &safeio.TooLargeError{Kind: "openrouter request", Limit: c.limits.OpenRouterRequestBytes}
		}
		return &encodedRequest{bytes: raw, length: int64(len(raw))}, nil
	}
	f, err := os.CreateTemp("", "ani-openrouter-request-*.json")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	w := &limitWriter{w: f, limit: c.limits.OpenRouterRequestBytes}
	if err := writeStreamingRequest(w, req); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	ok = true
	return &encodedRequest{path: path, length: w.n}, nil
}

type limitWriter struct {
	w        io.Writer
	n, limit int64
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.n+int64(len(p)) > w.limit {
		return 0, &safeio.TooLargeError{Kind: "openrouter request", Limit: w.limit}
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func writeStreamingRequest(w io.Writer, req chatRequest) error {
	write := func(s string) error { _, err := io.WriteString(w, s); return err }
	field := func(name string, value any, comma bool) error {
		if comma {
			if err := write(","); err != nil {
				return err
			}
		}
		key, _ := json.Marshal(name)
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err = w.Write(key); err != nil {
			return err
		}
		if err = write(":"); err != nil {
			return err
		}
		_, err = w.Write(raw)
		return err
	}
	if err := write("{"); err != nil {
		return err
	}
	if err := field("model", req.Model, false); err != nil {
		return err
	}
	if err := write(",\"messages\":["); err != nil {
		return err
	}
	for i, m := range req.Messages {
		if i > 0 {
			if err := write(","); err != nil {
				return err
			}
		}
		img, special := m.Content.(ImageContent)
		if !special {
			raw, err := json.Marshal(m)
			if err != nil {
				return err
			}
			if _, err = w.Write(raw); err != nil {
				return err
			}
			continue
		}
		role, _ := json.Marshal(m.Role)
		textRaw, _ := json.Marshal(img.Text)
		mimeRaw, _ := json.Marshal(img.MIME)
		if _, err := fmt.Fprintf(w, "{\"role\":%s,\"content\":[{\"type\":\"text\",\"text\":%s},{\"type\":\"image_url\",\"image_url\":{\"url\":\"data:", role, textRaw); err != nil {
			return err
		}
		// MIME is restricted to DetectContentType output, but quote/unquote it to keep JSON escaping correct.
		var mime string
		_ = json.Unmarshal(mimeRaw, &mime)
		if err := write(mime + ";base64,"); err != nil {
			return err
		}
		f, err := os.Open(img.Path)
		if err != nil {
			return err
		}
		enc := base64.NewEncoder(base64.StdEncoding, w)
		_, copyErr := io.Copy(enc, f)
		closeErr := enc.Close()
		fileErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if fileErr != nil {
			return fileErr
		}
		if err := write("\"}}]}"); err != nil {
			return err
		}
	}
	if err := write("]"); err != nil {
		return err
	}
	if err := field("temperature", req.Temperature, true); err != nil {
		return err
	}
	if req.ResponseFormat != nil {
		if err := field("response_format", req.ResponseFormat, true); err != nil {
			return err
		}
	}
	if len(req.Tools) > 0 {
		if err := field("tools", req.Tools, true); err != nil {
			return err
		}
	}
	if req.Provider != nil {
		if err := field("provider", req.Provider, true); err != nil {
			return err
		}
	}
	if req.MaxTokens > 0 {
		if err := field("max_tokens", req.MaxTokens, true); err != nil {
			return err
		}
	}
	return write("}")
}

func (c *Client) doRequest(ctx context.Context, reqBody *encodedRequest) (completionResult, error) {
	body, err := reqBody.Reader()
	if err != nil {
		return completionResult{}, err
	}
	defer body.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", body)
	if err != nil {
		return completionResult{}, err
	}
	req.ContentLength = reqBody.length
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	// Header khuyến nghị của OpenRouter (không bắt buộc để API chạy, dùng cho ranking/hiển thị
	// trên openrouter.ai) — https://openrouter.ai/docs.
	req.Header.Set("HTTP-Referer", "https://github.com/ani-telegram")
	req.Header.Set("X-Title", "ani-telegram")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return completionResult{}, err
	}
	defer resp.Body.Close()

	raw, err := safeio.ReadResponse(resp, c.limits.OpenRouterResponseBytes, "openrouter response")
	if err != nil {
		return completionResult{}, err
	}

	var parsed chatResponse
	parseErr := json.Unmarshal(raw, &parsed)

	// Status khác 2xx — lỗi API thật (nhà cung cấp lỗi/quá tải/tham số sai...). Bọc vào *apiError
	// kèm StatusCode để isRetryable phân biệt tạm thời hay vĩnh viễn.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return completionResult{}, &apiError{StatusCode: resp.StatusCode}
	}
	if parseErr != nil {
		return completionResult{}, fmt.Errorf("openrouter: parse lỗi: %w (response_bytes=%d)", parseErr, len(raw))
	}
	if parsed.Error != nil {
		// Hiếm khi xảy ra (status 2xx nhưng body vẫn có field "error") — vẫn coi là lỗi API.
		return completionResult{}, &apiError{StatusCode: resp.StatusCode}
	}
	if len(parsed.Choices) == 0 {
		return completionResult{}, fmt.Errorf("openrouter: không có choices nào trong response (response_bytes=%d)", len(raw))
	}

	return completionResult{Message: parsed.Choices[0].Message, FinishReason: parsed.Choices[0].FinishReason}, nil
}
