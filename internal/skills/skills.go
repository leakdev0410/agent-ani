// Package skills cung cấp tool "load_skill" cho model — cho phép Ani chủ động tải nội dung
// đầy đủ của 1 skill (ani-memory/skills/<tên>/SKILL.md, lưu trong DB qua memdb) khi câu chuyện
// thật sự chạm tới chủ đề đó, thay vì nhồi hết tất cả skill vào system prompt mỗi lượt chat —
// giống cách trí nhớ dài hạn con người chỉ "nhớ ra" chi tiết khi được gợi đúng lúc.
package skills

import (
	"encoding/json"
	"fmt"
	"strings"

	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
)

const ToolName = "load_skill"

// Tool dựng định nghĩa tool load_skill cho model, liệt kê đúng tên skill hiện có trong DB
// làm enum tham số — model chỉ được chọn trong số này. Trả về (Tool{}, false, nil) nếu chưa
// có skill nào trong DB (không seed tool để tránh model gọi vô ích).
func Tool(db *memdb.DB) (openrouter.Tool, bool, error) {
	names, err := db.SkillNames()
	if err != nil {
		return openrouter.Tool{}, false, fmt.Errorf("skills: không liệt kê được skill trong DB: %w", err)
	}
	if len(names) == 0 {
		return openrouter.Tool{}, false, nil
	}

	return openrouter.Tool{
		Type: "function",
		Function: openrouter.ToolFunction{
			Name: ToolName,
			Description: "Tải nội dung ĐẦY ĐỦ của 1 skill khi câu chuyện thật sự cần chi tiết " +
				"(VD anh hỏi sâu về 1 dự án cụ thể, hoặc về kiến trúc ani-telegram...). " +
				"Đừng gọi nếu chỉ đang chat bình thường không liên quan.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type": "string",
						"enum": names,
					},
				},
				"required": []string{"name"},
			},
		},
	}, true, nil
}

type args struct {
	Name string `json:"name"`
}

// Executor trả về hàm thực thi load_skill: parse tên skill từ JSON args, đọc
// "skills/<name>/SKILL.md" từ db, trả nội dung — hoặc lỗi rõ ràng nếu tên không hợp lệ, để
// model tự biết mà thử lại đúng tên hoặc bỏ qua.
func Executor(db *memdb.DB) openrouter.ToolExecutor {
	return func(name, argumentsJSON string) (string, error) {
		if name != ToolName {
			return "", fmt.Errorf("skills: tool lạ %q", name)
		}

		var a args
		if err := json.Unmarshal([]byte(argumentsJSON), &a); err != nil {
			return "", fmt.Errorf("skills: tham số không hợp lệ: %w", err)
		}
		skillName := strings.TrimSpace(a.Name)

		db.Note("🔧 Model gọi load_skill(%s) — đang tải chi tiết skill", skillName)
		content, err := db.Read(memdb.SkillKey(skillName))
		if err != nil {
			return "", fmt.Errorf("skills: đọc lỗi: %w", err)
		}
		if content == "" {
			return "", fmt.Errorf("skills: không có skill tên %q", skillName)
		}
		return content, nil
	}
}
