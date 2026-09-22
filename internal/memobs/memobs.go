// Package memobs cung cấp tool "recall_observations" — nhớ lại toàn bộ quan sát
// văn phong / sở thích khi prompt chỉ giữ phần nền tảng + gần đây.
package memobs

import (
	"encoding/json"
	"fmt"
	"strings"

	"ani-telegram/internal/memcore"
	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
)

const ToolName = "recall_observations"

// Tool chỉ đăng ký khi DB đã có ít nhất 1 observation.
func Tool(db *memdb.DB) (openrouter.Tool, bool, error) {
	kinds, err := availableKinds(db)
	if err != nil {
		return openrouter.Tool{}, false, err
	}
	if len(kinds) == 0 {
		return openrouter.Tool{}, false, nil
	}
	return openrouter.Tool{
		Type: "function",
		Function: openrouter.ToolFunction{
			Name: ToolName,
			Description: "Nhớ lại TOÀN BỘ quan sát văn phong (tone) hoặc sở thích (preference) " +
				"khi cần hơn phần đã hiện sẵn trong prompt. Đừng gọi nếu chat bình thường.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind": map[string]any{
						"type": "string",
						"enum": kinds,
					},
				},
				"required": []string{"kind"},
			},
		},
	}, true, nil
}

type args struct {
	Kind string `json:"kind"`
}

// Executor trả về toàn bộ observation của 1 kind, đúng định dạng bullet gốc.
func Executor(db *memdb.DB) openrouter.ToolExecutor {
	return func(name, argumentsJSON string) (string, error) {
		if name != ToolName {
			return "", fmt.Errorf("memobs: tool lạ %q", name)
		}
		var a args
		if err := json.Unmarshal([]byte(argumentsJSON), &a); err != nil {
			return "", fmt.Errorf("memobs: tham số không hợp lệ: %w", err)
		}
		kind := strings.ToLower(strings.TrimSpace(a.Kind))
		if kind != memdb.ObservationTone && kind != memdb.ObservationPreference {
			return "", fmt.Errorf("memobs: kind không hợp lệ %q", kind)
		}

		db.Note("🔧 Model gọi recall_observations(%s) — đang nhớ lại quan sát", kind)
		obs, err := db.Observations(kind)
		if err != nil {
			return "", err
		}
		if len(obs) == 0 {
			return "", fmt.Errorf("memobs: chưa có quan sát kind %q", kind)
		}
		converted := make([]memcore.Observation, len(obs))
		for i, o := range obs {
			converted[i] = memcore.Observation{Text: o.Text, Date: o.ObservedAt}
		}
		heading := "Văn phong anh quan sát được"
		if kind == memdb.ObservationPreference {
			heading = "Sở thích / phản ứng anh đã quan sát được"
		}
		return fmt.Sprintf("### %s\n%s", heading, memcore.RenderObservations(converted)), nil
	}
}

func availableKinds(db *memdb.DB) ([]string, error) {
	var kinds []string
	for _, kind := range []string{memdb.ObservationTone, memdb.ObservationPreference} {
		obs, err := db.Observations(kind)
		if err != nil {
			return nil, err
		}
		if len(obs) > 0 {
			kinds = append(kinds, kind)
		}
	}
	return kinds, nil
}
