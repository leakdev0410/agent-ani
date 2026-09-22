package app

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"ani-telegram/internal/openrouter"
)

func isNewCommand(text string) bool { return matchesCommand(text, newCommand) }

func matchesCommand(text, command string) bool {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return false
	}
	cmd := strings.SplitN(fields[0], "@", 2)[0]
	return strings.EqualFold(cmd, command)
}

func parseArgCommand(text, command string) (string, bool) {
	fields := strings.SplitN(strings.TrimSpace(text), " ", 2)
	if len(fields) == 0 || fields[0] == "" {
		return "", false
	}
	cmd := strings.SplitN(fields[0], "@", 2)[0]
	if !strings.EqualFold(cmd, command) {
		return "", false
	}
	if len(fields) == 1 {
		return "", true
	}
	return strings.TrimSpace(fields[1]), true
}

func saveSetting(store settingsStore, key, value, command string) {
	if store == nil {
		return
	}
	if err := store.SetSetting(key, value); err != nil {
		log.Printf("%s: lỗi lưu %s (vẫn áp dụng trong RAM): %v", command, key, err)
	}
}

func describeCurrentProvider(llm *openrouter.Client) string {
	switch provider := llm.Provider(); provider {
	case "":
		return "tự động"
	case "nodata":
		return "chỉ provider không lưu data"
	default:
		return provider
	}
}

func listProviders(llm *openrouter.Client, modelID string) string {
	endpoints, err := llm.Providers(modelID)
	if err != nil {
		log.Printf("/providers: %v", err)
		return "Không tra được provider lúc này, anh thử lại sau nha."
	}
	if len(endpoints) == 0 {
		return "Không thấy provider nào cho model " + modelID + "."
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].PromptPricePerM < endpoints[j].PromptPricePerM })
	var b strings.Builder
	fmt.Fprintf(&b, "Provider cho %s (USD/1M token):\n", modelID)
	for _, endpoint := range endpoints {
		fmt.Fprintf(&b, "- %s (tag: %s) — input $%.3f, output $%.3f, context %d\n",
			endpoint.Name, endpoint.Tag, endpoint.PromptPricePerM, endpoint.CompletionPricePerM, endpoint.ContextLength)
	}
	return strings.TrimSpace(b.String())
}

func handleProviderCommand(store settingsStore, llm *openrouter.Client, arg string) string {
	switch {
	case arg == "":
		return "Nhà cung cấp: " + describeCurrentProvider(llm)
	case strings.EqualFold(arg, "off") || strings.EqualFold(arg, "auto"):
		llm.SetProvider("")
		saveSetting(store, "current_provider", "", providerCommand)
		return "Đã bỏ ghim provider."
	case strings.EqualFold(arg, "nodata"):
		llm.SetProvider("nodata")
		saveSetting(store, "current_provider", "nodata", providerCommand)
		return "Đã chỉ dùng provider không lưu data."
	default:
		endpoints, err := llm.Providers(llm.Model())
		if err != nil {
			return "Không tra được provider lúc này, anh thử lại sau nha."
		}
		for _, endpoint := range endpoints {
			if strings.EqualFold(endpoint.Tag, arg) || strings.EqualFold(endpoint.Name, arg) {
				llm.SetProvider(endpoint.Tag)
				saveSetting(store, "current_provider", endpoint.Tag, providerCommand)
				return fmt.Sprintf("Đã ghim provider %s (tag: %s).", endpoint.Name, endpoint.Tag)
			}
		}
		return fmt.Sprintf("Không thấy provider %q cho model %s; dùng /providers để xem danh sách.", arg, llm.Model())
	}
}

// describeModelCapabilities tra cứu khả năng modelID qua OpenRouter và dựng 1 đoạn báo cáo ngắn
// gửi kèm reply xác nhận đổi model — để anh biết ngay model mới có tool-calling/vision không.
func describeModelCapabilities(llm *openrouter.Client, modelID string) string {
	caps, found, err := llm.ModelCapabilities(modelID)
	if err != nil {
		return "(không tra được khả năng model lúc này)"
	}
	if !found {
		return "⚠️ Không thấy model trong danh sách OpenRouter."
	}
	check := func(ok bool) string {
		if ok {
			return "✅"
		}
		return "❌"
	}
	return fmt.Sprintf("%s Nhớ lại chuyện cũ (tool-calling)\n%s Tự lưu memory (JSON mode)\n%s Xem ảnh (vision)",
		check(caps.Tools), check(caps.JSON), check(caps.Vision))
}
