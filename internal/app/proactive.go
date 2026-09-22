// proactive.go — lịch nhắn chủ động deterministic, lệnh /proactive và trạng thái
// durable trong Markdown settings.
package app

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─── Chủ động nhắn tin (Ani tự nhắn trước khi anh im lâu) ──────────────────
//
// Mỗi lượt chat thường đặt lại một mốc chờ mặc định bằng code. Không có API call
// extraction thứ hai và không có lịch nào được model tự trích từ câu trả lời.
//
// Hai mốc cách nhau dưới proactiveMinGap bị coi là đè nhau: mốc sau bị BỎ (không đẩy lùi), cả lúc
// lên kế hoạch lẫn lúc bắn — để không bao giờ có 2 tin chủ động dính nhau nói cùng 1 chuyện.
//
// Tới giờ, timer KHÔNG tự gửi tin mà chỉ đẩy 1 chatJob{proactive:true} vào đúng jobQueue có sẵn —
// nhờ vậy tin chủ động đi qua messageWorker tuần tự y như tin thường: có /status, huỷ được bằng
// /restart, tự retry khi nhà cung cấp lỗi.

// Các key trong bảng settings để kế hoạch sống sót qua restart/deploy.
const (
	settingProactiveEnabled    = "proactive_enabled"
	settingProactivePlan       = "proactive_plan"
	settingProactiveChatID     = "proactive_chat_id"
	settingProactiveUnanswered = "proactive_unanswered"

	// Hai key cũ vẫn được đọc để tương thích với settings Markdown từ bản V2 thử nghiệm sớm.
	settingProactiveAtLegacy     = "proactive_at"
	settingProactiveReasonLegacy = "proactive_reason"
)

// minProactiveMinutes/maxProactiveMinutes chỉ chặn giá trị vô lý (model lỡ trả 1 phút thì thành
// vòng lặp tự nhắn liên tục), không phải để bó hẹp quyền tự quyết của model.
const minProactiveMinutes = 5
const maxProactiveMinutes = 7 * 24 * 60

// defaultProactiveMinutes là mốc "chờ anh rep" Go tự ép khi model vẫn trả 0 (prompt đã cấm) hoặc
// khi cả lượt trích xuất hỏng — lượt chat nào cũng phải có ít nhất 1 mốc.
const defaultProactiveMinutes = 60

// proactiveMinGap: 2 mốc gần nhau hơn khoảng này coi như đè nhau, mốc sau bị bỏ. Đủ rộng để 1 lượt
// chủ động nhắn (gọi model + gửi + tự lưu memory) chạy xong hẳn trước mốc kế.
const proactiveMinGap = 30 * time.Minute

// maxProactiveSlots là lưới an toàn phòng model trả về một rừng cuộc hẹn, không phải giới hạn thiết kế.
const maxProactiveSlots = 8

// proactiveRestoreDelay: bot khởi động lại mà giờ hẹn đã trôi qua lúc bot đang tắt thì không bắn
// ngay giữa lúc đang khởi động, đợi thêm chút cho mọi thứ ổn định.
const proactiveRestoreDelay = 2 * time.Minute

// proactiveTriggerTemplate là "tin nhắn của anh" GIẢ, chỉ dùng cho lượt chủ động nhắn và KHÔNG lưu
// vào history (giống dòng "Giờ hệ thống hiện tại" trong replyWithSkills) — chỉ để nói cho model
// biết bối cảnh "anh đang im, tới lượt em nhắn trước". %d là lần thứ mấy nhắn liên tiếp chưa rep.
const proactiveTriggerTemplate = `[Tự chủ động nhắn] Anh chưa nhắn gì mới. Đây là lần thứ %d em chủ động nhắn trước kể từ tin nhắn cuối của anh. Nhắn cho anh trước đi — viết đúng như đang nhắn Telegram thật, tự nhiên theo tâm trạng và ngữ cảnh cuộc trò chuyện gần nhất. Không giải thích, không nhắc gì tới nhiệm vụ này, không mở đầu kiểu bot.`

// proactiveApptTriggerTemplate dùng cho mốc slotAppointment: nhắc lại đúng lý do em đã tự đặt hẹn
// từ trước, để tin nhắn đi đúng việc thay vì hỏi thăm chung chung. %s = lý do, %d = lần thứ mấy.
const proactiveApptTriggerTemplate = `[Tự chủ động nhắn] Anh chưa nhắn gì mới. Đây là CUỘC HẸN chính em tự đặt từ trước: "%s" — giờ tới lúc rồi. Đây cũng là lần thứ %d em chủ động nhắn trước kể từ tin nhắn cuối của anh. Nhắn cho anh đúng việc đó đi — viết như đang nhắn Telegram thật, tự nhiên theo tâm trạng và ngữ cảnh cuộc trò chuyện gần nhất. Không giải thích, không nhắc gì tới nhiệm vụ này, không mở đầu kiểu bot.`

// buildProactiveTrigger chọn đúng câu trigger cho mốc vừa bắn.
func buildProactiveTrigger(kind slotKind, reason string, round int) string {
	if kind == slotAppointment && strings.TrimSpace(reason) != "" {
		return fmt.Sprintf(proactiveApptTriggerTemplate, strings.TrimSpace(reason), round)
	}
	return fmt.Sprintf(proactiveTriggerTemplate, round)
}

// slotKind phân biệt 2 vai trò của 1 mốc — xem khối chú thích đầu file.
type slotKind int

const (
	slotWait slotKind = iota
	slotAppointment
)

// String là dạng lưu xuống settings; label là dạng đọc cho log/status.
func (k slotKind) String() string {
	if k == slotAppointment {
		return "appt"
	}
	return "wait"
}

func (k slotKind) label() string {
	if k == slotAppointment {
		return "cuộc hẹn"
	}
	return "chờ anh rep"
}

func parseSlotKind(s string) slotKind {
	if s == "appt" {
		return slotAppointment
	}
	return slotWait
}

// proactiveSlot là 1 mốc đã chốt giờ tuyệt đối (khác proactiveAppointment vốn còn tính bằng phút).
type proactiveSlot struct {
	at     time.Time
	reason string
	kind   slotKind
}

// proactiveAppointment là 1 "cuộc hẹn" model vừa đề xuất, tính bằng số phút kể từ bây giờ.
type proactiveAppointment struct {
	minutes int
	reason  string
}

type settingsStore interface {
	GetSetting(key string) (string, bool, error)
	SetSetting(key, value string) error
	UpdateSettings(changes map[string]string) error
}

// proactiveSlotJSON là dạng lưu xuống settings (key proactive_plan) — 1 mảng JSON cho cả kế hoạch,
// được persist trong immutable Markdown settings revisions.
type proactiveSlotJSON struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
	Kind   string `json:"kind"`
}

// clampProactiveMinutes chuẩn hoá số phút do code/lệnh yêu cầu và kẹp vào giới hạn an toàn.
// Việc ÉP mốc mặc định khi <= 0 nằm ở buildProactivePlan, vì chỉ mốc "chờ anh rep" mới bắt buộc.
func clampProactiveMinutes(n int) (minutes int, clamped bool) {
	switch {
	case n <= 0:
		return 0, false
	case n < minProactiveMinutes:
		return minProactiveMinutes, true
	case n > maxProactiveMinutes:
		return maxProactiveMinutes, true
	default:
		return n, false
	}
}

// buildProactivePlan dựng kế hoạch hoàn chỉnh từ những gì model vừa trả về: ép mốc "chờ anh rep"
// luôn tồn tại, kẹp từng mốc, bỏ mốc đè nhau (mốc chờ rep được ưu tiên giữ tuyệt đối), cắt còn
// maxProactiveSlots, trả về danh sách đã sắp tăng dần theo thời gian.
func buildProactivePlan(now time.Time, waitMinutes int, waitReason string, appts []proactiveAppointment) []proactiveSlot {
	wait, clamped := clampProactiveMinutes(waitMinutes)
	switch {
	case wait <= 0:
		log.Printf("proactive: model không đưa mốc chờ rep (%d) — ép mặc định %d phút",
			waitMinutes, defaultProactiveMinutes)
		wait = defaultProactiveMinutes
	case clamped:
		log.Printf("proactive: mốc chờ rep model chọn %d phút, kẹp lại còn %d phút", waitMinutes, wait)
	}

	// Mốc chờ rep đứng đầu danh sách ứng viên = ưu tiên tuyệt đối khi lọc đè nhau.
	cands := []proactiveSlot{{
		at:     now.Add(time.Duration(wait) * time.Minute),
		reason: strings.TrimSpace(waitReason),
		kind:   slotWait,
	}}

	appointments := make([]proactiveSlot, 0, len(appts))
	for _, a := range appts {
		m, c := clampProactiveMinutes(a.minutes)
		if m <= 0 {
			// Cuộc hẹn không có giờ thì bỏ hẳn — chỉ mốc chờ rep mới bị ép mặc định.
			continue
		}
		if c {
			log.Printf("proactive: cuộc hẹn model chọn %d phút, kẹp lại còn %d phút", a.minutes, m)
		}
		appointments = append(appointments, proactiveSlot{
			at:     now.Add(time.Duration(m) * time.Minute),
			reason: strings.TrimSpace(a.reason),
			kind:   slotAppointment,
		})
	}
	sort.SliceStable(appointments, func(i, j int) bool { return appointments[i].at.Before(appointments[j].at) })

	plan := dedupeByGap(append(cands, appointments...))
	if len(plan) > maxProactiveSlots {
		log.Printf("proactive: kế hoạch có %d mốc, cắt còn %d", len(plan), maxProactiveSlots)
		plan = plan[:maxProactiveSlots]
	}
	sort.SliceStable(plan, func(i, j int) bool { return plan[i].at.Before(plan[j].at) })
	return plan
}

// dedupeByGap duyệt các mốc THEO ĐÚNG THỨ TỰ ƯU TIÊN đã cho và bỏ mốc nào cách một mốc đã giữ dưới
// proactiveMinGap. Kết quả giữ nguyên thứ tự ưu tiên (người gọi tự sắp lại theo thời gian).
func dedupeByGap(cands []proactiveSlot) []proactiveSlot {
	kept := make([]proactiveSlot, 0, len(cands))
	for _, c := range cands {
		clash := false
		for _, k := range kept {
			if absDuration(k.at.Sub(c.at)) < proactiveMinGap {
				clash = true
				break
			}
		}
		if clash {
			log.Printf("proactive: bỏ mốc %s lúc %s vì cách mốc khác dưới %s",
				c.kind.label(), c.at.Format("15:04 02/01"), proactiveMinGap)
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// minutesUntil quy 1 mốc tuyệt đối về "còn bao nhiêu phút nữa" (làm tròn), dùng khi phải đưa kế
// hoạch đang chờ quay lại buildProactivePlan.
func minutesUntil(now, at time.Time) int {
	return int((at.Sub(now) + 30*time.Second) / time.Minute)
}

// proactiveScheduler giữ CẢ KẾ HOẠCH đang chờ (bot chỉ phục vụ 1 user whitelist nên không cần tách
// theo chat). Chỉ dùng đúng 1 *time.Timer — luôn nhắm mốc sớm nhất, bắn xong tự lên dây cho mốc kế.
// Mọi field dưới mutex riêng, tách khỏi botStatus, nên /status và /proactive đọc được ngay cả khi
// worker đang bận.
type proactiveScheduler struct {
	mu            sync.Mutex
	timer         *time.Timer
	slots         []proactiveSlot // sắp tăng dần theo at, đã đảm bảo cách nhau >= proactiveMinGap
	epoch         uint64          // tăng mỗi lần kế hoạch bị vô hiệu — xem messageWorker
	unanswered    int             // đã chủ động nhắn mấy lần liên tiếp mà anh chưa rep
	enabled       bool            // /proactive on|off
	chatID        int64           // chat gần nhất — cần vì tin chủ động không có update Telegram nào kèm theo
	allowedChatID int64           // policy bất biến từ validated config; persisted chatID không phải nguồn tin cậy
	jobs          chan<- chatJob
	store         settingsStore
}

func newProactiveScheduler(jobs chan<- chatJob, store settingsStore, allowedChatID int64) *proactiveScheduler {
	return &proactiveScheduler{enabled: true, jobs: jobs, store: store, allowedChatID: allowedChatID}
}

// AllowsChat is the final outbound boundary used by the worker as defense in depth.
func (s *proactiveScheduler) AllowsChat(chatID int64) bool {
	return s.allowedChatID > 0 && chatID == s.allowedChatID
}

// proactiveSnapshot là ảnh chụp trạng thái để hiển thị, tránh để chỗ gọi đụng thẳng vào mutex.
// nextAt/reason là của mốc sớm nhất; slots là cả kế hoạch (đã copy, an toàn để đọc thoải mái).
type proactiveSnapshot struct {
	enabled    bool
	nextAt     time.Time
	reason     string
	unanswered int
	chatID     int64
	slots      []proactiveSlot
}

func (s *proactiveScheduler) Snapshot() proactiveSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := proactiveSnapshot{
		enabled:    s.enabled,
		unanswered: s.unanswered,
		chatID:     s.chatID,
		slots:      append([]proactiveSlot(nil), s.slots...),
	}
	if len(s.slots) > 0 {
		snap.nextAt = s.slots[0].at
		snap.reason = s.slots[0].reason
	}
	return snap
}

// Epoch cho messageWorker biết kế hoạch hiện tại thuộc "đời" nào — job proactive mang epoch cũ
// (anh vừa nhắn tin lúc job đã nằm trong hàng đợi) sẽ bị bỏ thay vì nhắn chồng lên tin của anh.
func (s *proactiveScheduler) Epoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// Pending trả về các cuộc hẹn đang chờ (không gồm mốc chờ rep) để nhồi vào prompt lượt trích xuất —
// model phải nhìn thấy chúng thì mới chép lại được, vì mỗi lượt thay toàn bộ kế hoạch.
func (s *proactiveScheduler) Pending() []proactiveSlot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proactiveSlot, 0, len(s.slots))
	for _, sl := range s.slots {
		if sl.kind == slotAppointment {
			out = append(out, sl)
		}
	}
	return out
}

// SetPlan thay TOÀN BỘ kế hoạch cũ bằng kế hoạch model vừa đưa ra. chatID = 0 nghĩa là giữ nguyên
// chat hiện tại. Ghi luôn xuống settings để sống sót qua restart (best-effort, lỗi chỉ log).
func (s *proactiveScheduler) SetPlan(chatID int64, waitMinutes int, waitReason string, appts []proactiveAppointment) {
	s.mu.Lock()
	target := chatID
	if target == 0 {
		target = s.chatID
	}
	if !s.AllowsChat(target) {
		s.mu.Unlock()
		log.Printf("proactive: từ chối đặt kế hoạch reason=wrong_chat")
		return
	}
	plan := buildProactivePlan(time.Now(), waitMinutes, waitReason, appts)
	s.chatID = target
	if !s.enabled || s.chatID == 0 {
		s.slots = nil
		s.stopTimerLocked()
		s.mu.Unlock()
		s.persistPlan()
		return
	}
	s.slots = plan
	s.armLocked()
	s.mu.Unlock()

	s.persistPlan()
	log.Printf("[chat %d] proactive: kế hoạch mới, slots=%d", target, len(plan))
}

// Schedule đặt lại MỐC CHỜ REP, giữ nguyên các cuộc hẹn đang có. Dùng cho /proactive <phút> và cho
// những chỗ chỉ biết 1 con số. Vẫn đi qua bộ lọc giãn cách nên cuộc hẹn sát mốc mới vẫn bị bỏ.
func (s *proactiveScheduler) Schedule(chatID int64, minutes int, reason string) {
	s.SetPlan(chatID, minutes, reason, s.pendingAppointments())
}

// ClearAppointments xoá sạch cuộc hẹn, chừa lại đúng mốc chờ rep (/proactive clear).
func (s *proactiveScheduler) ClearAppointments() {
	now := time.Now()
	s.mu.Lock()
	minutes, reason := 0, ""
	for _, sl := range s.slots {
		if sl.kind == slotWait {
			minutes, reason = minutesUntil(now, sl.at), sl.reason
			break
		}
	}
	s.mu.Unlock()

	if minutes <= 0 {
		minutes, reason = defaultProactiveMinutes, "anh vừa dọn kế hoạch bằng /proactive clear"
	}
	s.SetPlan(0, minutes, reason, nil)
}

// pendingAppointments quy các cuộc hẹn đang chờ về số phút để đưa lại vào buildProactivePlan.
func (s *proactiveScheduler) pendingAppointments() []proactiveAppointment {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proactiveAppointment, 0, len(s.slots))
	for _, sl := range s.slots {
		if sl.kind != slotAppointment {
			continue
		}
		if m := minutesUntil(now, sl.at); m > 0 {
			out = append(out, proactiveAppointment{minutes: m, reason: sl.reason})
		}
	}
	return out
}

// Cancel huỷ toàn bộ kế hoạch đang chờ (không đụng tới unanswered).
func (s *proactiveScheduler) Cancel() {
	s.mu.Lock()
	s.slots = nil
	s.stopTimerLocked()
	s.mu.Unlock()
	s.persistPlan()
}

// NoteUserMessage: anh vừa nhắn tin thật → xoá kế hoạch cũ (lượt trích xuất của chính tin này sẽ
// đặt kế hoạch mới), reset chuỗi "đã nhắn mà chưa rep", và tăng epoch để job proactive đã lỡ nằm
// trong hàng đợi bị bỏ thay vì nhắn chồng lên tin của anh.
func (s *proactiveScheduler) NoteUserMessage(chatID int64) {
	if !s.AllowsChat(chatID) {
		log.Printf("proactive: bỏ qua user message reason=wrong_chat")
		return
	}
	s.mu.Lock()
	s.slots = nil
	s.stopTimerLocked()
	s.unanswered = 0
	s.chatID = chatID
	s.epoch++
	s.mu.Unlock()
	s.persistPlan()
}

// SetEnabled bật/tắt hẳn việc chủ động nhắn (/proactive on|off). Tắt thì huỷ luôn kế hoạch đang chờ.
func (s *proactiveScheduler) SetEnabled(on bool) {
	s.mu.Lock()
	s.enabled = on
	if !on {
		s.slots = nil
		s.stopTimerLocked()
		s.epoch++
	}
	s.mu.Unlock()

	value := "0"
	if on {
		value = "1"
	}
	saveSetting(s.store, settingProactiveEnabled, value, proactiveCommand)
	s.persistPlan()
}

// Restore nạp lại kế hoạch từ settings lúc khởi động — gọi 1 lần trong main trước khi vào vòng lặp,
// nên đọc/ghi field trực tiếp không cần khoá. Mốc còn ở tương lai giữ nguyên giờ; mốc đã quá hạn
// (bot tắt/deploy ngang) thì CHỈ mốc sớm nhất được dời lại sau proactiveRestoreDelay, các mốc quá
// hạn còn lại bỏ hẳn để không bắn dồn nhiều tin ngay lúc khởi động.
func (s *proactiveScheduler) Restore() {
	if s.store == nil {
		return
	}
	if v, ok, err := s.store.GetSetting(settingProactiveEnabled); err == nil && ok && v == "0" {
		s.enabled = false
	}
	if v, ok, err := s.store.GetSetting(settingProactiveChatID); err == nil && ok && v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			s.chatID = id
		}
	}
	if s.chatID != 0 && !s.AllowsChat(s.chatID) {
		log.Printf("proactive: không khôi phục kế hoạch reason=wrong_chat")
		s.chatID = 0
		s.unanswered = 0
		return
	}
	if v, ok, err := s.store.GetSetting(settingProactiveUnanswered); err == nil && ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			s.unanswered = n
		}
	}

	slots := s.restoreSlots()
	if len(slots) == 0 || !s.enabled || s.chatID == 0 {
		return
	}

	now := time.Now()
	out := make([]proactiveSlot, 0, len(slots))
	overdueTaken := false
	for _, sl := range slots {
		if sl.at.After(now) {
			out = append(out, sl)
			continue
		}
		if overdueTaken {
			log.Printf("proactive: bỏ mốc quá hạn lúc %s (đã dời 1 mốc rồi)",
				sl.at.Format("15:04 02/01"))
			continue
		}
		overdueTaken = true
		log.Printf("proactive: mốc cũ (%s) đã quá hạn lúc bot tắt — hẹn lại sau %s",
			sl.at.Format("15:04 02/01"), proactiveRestoreDelay)
		sl.at = now.Add(proactiveRestoreDelay)
		out = append(out, sl)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	out = dedupeByGap(out)

	s.slots = out
	s.armLocked()
	s.persistPlan()
	log.Printf("proactive: khôi phục kế hoạch cũ, slots=%d", len(out))
}

// restoreSlots đọc kế hoạch từ key mới; chưa có thì migrate 1 lần từ 2 key cũ thời còn 1 mốc.
func (s *proactiveScheduler) restoreSlots() []proactiveSlot {
	if s.store == nil {
		return nil
	}
	if raw, ok, err := s.store.GetSetting(settingProactivePlan); err == nil && ok && raw != "" {
		return decodeProactivePlan(raw)
	}

	raw, ok, err := s.store.GetSetting(settingProactiveAtLegacy)
	if err != nil || !ok || raw == "" {
		return nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		log.Printf("proactive: bỏ qua hẹn giờ cũ không đọc được: error_type=%T input_bytes=%d", err, len(raw))
		return nil
	}
	reason := ""
	if v, ok, err := s.store.GetSetting(settingProactiveReasonLegacy); err == nil && ok {
		reason = v
	}
	log.Printf("proactive: migrate hẹn giờ kiểu cũ (1 mốc) sang kế hoạch nhiều mốc")
	return []proactiveSlot{{at: at, reason: reason, kind: slotWait}}
}

// fire chạy trong goroutine riêng của time.AfterFunc. KHÔNG tự gửi tin ở đây — chỉ đẩy 1 job vào
// jobQueue để tin chủ động đi qua đúng messageWorker tuần tự. Gửi non-blocking: hàng đợi đầy
// (đang có tin thật của anh chờ) thì bỏ lượt này, tin của anh quan trọng hơn.
func (s *proactiveScheduler) fire() {
	s.mu.Lock()
	s.timer = nil
	if !s.enabled || s.chatID == 0 || len(s.slots) == 0 {
		s.mu.Unlock()
		return
	}
	if !s.AllowsChat(s.chatID) {
		s.chatID = 0
		s.slots = nil
		s.epoch++
		s.mu.Unlock()
		s.persistPlan()
		log.Printf("proactive: không bắn kế hoạch reason=wrong_chat")
		return
	}

	fired := s.slots[0]
	rest := s.slots[1:]
	// Mốc nào sát ngay sau mốc vừa bắn thì bỏ luôn — nếu không, 2 tin chủ động sẽ dính nhau và
	// nói cùng 1 chuyện (đúng luật "mốc 1 gần mốc 2 thì bỏ mốc 2").
	cut := 0
	for cut < len(rest) && rest[cut].at.Sub(fired.at) < proactiveMinGap {
		log.Printf("[chat %d] proactive: bỏ mốc %s lúc %s vì sát mốc vừa bắn",
			s.chatID, rest[cut].kind.label(), rest[cut].at.Format("15:04 02/01"))
		cut++
	}
	s.slots = append([]proactiveSlot(nil), rest[cut:]...)
	s.unanswered++
	chatID, round, epoch := s.chatID, s.unanswered, s.epoch
	s.armLocked()
	s.mu.Unlock()

	s.persistPlan()

	job := chatJob{
		chatID:          chatID,
		proactive:       true,
		proactiveRound:  round,
		proactiveReason: fired.reason,
		proactiveKind:   fired.kind,
		proactiveEpoch:  epoch,
	}
	select {
	case s.jobs <- job:
		log.Printf("[chat %d] proactive: tới giờ (%s), xếp lượt chủ động nhắn lần %d vào hàng đợi",
			chatID, fired.kind.label(), round)
	default:
		log.Printf("[chat %d] proactive: hàng đợi đầy, bỏ lượt chủ động nhắn lần %d", chatID, round)
	}
}

// armLocked lên dây timer cho mốc sớm nhất còn lại — người gọi phải đang giữ s.mu.
func (s *proactiveScheduler) armLocked() {
	s.stopTimerLocked()
	if !s.enabled || !s.AllowsChat(s.chatID) || len(s.slots) == 0 {
		return
	}
	d := time.Until(s.slots[0].at)
	if d < 0 {
		d = 0
	}
	s.timer = time.AfterFunc(d, s.fire)
}

// stopTimerLocked dừng timer đang chờ — người gọi phải đang giữ s.mu.
func (s *proactiveScheduler) stopTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// persistPlan ghi kế hoạch hiện tại xuống settings (best-effort) để restart khôi phục lại được.
// Đọc snapshot rồi mới ghi Markdown, không giữ mutex scheduler trong lúc I/O.
func (s *proactiveScheduler) persistPlan() {
	if s.store == nil {
		return
	}
	snap := s.Snapshot()
	if err := s.store.UpdateSettings(map[string]string{
		settingProactivePlan:       encodeProactivePlan(snap.slots),
		settingProactiveChatID:     strconv.FormatInt(snap.chatID, 10),
		settingProactiveUnanswered: strconv.Itoa(snap.unanswered),
	}); err != nil {
		log.Printf("%s: lỗi lưu proactive plan (vẫn áp dụng cho phiên này): %v", proactiveCommand, err)
	}
}

func encodeProactivePlan(slots []proactiveSlot) string {
	if len(slots) == 0 {
		return ""
	}
	out := make([]proactiveSlotJSON, 0, len(slots))
	for _, sl := range slots {
		out = append(out, proactiveSlotJSON{
			At:     sl.at.Format(time.RFC3339),
			Reason: sl.reason,
			Kind:   sl.kind.String(),
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		log.Printf("proactive: không mã hoá được kế hoạch: %v", err)
		return ""
	}
	return string(b)
}

func decodeProactivePlan(raw string) []proactiveSlot {
	var parsed []proactiveSlotJSON
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		log.Printf("proactive: bỏ qua kế hoạch cũ không đọc được: error_type=%T input_bytes=%d", err, len(raw))
		return nil
	}
	out := make([]proactiveSlot, 0, len(parsed))
	for _, p := range parsed {
		at, err := time.Parse(time.RFC3339, p.At)
		if err != nil {
			log.Printf("proactive: bỏ mốc có giờ hỏng: error_type=%T", err)
			continue
		}
		out = append(out, proactiveSlot{at: at, reason: p.Reason, kind: parseSlotKind(p.Kind)})
	}
	return out
}

// describePlanLog gói cả kế hoạch vào 1 dòng log để soi nhanh trong ani-telegram.log.
func describePlanLog(plan []proactiveSlot) string {
	if len(plan) == 0 {
		return "không có mốc nào"
	}
	parts := make([]string, 0, len(plan))
	for _, sl := range plan {
		parts = append(parts, fmt.Sprintf("%s [%s] %s", sl.at.Format("15:04 02/01"), sl.kind.label(), sl.reason))
	}
	return fmt.Sprintf("%d mốc: %s", len(plan), strings.Join(parts, " | "))
}

// maxReasonRunes cắt bớt lý do khi hiện trong /status. Prompt đã bắt model viết tối đa 12 từ, đây
// chỉ là lưới an toàn cho lượt nào model viết dài — để rộng rãi cho đủ 1 câu trọn vẹn thay vì cắt
// cụt giữa chừng.
const maxReasonRunes = 140

// describeProactiveLine dựng phần mô tả kế hoạch chủ động nhắn: 1 dòng "⏰ ..." cho mốc sớm nhất,
// rồi mỗi mốc còn lại 1 dòng "📌 ...". Dùng chung cho /status và /proactive để hai lệnh không bao
// giờ nói khác nhau.
func describeProactiveLine(snap proactiveSnapshot) string {
	if !snap.enabled {
		return "⏰ Chủ động nhắn đang TẮT (/proactive on để bật lại)."
	}
	if snap.nextAt.IsZero() {
		return "⏰ Chưa hẹn giờ chủ động nhắn (chưa có tin nhắn nào, hoặc kế hoạch vừa bị dọn)."
	}

	line := fmt.Sprintf("⏰ Sẽ chủ động nhắn anh lúc %s (còn %s nữa)",
		formatWhen(snap.nextAt), formatCountdown(time.Until(snap.nextAt)))
	if why := trimReason(snap.reason); why != "" {
		line += " — " + why
	}
	if snap.unanswered > 0 {
		line += fmt.Sprintf(" — đã chủ động nhắn %d lần liên tiếp anh chưa rep", snap.unanswered)
	}

	lines := []string{line}
	if len(snap.slots) > 1 {
		for _, sl := range snap.slots[1:] {
			l := fmt.Sprintf("📌 %s (còn %s nữa)", formatWhen(sl.at), formatCountdown(time.Until(sl.at)))
			if why := trimReason(sl.reason); why != "" {
				l += " — " + why
			}
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// formatWhen in giờ tuyệt đối: cùng ngày thì "21:15 hôm nay", khác ngày thì "07:30 ngày 21/08".
func formatWhen(t time.Time) string {
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04") + " hôm nay"
	}
	return t.Format("15:04 ngày 02/01")
}

// formatCountdown in khoảng còn lại kiểu người nói: "43 phút", "9 tiếng 12 phút", "2 ngày 3 tiếng".
func formatCountdown(d time.Duration) string {
	if d < time.Minute {
		return "dưới 1 phút"
	}
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	mins := int(d % time.Hour / time.Minute)
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%d ngày %d tiếng", days, hours)
	case days > 0:
		return fmt.Sprintf("%d ngày", days)
	case hours > 0 && mins > 0:
		return fmt.Sprintf("%d tiếng %d phút", hours, mins)
	case hours > 0:
		return fmt.Sprintf("%d tiếng", hours)
	default:
		return fmt.Sprintf("%d phút", mins)
	}
}

// trimReason cắt lý do model tự viết cho vừa 1 dòng /status.
func trimReason(reason string) string {
	r := strings.TrimSpace(reason)
	runes := []rune(r)
	if len(runes) > maxReasonRunes {
		return strings.TrimSpace(string(runes[:maxReasonRunes])) + "..."
	}
	return r
}

// describePendingAppointments dựng câu liệt kê cuộc hẹn đang chờ để nhồi vào prompt lượt trích
// xuất. Bắt buộc phải có, vì mỗi lượt thay TOÀN BỘ kế hoạch: model không chép lại = cuộc hẹn mất.
// Rỗng thì trả "" (chỗ gọi bỏ hẳn câu này).
func describePendingAppointments(appts []proactiveSlot) string {
	if len(appts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(appts))
	for _, sl := range appts {
		reason := trimReason(sl.reason)
		if reason == "" {
			reason = "không ghi lý do"
		}
		parts = append(parts, fmt.Sprintf("%s — %s", formatWhen(sl.at), reason))
	}
	return " Các cuộc hẹn em đã tự đặt từ trước và vẫn đang chờ: " + strings.Join(parts, "; ") +
		". Cuộc hẹn nào vẫn còn hợp lý thì PHẢI chép lại vào proactive_appointments (tính lại số phút kể từ giờ hệ thống hiện tại), cái nào không còn hợp lý nữa thì bỏ đi."
}

// handleProactiveCommand xử lý toàn bộ nhánh /proactive: không tham số = xem kế hoạch hiện tại,
// "off"/"on" = tắt/bật hẳn, "clear" = dọn hết cuộc hẹn, số = ép mốc chờ rep ngay N phút nữa (dùng
// để thử cho nhanh, khỏi chờ cả tiếng).
func handleProactiveCommand(sched *proactiveScheduler, chatID int64, arg string) string {
	switch {
	case arg == "":
		return describeProactiveLine(sched.Snapshot())

	case strings.EqualFold(arg, "off"):
		sched.SetEnabled(false)
		return "Đã TẮT — em sẽ chỉ nhắn khi anh nhắn trước. Gõ /proactive on để bật lại."

	case strings.EqualFold(arg, "on"):
		sched.SetEnabled(true)
		return "Đã BẬT chủ động nhắn.\n" + describeProactiveLine(sched.Snapshot()) +
			"\n(kế hoạch mới sẽ được đặt ngay sau tin nhắn kế tiếp của anh)"

	case strings.EqualFold(arg, "clear"):
		if !sched.Snapshot().enabled {
			return "Chủ động nhắn đang TẮT — gõ /proactive on trước đã nha anh."
		}
		sched.ClearAppointments()
		return "Đã dọn hết cuộc hẹn, chỉ còn mốc chờ anh rep.\n" + describeProactiveLine(sched.Snapshot())

	default:
		n, err := strconv.Atoi(strings.TrimSpace(arg))
		if err != nil || n <= 0 {
			return "Không hiểu tham số. Dùng: /proactive (xem), /proactive on, /proactive off, " +
				"/proactive clear, hoặc /proactive <số phút> để ép hẹn ngay."
		}
		if !sched.Snapshot().enabled {
			return "Chủ động nhắn đang TẮT — gõ /proactive on trước đã nha anh."
		}
		sched.Schedule(chatID, n, fmt.Sprintf("anh ép hẹn tay bằng %s %d", proactiveCommand, n))
		return "Ok.\n" + describeProactiveLine(sched.Snapshot())
	}
}
