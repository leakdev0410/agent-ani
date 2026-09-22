// Package memdiary cung cấp tool "recall_memory" cho model — cho phép Ani chủ động "nhớ
// lại" chi tiết đầy đủ 1 ngày cũ trong "Dấu ấn cảm xúc" (bảng diary_entries) khi câu chuyện
// thật sự cần, thay vì lúc nào cũng nạp toàn bộ nhật ký vào system prompt (xem
// memcore.RecentDiaryDays) — giống cách trí nhớ dài hạn con người chỉ "nhớ ra" chi tiết cũ khi
// được gợi đúng lúc. Không có gì bị xoá hay tóm tắt — chỉ SELECT lại đúng dữ liệu đã có.
package memdiary

import (
	"encoding/json"
	"fmt"
	"strings"

	"ani-telegram/internal/memcore"
	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
)

const ToolName = "recall_memory"

// Tool dựng recall_memory: mọi ngày không hiện đủ inline (cũ hơn cửa sổ gần,
// hoặc ngày gần bị cắt vì quá nhiều bullet).
func Tool(db *memdb.DB) (openrouter.Tool, bool, error) {
	dates, err := recallDates(db)
	if err != nil {
		return openrouter.Tool{}, false, err
	}
	if len(dates) == 0 {
		return openrouter.Tool{}, false, nil
	}

	return openrouter.Tool{
		Type: "function",
		Function: openrouter.ToolFunction{
			Name: ToolName,
			Description: "Nhớ lại chi tiết ĐẦY ĐỦ 1 ngày trong 'Dấu ấn cảm xúc' (ngày cũ, hoặc ngày " +
				"gần nếu prompt chỉ hiện một phần). Đừng gọi nếu đang chat bình thường.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"date": map[string]any{
						"type": "string",
						"enum": dates,
					},
				},
				"required": []string{"date"},
			},
		},
	}, true, nil
}

type args struct {
	Date string `json:"date"`
}

// Executor trả về hàm thực thi recall_memory: SELECT diary_entries của đúng ngày được yêu cầu,
// trả nguyên văn nội dung ngày đó.
func Executor(db *memdb.DB) openrouter.ToolExecutor {
	return func(name, argumentsJSON string) (string, error) {
		if name != ToolName {
			return "", fmt.Errorf("memdiary: tool lạ %q", name)
		}

		var a args
		if err := json.Unmarshal([]byte(argumentsJSON), &a); err != nil {
			return "", fmt.Errorf("memdiary: tham số không hợp lệ: %w", err)
		}
		date := strings.TrimSpace(a.Date)

		db.Note("🔧 Model gọi recall_memory(%s) — đang nhớ lại ngày cũ", date)
		texts, err := db.DiaryEntriesForDate(date)
		if err != nil {
			return "", fmt.Errorf("memdiary: đọc diary_entries(%s) lỗi: %w", date, err)
		}
		if len(texts) == 0 {
			return "", fmt.Errorf("memdiary: không có ngày %q trong Dấu ấn cảm xúc", date)
		}

		return fmt.Sprintf("**%s**:\n%s", date, memcore.RenderDiaryBullets(texts)), nil
	}
}

// recallDates: ngày ngoài cửa sổ gần, cộng ngày gần bị cắt vì vượt RecentDiaryBulletsPerDay.
func recallDates(db *memdb.DB) ([]string, error) {
	dates, err := db.DiaryDates()
	if err != nil {
		return nil, fmt.Errorf("memdiary: liệt kê diary_dates lỗi: %w", err)
	}

	var out []string
	for i, date := range dates {
		if i >= memcore.RecentDiaryDays {
			out = append(out, date)
			continue
		}
		texts, err := db.DiaryEntriesForDate(date)
		if err != nil {
			return nil, err
		}
		if len(texts) > memcore.RecentDiaryBulletsPerDay {
			out = append(out, date)
		}
	}
	return out, nil
}
