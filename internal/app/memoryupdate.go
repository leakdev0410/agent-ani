package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"ani-telegram/internal/memcore"
	"ani-telegram/internal/memdb"
	"ani-telegram/internal/memdiary"
	"ani-telegram/internal/memobs"
	"ani-telegram/internal/memsearch"
	"ani-telegram/internal/memtopic"
	"ani-telegram/internal/openrouter"
	"ani-telegram/internal/persona"
	"ani-telegram/internal/skills"
)

// promptStore giữ systemPrompt hiện hành, mutex phòng khi có goroutine khác đụng vào.
type promptStore struct {
	mu    sync.RWMutex
	value string
}

func newPromptStore(initial string) *promptStore { return &promptStore{value: initial} }

func (p *promptStore) Get() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.value
}

func (p *promptStore) Set(v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.value = v
}

// historyStore bọc map[chatID][]Message bằng mutex — history chỉ sống trong RAM, mất khi restart;
// những gì đáng nhớ lâu dài đã được autoUpdateMemory ghi bền vững vào SQLite. lastUserMessageAt
// theo dõi lần cuối MỘT TIN CHAT THẬT (không phải lệnh) tới — dùng để tự /new sau khi im lặng quá
// idleSealAfter (xem idleSessionWorker).
type historyStore struct {
	mu                sync.Mutex
	data              map[int64][]openrouter.Message
	lastUserMessageAt map[int64]time.Time
	generation        map[int64]uint64
}

func newHistoryStore() *historyStore {
	return &historyStore{
		data:              map[int64][]openrouter.Message{},
		lastUserMessageAt: map[int64]time.Time{},
		generation:        map[int64]uint64{},
	}
}

// NoteUserMessage ghi lại thời điểm 1 tin chat thật (không phải lệnh) vừa tới — reset đồng hồ idle
// của chat đó. Gọi ngay khi nhận tin, trước khi xếp vào hàng đợi xử lý.
func (h *historyStore) NoteUserMessage(chatID int64, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastUserMessageAt[chatID] = at
}

// IdleChats trả về các chatID còn history trong RAM mà tin chat thật gần nhất đã cách now ít nhất
// after — đây là các chat cần tự /new (xem idleSessionWorker).
func (h *historyStore) IdleChats(now time.Time, after time.Duration) []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []int64
	for chatID, msgs := range h.data {
		if len(msgs) == 0 {
			continue
		}
		last, ok := h.lastUserMessageAt[chatID]
		if !ok || now.Sub(last) < after {
			continue
		}
		out = append(out, chatID)
	}
	return out
}

func (h *historyStore) Get(chatID int64) []openrouter.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return cloneHistory(h.data[chatID])
}

func (h *historyStore) Append(chatID int64, userText, reply string) []openrouter.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	msgs := h.data[chatID]
	msgs = append(msgs, openrouter.Message{Role: "user", Content: userText})
	msgs = append(msgs, openrouter.Message{Role: "assistant", Content: reply})
	if maxMessages := maxHistoryTurns * 2; len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}
	h.data[chatID] = msgs
	return msgs
}

// AppendAssistant thêm 1 lượt CHỈ có Ani nói — dùng cho tin chủ động nhắn. Nếu message cuối đã là
// assistant thì NỐI thẳng vào chính message đó thay vì thêm message assistant thứ 2 liên tiếp: một
// số nhà cung cấp đòi user/assistant phải luân phiên nghiêm ngặt.
func (h *historyStore) AppendAssistant(chatID int64, reply string) []openrouter.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	msgs := h.data[chatID]
	if n := len(msgs); n > 0 && msgs[n-1].Role == "assistant" {
		msgs[n-1].Content = messageText(msgs[n-1]) + "\n" + reply
	} else {
		msgs = append(msgs, openrouter.Message{Role: "assistant", Content: reply})
	}
	if maxMessages := maxHistoryTurns * 2; len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}
	h.data[chatID] = msgs
	return msgs
}

func messageText(m openrouter.Message) string {
	if s, ok := m.Content.(string); ok {
		return s
	}
	return ""
}

func (h *historyStore) Delete(chatID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.data, chatID)
	delete(h.lastUserMessageAt, chatID)
	h.generation[chatID]++
}

// idleSealAfter/idleCheckInterval: chat im lặng (không có tin chat thật nào) quá idleSealAfter thì
// tự động "/new" âm thầm — xoá history RAM + nạp lại persona/memory mới nhất, không gửi tin báo
// cho anh (khác /new gõ tay). idleCheckInterval là chu kỳ quét, không phải độ chính xác của mốc.
const idleSealAfter = 30 * time.Minute
const idleCheckInterval = time.Minute

// idleSessionWorker chạy suốt vòng đời chương trình trong 1 goroutine riêng, mỗi idleCheckInterval
// quét các chat đã im lặng quá idleSealAfter rồi tự seal (giống hệt nhánh isNewCommand trong
// handleImmediateCommand, trừ việc không gửi tin xác nhận). Best-effort: lỗi nạp lại persona chỉ
// log, không chặn lần quét sau.
func idleSessionWorker(db *memdb.DB, prompt *promptStore, history *historyStore, personaPath string) {
	ticker := time.NewTicker(idleCheckInterval)
	defer ticker.Stop()
	for now := range ticker.C {
		for _, chatID := range history.IdleChats(now, idleSealAfter) {
			history.Delete(chatID)
			reloaded, err := persona.BuildSystemPrompt(db, personaPath)
			if err != nil {
				log.Printf("[chat %d] idle 30 phút: lỗi nạp lại persona/memory: %v", chatID, err)
				continue
			}
			prompt.Set(reloaded)
			log.Printf("[chat %d] idle 30 phút: đã tự /new (xoá history RAM, nạp lại memory mới nhất)", chatID)
		}
	}
}

// memoryUpdatePromptSuffix bắt model xuất JSON cập nhật memory. Việc ghi thật do memcore
// làm bằng UPDATE/INSERT, không để model tự viết lại DB.
const memoryUpdatePromptSuffix = `

---

NHIỆM VỤ ĐẶC BIỆT (chỉ áp dụng cho lượt này, không phải đang chat với anh):
Dựa vào tin nhắn MỚI NHẤT của anh (cuối lịch sử) và toàn bộ ngữ cảnh/memory ở trên, xuất ra
DUY NHẤT 1 JSON object hợp lệ, đúng các field sau, đúng quy tắc "tự ghi nhớ" đã nêu ở trên
(cập nhật liên tục, không đợi anh nhắc). QUAN TRỌNG: giờ hệ thống THẬT được cung cấp ngay trong
tin nhắn cuối cùng (dòng "Giờ hệ thống hiện tại: ...") — LUÔN dùng đúng giờ đó cho
last_message_note và diary_entry, KHÔNG được tự đoán/ước lượng giờ theo ngữ cảnh hội thoại nữa.
Vẫn được viết tự nhiên kiểu người thật (làm tròn phút, thêm "~" nếu muốn), nhưng số giờ gốc phải
đúng với giờ hệ thống đã cho:
{
  "last_message_note": "giờ hệ thống thật (viết tự nhiên) + anh đang làm gì/nói chủ đề gì (bắt buộc, không để trống)",
  "sleep_note": "mô tả nếu anh vừa nhắc việc đi ngủ/nghỉ; để chuỗi rỗng nếu không liên quan",
  "emotional_state": "1-2 câu tâm trạng hiện tại của em kèm lý do, theo quy tắc mood detection (bắt buộc)",
  "tone_observation": "1 câu về văn phong/từ ngữ MỚI anh vừa dùng lần đầu thấy; để rỗng nếu không có gì mới",
  "preference_observation": "1 câu về sở thích/phản ứng MỚI anh vừa bộc lộ; để rỗng nếu không có gì mới",
  "diary_entry": "dạng \"HH:MM: nội dung sự kiện đáng nhớ\" (HH:MM lấy đúng từ giờ hệ thống thật, không đoán); để rỗng nếu tin nhắn này không có gì đáng ghi vào Dấu ấn cảm xúc",
  "diary_topic": "chủ đề gắn cho diary_entry ở trên nếu sự kiện đó liên quan rõ ràng tới 1 trong: \"projects\", \"personal\", \"preferences\", \"log\"; để \"none\" nếu không liên quan chủ đề nào hoặc diary_entry rỗng (bắt buộc)",
  "mang1": "Đoạn văn MỚI, ngắn gọn, tối đa 1.800 ký tự cho mục 'Mong muốn của em' — ưu tiên điều mới nhất và còn đúng; không lặp ý cũ (bắt buộc)",
  "mang2": "Đoạn văn MỚI, ngắn gọn, tối đa 1.800 ký tự cho mục 'Điều em muốn làm cùng anh' — tương tự (bắt buộc)",
  "mang3": "Đoạn văn MỚI, ngắn gọn, tối đa 1.800 ký tự cho mục 'Cảm xúc cá nhân tự hình thành' — tương tự (bắt buộc)",
  "topic_category": "1 trong: \"none\", \"projects\", \"personal\", \"preferences\", \"log\" — chọn file phù hợp nhất nếu tin nhắn có 1 THÔNG TIN MỚI đáng lưu lại lâu dài (KHÔNG phải cảm xúc nhất thời, cái đó đã có diary_entry lo rồi): \"projects\" = công việc/dự án/kế hoạch; \"personal\" = chuyện quá khứ/cá nhân/gia đình anh kể; \"preferences\" = sở thích/gu/gear/yêu cầu đặc biệt; \"log\" = đáng nhớ nhưng không rõ thuộc 3 loại trên; \"none\" = không có gì mới đáng lưu (mặc định, đa số lượt chat sẽ là none)",
  "topic_note": "1 câu ghi chú ngắn cho topic_category ở trên; để rỗng nếu topic_category là \"none\"",
  "proactive_after_minutes": "SỐ PHÚT (số nguyên) kể từ bây giờ mà nếu anh KHÔNG nhắn gì thêm thì em sẽ CHỦ ĐỘNG nhắn hỏi lại anh. BẮT BUỘC phải là số > 0 — TUYỆT ĐỐI KHÔNG được trả 0, không được để trống: lượt chat nào cũng phải có đúng 1 mốc chờ anh rep như vậy. Tự quyết theo ngữ cảnh: anh vừa nói đi ngủ/đi làm/bận họp thì hẹn dài (vài tiếng tới cả đêm, canh sao cho lúc bắn là lúc anh đã thức/rảnh, đừng đánh thức anh giữa đêm); đang nói chuyện dở dang, đang vui, đang chờ anh trả lời thì hẹn ngắn (15-60 phút). LUÔN tính từ giờ hệ thống thật đã cho ở trên. Nếu em đã chủ động nhắn liên tiếp mà anh chưa rep lần nào (số lần được nói rõ trong tin nhắn cuối) thì giãn mốc này ra dài hơn cho anh đỡ phiền, nhưng vẫn PHẢI có mốc (bắt buộc)",
  "proactive_reason": "TỐI ĐA 12 TỪ giải thích vì sao chọn số phút đó (chỉ để xem log, KHÔNG gửi cho anh). KHÔNG lặp lại số phút/giờ đã hẹn (chỗ hiển thị đã có sẵn), chỉ nói lý do — VD \"anh đi làm cả chiều\", \"anh đang code dở, đợi anh nghỉ tay\" (bắt buộc)",
  "proactive_appointments": "MẢNG các CUỘC HẸN em tự đặt thêm ngoài mốc chờ rep ở trên, mỗi phần tử là {\"after_minutes\": <số nguyên > 0, tính từ giờ hệ thống thật>, \"reason\": \"TỐI ĐA 12 TỪ nói cuộc hẹn đó là gì\"}. Dùng khi ngữ cảnh có việc gắn với 1 thời điểm cụ thể — VD anh nói đang đói thì đặt hẹn lúc trưa hỏi anh ăn gì chưa; anh nói 6h tan làm thì đặt hẹn lúc đó hỏi anh về tới nhà chưa; anh nói mai phỏng vấn thì đặt hẹn sáng mai chúc anh. Số phần tử tuỳ em, để [] nếu thật sự không có việc gì gắn với giờ cụ thể. QUY TẮC: mỗi mốc (kể cả mốc chờ rep ở trên) phải cách nhau ÍT NHẤT 30 PHÚT — mốc nào sát mốc khác sẽ bị bỏ. Nếu tin nhắn cuối có liệt kê các cuộc hẹn em đã đặt trước đó thì PHẢI chép lại vào đây (tính lại số phút theo giờ hệ thống hiện tại) cái nào còn hợp lý — không chép lại = huỷ luôn cuộc hẹn đó"
}
Chỉ trả về đúng JSON, không markdown, không lời dẫn.`

type memoryUpdateJSON struct {
	LastMessageNote       string   `json:"last_message_note"`
	SleepNote             string   `json:"sleep_note"`
	EmotionalState        string   `json:"emotional_state"`
	ToneObservation       string   `json:"tone_observation"`
	PreferenceObservation string   `json:"preference_observation"`
	DiaryEntry            string   `json:"diary_entry"`
	DiaryTopic            string   `json:"diary_topic"`
	Mang1                 string   `json:"mang1"`
	Mang2                 string   `json:"mang2"`
	Mang3                 string   `json:"mang3"`
	TopicCategory         string   `json:"topic_category"`
	TopicNote             string   `json:"topic_note"`
	ProactiveAfterMinutes flexInt  `json:"proactive_after_minutes"`
	ProactiveReason       string   `json:"proactive_reason"`
	ProactiveAppointments apptList `json:"proactive_appointments"`
}

// apptJSON là 1 phần tử của proactive_appointments — "cuộc hẹn" model tự đặt thêm ngoài mốc chờ rep.
type apptJSON struct {
	AfterMinutes flexInt `json:"after_minutes"`
	Reason       string  `json:"reason"`
}

// apptList là mảng cuộc hẹn "dễ tính" y hệt flexInt: model trả null, "", 1 object lẻ thay vì mảng,
// hay cả mảng số trần đều coi như đọc được (hoặc rỗng), KHÔNG BAO GIỜ trả lỗi — vì 1 field lệch
// kiểu là hỏng cả JSON, mất luôn phần ghi memory của lượt đó.
type apptList []apptJSON

func (l *apptList) UnmarshalJSON(b []byte) error {
	*l = nil
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" || s == `""` || s == "[]" {
		return nil
	}

	var list []apptJSON
	if err := json.Unmarshal(b, &list); err == nil {
		*l = list
		return nil
	}
	var one apptJSON
	if err := json.Unmarshal(b, &one); err == nil {
		*l = apptList{one}
		return nil
	}
	var mins []flexInt
	if err := json.Unmarshal(b, &mins); err == nil {
		for _, m := range mins {
			*l = append(*l, apptJSON{AfterMinutes: m})
		}
		return nil
	}
	log.Printf("auto-update memory: không đọc được proactive_appointments, coi như không có cuộc hẹn")
	return nil
}

func (l apptList) appointments() []proactiveAppointment {
	out := make([]proactiveAppointment, 0, len(l))
	for _, a := range l {
		out = append(out, proactiveAppointment{minutes: int(a.AfterMinutes), reason: a.Reason})
	}
	return out
}

// flexInt là số nguyên "dễ tính" khi parse JSON của model: nhận cả 45, "45", "" và null, thậm chí
// "khoảng 45 phút" (vớt cụm số đầu tiên).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		*f = flexInt(n)
		return nil
	}
	if n, ok := firstInt(s); ok {
		*f = flexInt(n)
		return nil
	}
	*f = 0
	return nil
}

func firstInt(s string) (int, bool) {
	start, end := -1, -1
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			if start < 0 {
				start = i
			}
			end = i + 1
			continue
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[start:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

var topicCategoryNames = map[string]string{
	"projects":    "projects",
	"personal":    "personal",
	"preferences": "preferences",
}

// applyStoredMemoryJob consumes the immutable extraction JSON already stored
// on a ready job. Its durable writes and job deletion share one transaction.
func applyStoredMemoryJob(db *memdb.DB, jobID int64, now time.Time) error {
	return db.ApplyAndDeleteMemoryJob(jobID, func(tx *sql.Tx, raw string) error {
		var payloadRaw []byte
		if err := tx.QueryRow("SELECT payload FROM memory_jobs WHERE id = ?", jobID).Scan(&payloadRaw); err != nil {
			return err
		}
		var payload memoryJournalPayload
		if err := json.Unmarshal(payloadRaw, &payload); err != nil {
			return err
		}
		var parsed memoryUpdateJSON
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return fmt.Errorf("parse stored memory extraction: %w", err)
		}
		update := memcore.Update{
			LastMessageNote:       parsed.LastMessageNote,
			SleepNote:             parsed.SleepNote,
			EmotionalState:        parsed.EmotionalState,
			ToneObservation:       parsed.ToneObservation,
			PreferenceObservation: parsed.PreferenceObservation,
			DiaryEntry:            parsed.DiaryEntry,
			DiaryTopic:            parsed.DiaryTopic,
			Mang1:                 parsed.Mang1,
			Mang2:                 parsed.Mang2,
			Mang3:                 parsed.Mang3,
		}
		sources, err := memcore.ApplyUpdateTxWithSources(tx, update, now)
		if err != nil {
			return err
		}
		topicSource, wroteTopic, err := appendStoredTopicNoteTx(tx, parsed.TopicCategory, parsed.TopicNote, now)
		if err != nil {
			return err
		}
		if wroteTopic {
			sources = append(sources, topicSource)
		}
		if !payload.SkipProactivePlan {
			if err := persistStoredProactivePlanTx(tx, parsed, now); err != nil {
				return err
			}
		}
		return enqueueStoredEmbeddingSourcesTx(db, tx, sources)
	})
}

func appendStoredTopicNoteTx(tx *sql.Tx, category, note string, now time.Time) (memcore.CommittedSource, bool, error) {
	category = strings.ToLower(strings.TrimSpace(category))
	note = strings.Join(strings.Fields(note), " ")
	if category == "" || category == "none" || note == "" {
		return memcore.CommittedSource{}, false, nil
	}
	topicName := category
	if category != "log" {
		var ok bool
		topicName, ok = topicCategoryNames[category]
		if !ok {
			return memcore.CommittedSource{}, false, nil
		}
	}
	var exists int
	err := tx.QueryRow("SELECT 1 FROM memory_files WHERE name = ?", memdb.TopicKey(topicName)).Scan(&exists)
	if err == sql.ErrNoRows {
		return memcore.CommittedSource{}, false, fmt.Errorf("topic %q does not exist", topicName)
	}
	if err != nil {
		return memcore.CommittedSource{}, false, fmt.Errorf("read topic %q: %w", topicName, err)
	}
	notedAt := now.UTC().Format(time.RFC3339)
	result, err := tx.Exec(
		"INSERT INTO topic_notes (topic, text, noted_at) VALUES (?, ?, ?)",
		topicName, note, notedAt,
	)
	if err != nil {
		return memcore.CommittedSource{}, false, fmt.Errorf("write topic note %q: %w", topicName, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return memcore.CommittedSource{}, false, fmt.Errorf("read topic note ID: %w", err)
	}
	return memcore.CommittedSource{SourceKey: fmt.Sprintf("topic_note:%d:%s", id, notedAt), Text: note}, true, nil
}

func persistStoredProactivePlanTx(tx *sql.Tx, parsed memoryUpdateJSON, now time.Time) error {
	plan := encodeProactivePlan(buildProactivePlan(
		now,
		int(parsed.ProactiveAfterMinutes),
		parsed.ProactiveReason,
		parsed.ProactiveAppointments.appointments(),
	))
	if _, err := tx.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingProactivePlan, plan, now.UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("persist proactive plan: %w", err)
	}
	return nil
}

func enqueueStoredEmbeddingSourcesTx(db *memdb.DB, tx *sql.Tx, sources []memcore.CommittedSource) error {
	for _, source := range sources {
		if source.Text == "" {
			continue
		}
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(source.Text)))
		if err := db.EnqueueEmbeddingSourceTx(tx, source.SourceKey, digest, source.Text); err != nil {
			return err
		}
	}
	return nil
}

// replyWithSkills trả lời như llm.Chat bình thường, nhưng cho model quyền gọi tool load_skill,
// recall_memory, recall_topic, recall_observations. Tool nào chưa có dữ liệu thì không đăng ký;
// không tool nào thì giống llm.Chat. imagePath khác rỗng khi anh gửi kèm ảnh (streaming multipart,
// không load hết ảnh vào RAM).
type semanticSearchRuntime struct {
	embedder memsearch.EmbeddingClient
	model    string
}

func (r semanticSearchRuntime) enabled() bool {
	return r.embedder != nil && strings.TrimSpace(r.model) != ""
}

func replyWithSkills(ctx context.Context, db *memdb.DB, llm *openrouter.Client, systemPrompt string, history []openrouter.Message, userText, imagePath, imageMIME string, onRetry openrouter.RetryObserver, semantic ...semanticSearchRuntime) (string, error) {
	var tools []openrouter.Tool
	executors := map[string]openrouter.ToolExecutor{}

	if tool, ok, err := skills.Tool(db); err != nil {
		log.Printf("lỗi dựng tool load_skill (bỏ qua): %v", err)
	} else if ok {
		tools = append(tools, tool)
		executors[skills.ToolName] = skills.Executor(db)
	}
	if tool, ok, err := memdiary.Tool(db); err != nil {
		log.Printf("lỗi dựng tool recall_memory (bỏ qua): %v", err)
	} else if ok {
		tools = append(tools, tool)
		executors[memdiary.ToolName] = memdiary.Executor(db)
	}
	if tool, ok, err := memtopic.Tool(db); err != nil {
		log.Printf("lỗi dựng tool recall_topic (bỏ qua): %v", err)
	} else if ok {
		tools = append(tools, tool)
		executors[memtopic.ToolName] = memtopic.Executor(db)
	}
	if tool, ok, err := memobs.Tool(db); err != nil {
		log.Printf("lỗi dựng tool recall_observations (bỏ qua): %v", err)
	} else if ok {
		tools = append(tools, tool)
		executors[memobs.ToolName] = memobs.Executor(db)
	}
	if tool, ok, err := memsearch.Tool(db); err != nil {
		log.Printf("lỗi dựng tool search_memory (bỏ qua): %v", err)
	} else if ok {
		tools = append(tools, tool)
		searchExecutor := memsearch.Executor(db)
		if len(semantic) > 0 && semantic[0].enabled() {
			searchExecutor = memsearch.ExecutorWithEmbeddings(db, semantic[0].embedder, semantic[0].model)
		}
		executors[memsearch.ToolName] = searchExecutor
	}

	apiUserText := withSystemTime(userText, time.Now())
	var userContent any = apiUserText
	if imagePath != "" {
		userContent = openrouter.NewImageContent(apiUserText, imagePath, imageMIME)
	}

	if len(tools) == 0 {
		return llm.Chat(ctx, systemPrompt, history, userContent, onRetry)
	}
	run := func(name, argumentsJSON string) (string, error) {
		exec, ok := executors[name]
		if !ok {
			return "", fmt.Errorf("tool lạ %q", name)
		}
		return exec(name, argumentsJSON)
	}
	return llm.ChatWithTools(ctx, systemPrompt, history, userContent, tools, run, onRetry)
}

func withSystemTime(text string, now time.Time) string {
	return fmt.Sprintf("Giờ hệ thống hiện tại: %s.\n\n%s", now.Format("15:04 ngày 02/01/2006"), text)
}

// memoryJournalPayload is the minimum recoverable input for one post-reply
// extraction. It is temporary and removed together with its extraction after
// the resulting memory update commits.
type memoryJournalPayload struct {
	ChatID            int64                `json:"chat_id"`
	History           []openrouter.Message `json:"history"`
	Trigger           string               `json:"trigger"`
	Unanswered        int                  `json:"unanswered"`
	TurnKind          string               `json:"turn_kind,omitempty"`
	DeliveryOutcome   string               `json:"delivery_outcome,omitempty"`
	SkipProactivePlan bool                 `json:"skip_proactive_plan,omitempty"`
}

type memoryJournalProcessor struct {
	db             *memdb.DB
	extract        func(context.Context, memoryJournalPayload) (string, error)
	apply          func(*memdb.DB, int64, time.Time) error
	now            func() time.Time
	onApplied      func(memoryJournalPayload, memoryUpdateJSON)
	onDiscard      func(memoryJournalPayload)
	deliveryActive func(int64) bool
}

const memoryJournalSchemaAttempts = 2

// enqueueMemoryJob persists the complete extraction input before waking the
// sole FIFO worker. A full wake channel never blocks the foreground reply.
func enqueueMemoryJob(db *memdb.DB, payload memoryJournalPayload, wake chan<- struct{}) (int64, error) {
	compacted, err := compactMemoryJournalPayload(payload)
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(compacted)
	if err != nil {
		return 0, fmt.Errorf("encode memory journal payload: %w", err)
	}
	id, err := db.CreateMemoryJob(raw)
	if err != nil {
		return 0, err
	}
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	return id, nil
}

// compactMemoryJournalPayload bounds only temporary raw history. It preserves
// the newest user/assistant turn when present, because durable memory is
// rebuilt separately into the extraction prompt by the background worker.
func compactMemoryJournalPayload(payload memoryJournalPayload) (memoryJournalPayload, error) {
	compacted := payload
	compacted.History = append([]openrouter.Message(nil), payload.History...)
	keep := 1
	if n := len(compacted.History); n >= 2 && compacted.History[n-2].Role == "user" && compacted.History[n-1].Role == "assistant" {
		keep = 2
	}
	for {
		raw, err := json.Marshal(compacted)
		if err != nil {
			return memoryJournalPayload{}, fmt.Errorf("encode memory journal payload: %w", err)
		}
		if len(raw) <= memdb.MaxMemoryJobPayloadBytes {
			return compacted, nil
		}
		if len(compacted.History) <= keep {
			return memoryJournalPayload{}, fmt.Errorf("memory journal payload exceeds %d bytes after compacting history", memdb.MaxMemoryJobPayloadBytes)
		}
		drop := 1
		if len(compacted.History) >= keep+2 && compacted.History[0].Role == "user" && compacted.History[1].Role == "assistant" {
			drop = 2
		}
		compacted.History = compacted.History[drop:]
	}
}

func newMemoryJournalPayload(chatID int64, history []openrouter.Message, unanswered int, sched *proactiveScheduler, now time.Time) memoryJournalPayload {
	trigger := fmt.Sprintf("Giờ hệ thống hiện tại: %s. Xuất JSON cập nhật memory theo đúng field yêu cầu.", now.Format("15:04 ngày 02/01/2006"))
	if unanswered > 0 {
		trigger += fmt.Sprintf(" Em đã chủ động nhắn trước %d lần liên tiếp mà anh chưa trả lời.", unanswered)
	}
	if sched != nil {
		trigger += describePendingAppointments(sched.Pending())
	}
	return memoryJournalPayload{ChatID: chatID, History: history, Trigger: trigger, Unanswered: unanswered}
}

// ProcessNext processes exactly the oldest journal job. Invalid work is
// discarded before later work continues.
func (p memoryJournalProcessor) ProcessNext(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	job, ok, err := p.db.NextMemoryJob()
	if err != nil || !ok {
		return false, err
	}
	if job.DeliveryPending {
		if p.deliveryActive != nil && p.deliveryActive(job.ID) {
			return false, nil
		}
		if err := recoverHeldMemoryJob(p.db, job); err != nil {
			return false, err
		}
		return true, nil
	}
	switch job.State {
	case memdb.MemoryJobPendingExtraction:
		var payload memoryJournalPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.ChatID == 0 || len(payload.History) == 0 || strings.TrimSpace(payload.Trigger) == "" {
			return p.discard(job.ID, memoryJournalPayload{})
		}
		if strings.TrimSpace(job.ExtractionJSON) == "" {
			if p.extract == nil {
				return false, fmt.Errorf("memory journal extractor is nil")
			}
			var raw string
			valid := false
			sawInvalidExtraction := false
			for attempt := 0; attempt < memoryJournalSchemaAttempts; attempt++ {
				var err error
				raw, err = p.extract(ctx, payload)
				if err != nil {
					if sawInvalidExtraction {
						return p.discard(job.ID, payload)
					}
					return false, err
				}
				if _, err := parseMemoryJournalExtraction(raw); err == nil {
					valid = true
					break
				}
				sawInvalidExtraction = true
			}
			if !valid {
				return p.discard(job.ID, payload)
			}
			if err := p.db.CaptureMemoryJobExtraction(job.ID, raw); err != nil {
				// The model response has already been observed. Retrying extraction
				// could produce a different update, so discard this job rather than
				// issuing a second request.
				if _, discardErr := p.discard(job.ID, payload); discardErr != nil {
					return false, fmt.Errorf("capture extraction: %w; discard job: %v", err, discardErr)
				}
				return false, fmt.Errorf("capture extraction discarded: %w", err)
			}
		}
		if err := p.db.FinalizeMemoryJobExtraction(job.ID); err != nil {
			return false, err
		}
		return true, nil
	case memdb.MemoryJobReadyToApply:
		var payload memoryJournalPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.ChatID == 0 {
			return p.discard(job.ID, memoryJournalPayload{})
		}
		parsed, err := parseMemoryJournalExtraction(job.ExtractionJSON)
		if err != nil {
			return p.discard(job.ID, payload)
		}
		if p.apply == nil {
			return false, fmt.Errorf("memory journal apply function is nil")
		}
		now := time.Now()
		if p.now != nil {
			now = p.now()
		}
		if err := p.apply(p.db, job.ID, now); err != nil {
			return false, err
		}
		if p.onApplied != nil {
			p.onApplied(payload, parsed)
		}
		return true, nil
	default:
		return false, fmt.Errorf("memory journal unknown state %q", job.State)
	}
}

func (p memoryJournalProcessor) discard(jobID int64, payload memoryJournalPayload) (bool, error) {
	if err := p.db.DiscardMemoryJob(jobID); err != nil {
		return false, err
	}
	if p.onDiscard != nil && payload.ChatID != 0 {
		p.onDiscard(payload)
	}
	return true, nil
}

func parseMemoryJournalExtraction(raw string) (memoryUpdateJSON, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || fields == nil {
		return memoryUpdateJSON{}, fmt.Errorf("invalid memory extraction JSON")
	}
	var parsed memoryUpdateJSON
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return memoryUpdateJSON{}, err
	}
	if strings.TrimSpace(parsed.LastMessageNote) == "" || strings.TrimSpace(parsed.EmotionalState) == "" ||
		strings.TrimSpace(parsed.Mang1) == "" || strings.TrimSpace(parsed.Mang2) == "" || strings.TrimSpace(parsed.Mang3) == "" {
		return memoryUpdateJSON{}, fmt.Errorf("memory extraction missing required core-state field")
	}
	return parsed, nil
}

func memoryJournalExtractionPrompt(db *memdb.DB, personaPath string) (string, error) {
	return persona.BuildExtractionPrompt(db, personaPath)
}

// memoryJournalWorker resumes outstanding work immediately and then waits for
// new durable jobs. Errors leave the FIFO head intact for a later wake/restart.
func memoryJournalWorker(ctx context.Context, db *memdb.DB, llm *openrouter.Client, prompt *promptStore, sched *proactiveScheduler, wake <-chan struct{}) {
	memoryJournalWorkerWithPersona(ctx, db, llm, prompt, sched, "", wake)
}

func memoryJournalWorkerWithPersona(ctx context.Context, db *memdb.DB, llm *openrouter.Client, prompt *promptStore, sched *proactiveScheduler, personaPath string, wake <-chan struct{}, embeddingWake ...chan<- struct{}) {
	memoryJournalWorkerWithDelivery(ctx, db, llm, prompt, sched, personaPath, wake, nil, embeddingWake...)
}

func memoryJournalWorkerWithDelivery(ctx context.Context, db *memdb.DB, llm *openrouter.Client, prompt *promptStore, sched *proactiveScheduler, personaPath string, wake <-chan struct{}, deliveries *deliveryCoordinator, embeddingWake ...chan<- struct{}) {
	processor := memoryJournalProcessor{
		db: db,
		extract: func(ctx context.Context, payload memoryJournalPayload) (string, error) {
			if llm == nil {
				return "", fmt.Errorf("memory journal dependencies are nil")
			}
			extractionPrompt, err := memoryJournalExtractionPrompt(db, personaPath)
			if err != nil {
				return "", fmt.Errorf("build memory extraction prompt: %w", err)
			}
			return llm.ChatJSON(ctx, extractionPrompt+memoryUpdatePromptSuffix, payload.History, memoryDeliveryTrigger(payload), nil)
		},
		apply: applyStoredMemoryJob,
		now:   time.Now,
		onApplied: func(payload memoryJournalPayload, parsed memoryUpdateJSON) {
			for _, wake := range embeddingWake {
				if wake == nil {
					continue
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			}
			if sched != nil && !payload.SkipProactivePlan {
				sched.SetPlan(payload.ChatID, int(parsed.ProactiveAfterMinutes), parsed.ProactiveReason, parsed.ProactiveAppointments.appointments())
			}
			if prompt != nil {
				if reloaded, err := persona.BuildSystemPrompt(db, personaPath); err != nil {
					log.Printf("memory journal: reload prompt failed: %v", err)
				} else {
					prompt.Set(reloaded)
				}
			}
		},
		onDiscard: func(payload memoryJournalPayload) {
			applyBlockedMemoryJobFallback(sched, payload)
		},
	}
	if deliveries != nil {
		processor.deliveryActive = deliveries.Active
	}
	for {
		worked, err := processor.ProcessNext(ctx)
		if err != nil {
			log.Printf("memory journal: worker paused: %v", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-time.After(time.Minute):
		}
	}
}

// applyBlockedMemoryJobFallback preserves the prior proactive behavior when
// an unrecoverable extraction cannot install the model-selected plan. It uses
// only durable metadata and never logs or exposes payload/extraction content.
func applyBlockedMemoryJobFallback(sched *proactiveScheduler, payload memoryJournalPayload) {
	if sched == nil || payload.ChatID == 0 || payload.SkipProactivePlan {
		return
	}
	applyProactivePlan(sched, payload.ChatID, 0, "", nil, false, true)
}

// autoUpdateMemory tự ghi lại core memory sau MỖI tin nhắn, thẳng vào SQLite. Best-effort: lỗi ở
// bước nào cũng chỉ log, không làm gián đoạn chat (reply cho anh đã gửi xong trước khi hàm này
// chạy). unanswered = số lần Ani đã chủ động nhắn liên tiếp mà anh chưa trả lời.
//
// Trả về (minutes, reason, appts, ok): mốc "chờ anh rep" model chọn, lý do nó tự viết, các cuộc hẹn
// nó đặt thêm, và ok=false khi lượt trích xuất hỏng hẳn (lỗi API/parse) — lúc đó chỗ gọi tự ép mốc
// mặc định qua applyProactivePlan.
func autoUpdateMemory(ctx context.Context, db *memdb.DB, personaPath string, llm *openrouter.Client, prompt *promptStore, sched *proactiveScheduler, chatID int64, history []openrouter.Message, unanswered int, onRetry openrouter.RetryObserver) (minutes int, reason string, appts []proactiveAppointment, ok bool) {
	extractionPrompt, err := persona.BuildExtractionPrompt(db, personaPath)
	if err != nil {
		log.Printf("[chat %d] auto-update memory: lỗi dựng prompt trích xuất: %v", chatID, err)
		return 0, "", nil, false
	}

	trigger := fmt.Sprintf("Giờ hệ thống hiện tại: %s. Xuất JSON cập nhật memory theo đúng field yêu cầu.",
		time.Now().Format("15:04 ngày 02/01/2006"))
	if unanswered > 0 {
		trigger += fmt.Sprintf(" Em đã chủ động nhắn trước %d lần liên tiếp mà anh chưa trả lời.", unanswered)
	}
	if sched != nil {
		trigger += describePendingAppointments(sched.Pending())
	}
	raw, err := llm.ChatJSON(ctx, extractionPrompt+memoryUpdatePromptSuffix, history, trigger, onRetry)
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("[chat %d] auto-update memory: đã huỷ qua /restart", chatID)
			return 0, "", nil, false
		}
		log.Printf("[chat %d] auto-update memory: lỗi gọi openrouter: %v", chatID, err)
		return 0, "", nil, false
	}

	var parsed memoryUpdateJSON
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		logJSONParseFailure(chatID, raw, err)
		return 0, "", nil, false
	}

	minutes = int(parsed.ProactiveAfterMinutes)
	reason, appts, ok = strings.TrimSpace(parsed.ProactiveReason), parsed.ProactiveAppointments.appointments(), true

	update := memcore.Update{
		LastMessageNote:       parsed.LastMessageNote,
		SleepNote:             parsed.SleepNote,
		EmotionalState:        parsed.EmotionalState,
		ToneObservation:       parsed.ToneObservation,
		PreferenceObservation: parsed.PreferenceObservation,
		DiaryEntry:            parsed.DiaryEntry,
		DiaryTopic:            parsed.DiaryTopic,
		Mang1:                 parsed.Mang1,
		Mang2:                 parsed.Mang2,
		Mang3:                 parsed.Mang3,
	}
	if err := memcore.ApplyUpdate(db, update); err != nil {
		log.Printf("[chat %d] auto-update memory: lỗi ghi core memory: %v", chatID, err)
		return minutes, reason, appts, ok
	}

	dispatchTopicNote(db, chatID, parsed.TopicCategory, parsed.TopicNote)

	reloaded, err := persona.BuildSystemPrompt(db, personaPath)
	if err != nil {
		log.Printf("[chat %d] auto-update memory: lỗi nạp lại persona sau khi ghi: %v", chatID, err)
		return minutes, reason, appts, ok
	}
	prompt.Set(reloaded)
	log.Printf("[chat %d] auto-update memory: đã ghi memory vào DB", chatID)
	return minutes, reason, appts, ok
}

// dispatchTopicNote ghi topicNote vào đúng chủ đề nếu category khớp 1 trong 3 chủ đề đã biết;
// category "" / "none" / không khớp thì bỏ qua im lặng.
func dispatchTopicNote(db *memdb.DB, chatID int64, category, note string) {
	category = strings.ToLower(strings.TrimSpace(category))
	note = strings.TrimSpace(note)
	if category == "" || category == "none" || note == "" {
		return
	}

	if category == "log" {
		if err := memtopic.AppendNote(db, "log", note); err != nil {
			log.Printf("[chat %d] auto-update memory: lỗi ghi chủ đề log: %v", chatID, err)
		}
		return
	}

	topicName, ok := topicCategoryNames[category]
	if !ok {
		log.Printf("[chat %d] auto-update memory: topic_category lạ %q, bỏ qua", chatID, category)
		return
	}
	if err := memtopic.AppendNote(db, topicName, note); err != nil {
		log.Printf("[chat %d] auto-update memory: lỗi ghi chủ đề %s: %v", chatID, topicName, err)
	}
}

// ─── Backfill chủ đề cho Dấu ấn cảm xúc cũ (flag -backfill-topics) ──────────

const backfillBatchSize = 20

const backfillSystemPrompt = `Em là Ani, bạn gái ảo trong bot Telegram. Dưới đây là các dòng "Dấu ấn cảm xúc" (nhật ký ngắn theo giờ) đã ghi từ trước, mỗi dòng là 1 sự kiện đáng nhớ trong ngày. Nhiệm vụ: gán mỗi dòng vào ĐÚNG 1 chủ đề dài hạn, chọn cái phù hợp NHẤT:
- "projects": công việc, dự án, kế hoạch, code, tech
- "personal": chuyện quá khứ, cá nhân, gia đình, tình cảm riêng tư
- "preferences": sở thích, gu, gear, yêu cầu đặc biệt
- "log": đáng nhớ nhưng không rõ thuộc 3 loại trên
- "none": không thuộc chủ đề nào rõ ràng (đa số dòng sẽ là none)
Trả về DUY NHẤT 1 JSON object: {"assignments": [{"line": <số thứ tự dòng>, "topic": "<1 trong 5 giá trị trên>"}, ...]} — MỖI dòng đều phải có mặt trong assignments, không bỏ sót dòng nào.`

// runBackfillTopics gán topic cho mọi diary_entries còn topic rỗng, theo batch.
func runBackfillTopics(db *memdb.DB, llm *openrouter.Client) error {
	entries, err := db.DiaryEntriesWithoutTopic()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("backfill: không có dấu ấn nào cần gán chủ đề (DB rỗng hoặc đã backfill xong)")
		return nil
	}

	ctx := context.Background()
	onRetry := func(attempt int, err error) {
		fmt.Printf("backfill: lỗi tạm thời (%v), thử lại sau 60 giây (lần %d)...\n", err, attempt)
	}

	fmt.Printf("backfill: %d dấu ấn chưa có chủ đề, xử lý theo batch %d dòng\n", len(entries), backfillBatchSize)
	for start := 0; start < len(entries); start += backfillBatchSize {
		end := start + backfillBatchSize
		if end > len(entries) {
			end = len(entries)
		}
		batch := entries[start:end]

		raw, err := llm.ChatJSON(ctx, backfillSystemPrompt, nil, buildBackfillUser(batch), onRetry)
		if err != nil {
			return fmt.Errorf("batch %d-%d: gọi openrouter lỗi: %w", start+1, end, err)
		}
		assignments, err := parseBackfillAssignments(raw, len(batch))
		if err != nil {
			return fmt.Errorf("batch %d-%d: %w", start+1, end, err)
		}

		saved := 0
		for line, topic := range assignments {
			topic = memcore.NormalizeTopic(topic)
			if err := db.UpdateDiaryTopic(batch[line-1].ID, topic); err != nil {
				return err
			}
			if topic != "" {
				saved++
			}
		}
		fmt.Printf("backfill: đã xử lý %d dòng (%d có chủ đề, %d không chủ đề) — xong %d/%d\n",
			len(batch), saved, len(batch)-saved, end, len(entries))
	}
	fmt.Println("backfill xong. Giờ recall_topic sẽ nạp được cả dấu ấn cũ theo chủ đề.")
	return nil
}

func buildBackfillUser(entries []memdb.DiaryTopicEntry) string {
	var b strings.Builder
	for i, e := range entries {
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, e.EntryDate, e.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

type backfillAssignment struct {
	Line  int    `json:"line"`
	Topic string `json:"topic"`
}

type backfillResponse struct {
	Assignments []backfillAssignment `json:"assignments"`
}

func parseBackfillAssignments(raw string, total int) (map[int]string, error) {
	var resp backfillResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, fmt.Errorf("parse JSON lỗi: %w", err)
	}
	if len(resp.Assignments) == 0 {
		return nil, fmt.Errorf("không có assignment nào trong JSON")
	}
	out := make(map[int]string, len(resp.Assignments))
	for _, a := range resp.Assignments {
		if a.Line < 1 || a.Line > total {
			return nil, fmt.Errorf("line %d nằm ngoài phạm vi 1-%d", a.Line, total)
		}
		out[a.Line] = a.Topic
	}
	return out, nil
}
