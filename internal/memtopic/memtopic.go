// Package memtopic ghi ghi chú tự động vào chủ đề (projects/personal/preferences/log, lưu trong
// SQLite qua memdb) — chỉ vào những chủ đề ĐÃ TỒN TẠI SẴN (memory_files, key topic/<tên>), mỗi
// ghi chú là 1 row riêng trong bảng topic_notes (INSERT đơn giản, không còn phải dò heading rồi
// chèn text vào giữa 1 blob) — không tự tạo chủ đề mới, không đụng nội dung tĩnh cũ.
package memtopic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
)

// AppendNote thêm 1 ghi chú tự động mới vào chủ đề topicName (VD "projects", "personal",
// "preferences"). Chủ đề phải đã tồn tại sẵn trong memory_files — không tự tạo chủ đề mới,
// tránh bot tạo lung tung mục lạ ngoài ý muốn.
func AppendNote(db *memdb.DB, topicName, note string) error {
	note = strings.Join(strings.Fields(note), " ")
	if note == "" {
		return fmt.Errorf("ghi chú rỗng, không có gì để lưu")
	}

	exists, err := db.Exists(memdb.TopicKey(topicName))
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("chủ đề %q chưa tồn tại trong DB (phải tồn tại sẵn)", topicName)
	}

	notedAt := time.Now().Format("2006-01-02 15:04")
	if err := db.AddTopicNote(topicName, note, notedAt); err != nil {
		return fmt.Errorf("ghi topic_notes(%s) lỗi: %w", topicName, err)
	}
	return nil
}

const ToolName = "recall_topic"
const autoNotesHeading = "## Ghi chú tự động từ Telegram"
const diaryByTopicHeading = "## Dấu ấn cảm xúc liên quan (mọi ngày)"

// TopicDiaryBulletLimit giới hạn số bullet dấu ấn cảm xúc recall_topic nạp cho 1 chủ đề (mọi
// ngày gộp lại, ngày mới nhất trước) — phần cũ hơn lấy lại bằng recall_memory(date) theo ngày.
const TopicDiaryBulletLimit = 50

// Tool dựng recall_topic — tải đủ 1 chủ đề (blob tĩnh + topic_notes) khi câu chuyện chạm tới.
func Tool(db *memdb.DB) (openrouter.Tool, bool, error) {
	names, err := db.TopicNames()
	if err != nil {
		return openrouter.Tool{}, false, err
	}
	if len(names) == 0 {
		return openrouter.Tool{}, false, nil
	}
	return openrouter.Tool{
		Type: "function",
		Function: openrouter.ToolFunction{
			Name: ToolName,
			Description: "Nhớ lại ĐẦY ĐỦ 1 chủ đề dài hạn (projects/personal/preferences/log) khi " +
				"câu chuyện thật sự chạm tới: nội dung tĩnh + ghi chú Telegram + các dấu ấn cảm xúc " +
				"gắn chủ đề đó (mọi ngày). Đừng gọi nếu đang chat bình thường không liên quan.",
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

type topicArgs struct {
	Name string `json:"name"`
}

// Executor trả nội dung đầy đủ 1 chủ đề, gồm văn xuôi tĩnh + ghi chú Telegram mới.
func Executor(db *memdb.DB) openrouter.ToolExecutor {
	return func(name, argumentsJSON string) (string, error) {
		if name != ToolName {
			return "", fmt.Errorf("memtopic: tool lạ %q", name)
		}
		var a topicArgs
		if err := json.Unmarshal([]byte(argumentsJSON), &a); err != nil {
			return "", fmt.Errorf("memtopic: tham số không hợp lệ: %w", err)
		}
		topicName := strings.TrimSpace(a.Name)
		db.Note("🔧 Model gọi recall_topic(%s) — đang nhớ lại chủ đề", topicName)
		return RenderFull(db, topicName)
	}
}

// RenderFull dựng lại đúng 1 chủ đề: blob tĩnh + các topic_notes mới + dấu ấn cảm xúc gắn
// chủ đề (mọi ngày, tối đa TopicDiaryBulletLimit, ngày mới nhất trước). Phần dấu ấn bị cắt có
// hint gọi recall_memory(date) theo ngày để nhớ đủ.
func RenderFull(db *memdb.DB, topicName string) (string, error) {
	content, err := db.Read(memdb.TopicKey(topicName))
	if err != nil {
		return "", fmt.Errorf("memtopic: đọc %s lỗi: %w", topicName, err)
	}
	if content == "" {
		exists, err := db.Exists(memdb.TopicKey(topicName))
		if err != nil {
			return "", err
		}
		if !exists {
			return "", fmt.Errorf("memtopic: không có chủ đề %q", topicName)
		}
	}
	notes, err := db.TopicNotes(topicName)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## %s.md\n\n", topicName)
	b.WriteString(strings.TrimRight(content, "\n"))
	if len(notes) > 0 {
		if !strings.Contains(content, autoNotesHeading) {
			b.WriteString("\n\n" + autoNotesHeading)
		}
		for _, n := range notes {
			fmt.Fprintf(&b, "\n- %s (qua Telegram): %s", n.NotedAt, n.Text)
		}
	}

	diaryText, err := renderTopicDiary(db, topicName)
	if err != nil {
		return "", err
	}
	if diaryText != "" {
		b.WriteString("\n\n" + diaryText)
	}
	return b.String(), nil
}

// renderTopicDiary dựng mục dấu ấn cảm xúc theo chủ đề: nhóm theo ngày (mới nhất trước), cắt ở
// TopicDiaryBulletLimit, kèm hint recall_memory nếu còn nhiều hơn. Trả rỗng nếu chủ đề chưa có
// dấu ấn nào.
func renderTopicDiary(db *memdb.DB, topicName string) (string, error) {
	total, err := db.DiaryCountForTopic(topicName)
	if err != nil {
		return "", err
	}
	if total == 0 {
		return "", nil
	}

	entries, err := db.DiaryEntriesForTopic(topicName, TopicDiaryBulletLimit)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(diaryByTopicHeading)
	currentDate := ""
	for _, e := range entries {
		if e.EntryDate != currentDate {
			if currentDate != "" {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "\n**%s**:", e.EntryDate)
			currentDate = e.EntryDate
		}
		fmt.Fprintf(&b, "\n- %s", e.Text)
	}
	if hidden := total - len(entries); hidden > 0 {
		fmt.Fprintf(&b, "\n(còn %d dấu ấn sớm hơn của chủ đề này — gọi recall_memory(date) theo từng ngày để nhớ đủ)", hidden)
	}
	return b.String(), nil
}
