package openrouter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"ani-telegram/internal/safeio"
)

// ModelCapabilities tóm tắt những gì bot thực sự cần từ 1 model OpenRouter — dùng để báo trước
// cho anh khi đổi model bằng lệnh Telegram /model, tránh phải tự tra rồi thử lỗi.
type ModelCapabilities struct {
	Tools  bool // "tools" trong supported_parameters — bắt buộc cho load_skill/recall_memory/recall_topic/recall_observations
	JSON   bool // "response_format" trong supported_parameters — bắt buộc cho auto-update-memory (ChatJSON)
	Vision bool // "image" trong architecture.input_modalities — bắt buộc để xem ảnh anh gửi
}

type modelsListResponse struct {
	Data []struct {
		ID                  string   `json:"id"`
		SupportedParameters []string `json:"supported_parameters"`
		Architecture        struct {
			InputModalities []string `json:"input_modalities"`
		} `json:"architecture"`
	} `json:"data"`
}

// ModelCapabilities tra cứu khả năng của modelID qua GET /models của OpenRouter (danh sách công
// khai, không cần key riêng nhưng vẫn gửi kèm apiKey nếu có). found=false nghĩa là modelID không
// có trong danh sách (gõ sai tên, hoặc model quá mới/đã gỡ) — không phải lỗi.
func (c *Client) ModelCapabilities(modelID string) (caps ModelCapabilities, found bool, err error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return ModelCapabilities{}, false, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ModelCapabilities{}, false, err
	}
	defer resp.Body.Close()

	raw, err := safeio.ReadResponse(resp, c.limits.OpenRouterResponseBytes, "openrouter models response")
	if err != nil {
		return ModelCapabilities{}, false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ModelCapabilities{}, false, fmt.Errorf("openrouter: models HTTP %d", resp.StatusCode)
	}

	var parsed modelsListResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ModelCapabilities{}, false, fmt.Errorf("openrouter: parse danh sách model lỗi: %w", err)
	}

	for _, m := range parsed.Data {
		if m.ID != modelID {
			continue
		}
		return ModelCapabilities{
			Tools:  containsStr(m.SupportedParameters, "tools"),
			JSON:   containsStr(m.SupportedParameters, "response_format"),
			Vision: containsStr(m.Architecture.InputModalities, "image"),
		}, true, nil
	}
	return ModelCapabilities{}, false, nil
}

func containsStr(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

// ProviderEndpoint là 1 nhà cung cấp đang host 1 model cụ thể trên OpenRouter — giá cả/context
// khác nhau giữa các nhà cung cấp cùng host 1 model. Tag là giá trị dùng để ghim qua Client.
// SetProvider / field "provider.order" trong request (không phải Name — Name chỉ để hiển thị).
type ProviderEndpoint struct {
	Name                string  // provider_name, VD "DeepInfra" — chỉ để hiển thị
	Tag                 string  // tag, VD "deepinfra/fp4" — dùng để ghim (khớp field "order" của OpenRouter)
	PromptPricePerM     float64 // USD / 1 triệu token input
	CompletionPricePerM float64 // USD / 1 triệu token output
	ContextLength       int
}

type endpointsResponse struct {
	Data struct {
		Endpoints []struct {
			ProviderName  string `json:"provider_name"`
			Tag           string `json:"tag"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`     // USD/token, dạng string (VD "0.0000002574")
				Completion string `json:"completion"` // USD/token, dạng string
			} `json:"pricing"`
		} `json:"endpoints"`
	} `json:"data"`
}

// Providers liệt kê mọi nhà cung cấp đang host modelID (GET /models/{author}/{slug}/endpoints) —
// dùng cho lệnh Telegram /providers (xem danh sách + giá) và /provider <tag> (kiểm tra hợp lệ
// trước khi ghim). modelID phải đúng dạng "author/slug" (VD "deepseek/deepseek-chat").
func (c *Client) Providers(modelID string) ([]ProviderEndpoint, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/models/"+modelID+"/endpoints", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := safeio.ReadResponse(resp, c.limits.OpenRouterResponseBytes, "openrouter providers response")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openrouter: providers HTTP %d", resp.StatusCode)
	}

	var parsed endpointsResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("openrouter: parse danh sách nhà cung cấp lỗi: %w (response_bytes=%d)", err, len(raw))
	}

	out := make([]ProviderEndpoint, 0, len(parsed.Data.Endpoints))
	for _, e := range parsed.Data.Endpoints {
		promptPrice, _ := strconv.ParseFloat(e.Pricing.Prompt, 64)
		completionPrice, _ := strconv.ParseFloat(e.Pricing.Completion, 64)
		out = append(out, ProviderEndpoint{
			Name:                e.ProviderName,
			Tag:                 e.Tag,
			PromptPricePerM:     promptPrice * 1_000_000,
			CompletionPricePerM: completionPrice * 1_000_000,
			ContextLength:       e.ContextLength,
		})
	}
	return out, nil
}
