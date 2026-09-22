package app

import (
	"strings"
	"testing"
	"time"
)

// ─── Kế hoạch nhiều mốc ────────────────────────────────────────────────────
//
// newTestScheduler (main_test.go) dựng scheduler gắn Markdown store tạm — dùng lại ở đây.

// planAt trả về khoảng cách từ mốc gốc tới mốc thứ i, cho gọn phần assert.
func planAt(t *testing.T, plan []proactiveSlot, i int, now time.Time) time.Duration {
	t.Helper()
	if i >= len(plan) {
		t.Fatalf("kế hoạch chỉ có %d mốc, không có mốc thứ %d", len(plan), i)
	}
	return plan[i].at.Sub(now)
}

func TestBuildProactivePlan_LocMocSatNhau(t *testing.T) {
	now := time.Now()

	// Cuộc hẹn cách mốc chờ rep 15 phút (< proactiveMinGap) -> bỏ.
	plan := buildProactivePlan(now, 30, "chờ anh rep", []proactiveAppointment{{minutes: 45, reason: "sát quá"}})
	if len(plan) != 1 || plan[0].kind != slotWait {
		t.Fatalf("mốc sát nhau phải bị bỏ, còn lại: %s", describePlanLog(plan))
	}

	// Cách 60 phút -> giữ cả hai.
	plan = buildProactivePlan(now, 30, "chờ anh rep", []proactiveAppointment{{minutes: 90, reason: "nhắc ăn cơm"}})
	if len(plan) != 2 {
		t.Fatalf("cách đủ xa phải giữ cả 2 mốc, được: %s", describePlanLog(plan))
	}
	if plan[1].kind != slotAppointment || plan[1].reason != "nhắc ăn cơm" {
		t.Errorf("mốc thứ 2 sai: %+v", plan[1])
	}
	if d := planAt(t, plan, 1, now); d < 89*time.Minute || d > 91*time.Minute {
		t.Errorf("mốc thứ 2 phải cách ~90 phút, được %s", d)
	}
}

func TestBuildProactivePlan_UuTienMocChoRep(t *testing.T) {
	now := time.Now()
	// Cuộc hẹn sớm hơn mốc chờ rep nhưng sát nó -> vẫn phải bỏ cuộc hẹn, giữ mốc chờ rep.
	plan := buildProactivePlan(now, 30, "chờ anh rep", []proactiveAppointment{{minutes: 10, reason: "sát trước"}})
	if len(plan) != 1 || plan[0].kind != slotWait {
		t.Fatalf("mốc chờ rep phải được ưu tiên giữ, còn lại: %s", describePlanLog(plan))
	}
}

func TestBuildProactivePlan_EpMocChoRepKhiModelTraKhong(t *testing.T) {
	now := time.Now()
	for _, minutes := range []int{0, -5} {
		plan := buildProactivePlan(now, minutes, "", nil)
		if len(plan) != 1 || plan[0].kind != slotWait {
			t.Fatalf("waitMinutes=%d phải ép ra đúng 1 mốc chờ rep, được: %s", minutes, describePlanLog(plan))
		}
		if d := planAt(t, plan, 0, now); d != time.Duration(defaultProactiveMinutes)*time.Minute {
			t.Errorf("waitMinutes=%d phải thành %d phút, được %s", minutes, defaultProactiveMinutes, d)
		}
	}

	// Cuộc hẹn <= 0 thì bỏ hẳn — chỉ mốc chờ rep mới bắt buộc.
	plan := buildProactivePlan(now, 60, "", []proactiveAppointment{{minutes: 0, reason: "không giờ"}})
	if len(plan) != 1 {
		t.Errorf("cuộc hẹn 0 phút phải bị bỏ, được: %s", describePlanLog(plan))
	}
}

func TestBuildProactivePlan_CatToiDaSlots(t *testing.T) {
	now := time.Now()
	var appts []proactiveAppointment
	for i := 1; i <= 20; i++ {
		appts = append(appts, proactiveAppointment{minutes: 60 * i, reason: "hẹn"})
	}
	plan := buildProactivePlan(now, 20, "chờ anh rep", appts)
	if len(plan) != maxProactiveSlots {
		t.Fatalf("phải cắt còn %d mốc, được %d", maxProactiveSlots, len(plan))
	}
	// Cắt xong vẫn phải còn mốc chờ rep và vẫn sắp tăng dần.
	if plan[0].kind != slotWait {
		t.Errorf("mốc đầu phải là mốc chờ rep: %+v", plan[0])
	}
	for i := 1; i < len(plan); i++ {
		if !plan[i].at.After(plan[i-1].at) {
			t.Fatalf("kế hoạch không sắp tăng dần: %s", describePlanLog(plan))
		}
	}
}

func TestSetPlan_ThayToanBoKeHoachCu(t *testing.T) {
	sched, _, _ := newTestScheduler(t)
	sched.SetPlan(42, 60, "kế hoạch 1", []proactiveAppointment{
		{minutes: 300, reason: "cũ A"},
		{minutes: 600, reason: "cũ B"},
	})
	if got := len(sched.Snapshot().slots); got != 3 {
		t.Fatalf("kế hoạch 1 phải có 3 mốc, được %d", got)
	}

	sched.SetPlan(42, 45, "kế hoạch 2", []proactiveAppointment{{minutes: 400, reason: "mới"}})
	snap := sched.Snapshot()
	if len(snap.slots) != 2 {
		t.Fatalf("kế hoạch 2 phải thay TOÀN BỘ (2 mốc), được: %s", describePlanLog(snap.slots))
	}
	for _, sl := range snap.slots {
		if strings.HasPrefix(sl.reason, "cũ ") {
			t.Errorf("còn sót mốc của kế hoạch cũ: %+v", sl)
		}
	}
}

func TestProactiveScheduler_FireBoMocKeSatVaLenDayMocSau(t *testing.T) {
	sched, _, jobs := newTestScheduler(t)
	now := time.Now()
	sched.chatID = 42
	sched.slots = []proactiveSlot{
		{at: now, reason: "tới giờ", kind: slotWait},
		{at: now.Add(10 * time.Minute), reason: "sát quá", kind: slotAppointment},
		{at: now.Add(2 * time.Hour), reason: "hẹn xa", kind: slotAppointment},
	}

	sched.fire()

	select {
	case job := <-jobs:
		if job.proactiveReason != "tới giờ" || job.proactiveKind != slotWait {
			t.Errorf("job phải mang đúng mốc vừa bắn: %+v", job)
		}
	default:
		t.Fatal("fire() phải đẩy job vào hàng đợi")
	}

	snap := sched.Snapshot()
	if len(snap.slots) != 1 || snap.slots[0].reason != "hẹn xa" {
		t.Fatalf("mốc sát mốc vừa bắn phải bị bỏ, còn lại: %s", describePlanLog(snap.slots))
	}
	if snap.nextAt.IsZero() {
		t.Error("phải tự lên dây cho mốc kế tiếp")
	}
}

func TestProactiveScheduler_EpochDoiKhiAnhNhanXenVao(t *testing.T) {
	sched, _, jobs := newTestScheduler(t)
	sched.chatID = 42
	sched.slots = []proactiveSlot{{at: time.Now(), reason: "tới giờ", kind: slotWait}}
	sched.fire()

	job := <-jobs
	if job.proactiveEpoch != sched.Epoch() {
		t.Fatal("ngay sau khi bắn, job phải cùng đời với kế hoạch hiện tại")
	}

	// Anh nhắn tin xen vào lúc job còn nằm trong hàng đợi → messageWorker phải bỏ job này.
	sched.NoteUserMessage(42)
	if job.proactiveEpoch == sched.Epoch() {
		t.Error("NoteUserMessage phải đổi epoch để job cũ bị bỏ")
	}
}

func TestProactiveScheduler_PersistRestoreNhieuMoc(t *testing.T) {
	sched, db, jobs := newTestScheduler(t, 555)
	sched.SetPlan(555, 90, "anh nói đi ngủ", []proactiveAppointment{{minutes: 300, reason: "nhắc ăn cơm"}})
	before := sched.Snapshot()
	if len(before.slots) != 2 {
		t.Fatalf("chưa đặt được 2 mốc: %s", describePlanLog(before.slots))
	}

	// Giả lập restart: scheduler mới tinh, chỉ Markdown settings là còn.
	restored := newProactiveScheduler(jobs, db, 555)
	restored.Restore()
	t.Cleanup(restored.Cancel)

	got := restored.Snapshot()
	if len(got.slots) != len(before.slots) {
		t.Fatalf("mất mốc sau restart: %s", describePlanLog(got.slots))
	}
	for i := range got.slots {
		if got.slots[i].reason != before.slots[i].reason || got.slots[i].kind != before.slots[i].kind {
			t.Errorf("mốc %d lệch sau restart: %+v vs %+v", i, got.slots[i], before.slots[i])
		}
		if diff := got.slots[i].at.Sub(before.slots[i].at); diff > time.Minute || diff < -time.Minute {
			t.Errorf("mốc %d lệch giờ %s sau restart", i, diff)
		}
	}
}

func TestProactiveScheduler_RestoreMigrateTuKeyCu(t *testing.T) {
	sched, db, _ := newTestScheduler(t, 77)
	at := time.Now().Add(90 * time.Minute)
	if err := db.SetSetting(settingProactiveChatID, "77"); err != nil {
		t.Fatalf("ghi settings: %v", err)
	}
	if err := db.SetSetting(settingProactiveAtLegacy, at.Format(time.RFC3339)); err != nil {
		t.Fatalf("ghi settings: %v", err)
	}
	if err := db.SetSetting(settingProactiveReasonLegacy, "hẹn kiểu cũ"); err != nil {
		t.Fatalf("ghi settings: %v", err)
	}

	sched.Restore()
	snap := sched.Snapshot()
	if len(snap.slots) != 1 || snap.slots[0].kind != slotWait || snap.slots[0].reason != "hẹn kiểu cũ" {
		t.Fatalf("phải migrate được hẹn 1 mốc kiểu cũ, được: %s", describePlanLog(snap.slots))
	}
	if snap.chatID != 77 {
		t.Errorf("mất chatID khi migrate: %d", snap.chatID)
	}
}

func TestProactiveScheduler_RestoreNhieuMocQuaHanChiGiuMot(t *testing.T) {
	sched, db, _ := newTestScheduler(t, 77)
	now := time.Now()
	plan := encodeProactivePlan([]proactiveSlot{
		{at: now.Add(-2 * time.Hour), reason: "quá hạn 1", kind: slotWait},
		{at: now.Add(-time.Hour), reason: "quá hạn 2", kind: slotAppointment},
		{at: now.Add(5 * time.Hour), reason: "còn hạn", kind: slotAppointment},
	})
	if err := db.SetSetting(settingProactiveChatID, "77"); err != nil {
		t.Fatalf("ghi settings: %v", err)
	}
	if err := db.SetSetting(settingProactivePlan, plan); err != nil {
		t.Fatalf("ghi settings: %v", err)
	}

	sched.Restore()
	snap := sched.Snapshot()
	if len(snap.slots) != 2 {
		t.Fatalf("chỉ được dời 1 mốc quá hạn, bỏ phần còn lại, được: %s", describePlanLog(snap.slots))
	}
	if snap.slots[0].reason != "quá hạn 1" {
		t.Errorf("mốc quá hạn sớm nhất mới được dời lại: %+v", snap.slots[0])
	}
	if d := time.Until(snap.slots[0].at); d <= 0 || d > proactiveRestoreDelay+time.Minute {
		t.Errorf("mốc quá hạn phải dời khoảng %s, được %s", proactiveRestoreDelay, d)
	}
}

func TestProactiveScheduler_ClearAppointments(t *testing.T) {
	sched, _, _ := newTestScheduler(t)
	sched.SetPlan(42, 60, "chờ anh rep", []proactiveAppointment{{minutes: 300, reason: "nhắc ăn cơm"}})
	sched.ClearAppointments()

	snap := sched.Snapshot()
	if len(snap.slots) != 1 || snap.slots[0].kind != slotWait {
		t.Fatalf("clear phải chừa đúng mốc chờ rep, được: %s", describePlanLog(snap.slots))
	}
	if d := time.Until(snap.slots[0].at); d < 55*time.Minute || d > 61*time.Minute {
		t.Errorf("clear không được đổi giờ mốc chờ rep (còn ~60 phút), được %s", d)
	}
}

func TestProactiveScheduler_RejectsWrongChatWithoutMutatingPlan(t *testing.T) {
	sched, _, _ := newTestScheduler(t, 42)
	sched.SetPlan(42, 60, "valid plan", nil)
	before := sched.Snapshot()
	beforeEpoch := sched.Epoch()

	sched.SetPlan(99, 30, "wrong chat plan", nil)
	sched.NoteUserMessage(99)
	after := sched.Snapshot()

	if after.chatID != before.chatID || sched.Epoch() != beforeEpoch || len(after.slots) != len(before.slots) {
		t.Fatalf("wrong chat mutated scheduler: before=%+v after=%+v", before, after)
	}
	if !sched.AllowsChat(42) || sched.AllowsChat(99) {
		t.Fatal("AllowsChat không enforce exact configured chat")
	}
}

func TestProactiveScheduler_DropsPersistedAndQueuedWrongChat(t *testing.T) {
	_, db, jobs := newTestScheduler(t, 42)
	plan := encodeProactivePlan([]proactiveSlot{{at: time.Now().Add(time.Hour), reason: "stale", kind: slotWait}})
	if err := db.SetSetting(settingProactiveChatID, "99"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(settingProactivePlan, plan); err != nil {
		t.Fatal(err)
	}

	restored := newProactiveScheduler(jobs, db, 42)
	restored.Restore()
	t.Cleanup(restored.Cancel)
	if got := restored.Snapshot(); got.chatID != 0 || len(got.slots) != 0 {
		t.Fatalf("restored wrong-chat plan: %+v", got)
	}

	restored.chatID = 99
	restored.slots = []proactiveSlot{{at: time.Now(), reason: "stale queued", kind: slotWait}}
	restored.fire()
	select {
	case job := <-jobs:
		t.Fatalf("wrong-chat fire enqueued job: %+v", job)
	default:
	}
	if got := restored.Snapshot(); got.chatID != 0 || len(got.slots) != 0 {
		t.Fatalf("wrong-chat fire did not invalidate plan: %+v", got)
	}
}

func TestDescribeProactiveLine_NhieuMoc(t *testing.T) {
	now := time.Now()
	snap := proactiveSnapshot{
		enabled: true,
		nextAt:  now.Add(45 * time.Minute),
		reason:  "anh đang code dở",
		slots: []proactiveSlot{
			{at: now.Add(45 * time.Minute), reason: "anh đang code dở", kind: slotWait},
			{at: now.Add(5 * time.Hour), reason: "nhắc anh ăn cơm", kind: slotAppointment},
			{at: now.Add(9 * time.Hour), reason: "hỏi anh tan làm chưa", kind: slotAppointment},
		},
	}

	lines := strings.Split(describeProactiveLine(snap), "\n")
	if len(lines) != 3 {
		t.Fatalf("3 mốc phải ra 3 dòng, được %d: %q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "⏰") {
		t.Errorf("dòng đầu phải là mốc sớm nhất: %q", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "📌") {
			t.Errorf("cuộc hẹn phải in dòng riêng dạng 📌: %q", l)
		}
	}
	if !strings.Contains(lines[1], "nhắc anh ăn cơm") || !strings.Contains(lines[2], "tan làm") {
		t.Errorf("thiếu lý do cuộc hẹn: %q", lines)
	}
}

func TestDescribePendingAppointments(t *testing.T) {
	if got := describePendingAppointments(nil); got != "" {
		t.Errorf("không có cuộc hẹn thì phải trả rỗng, được %q", got)
	}

	got := describePendingAppointments([]proactiveSlot{
		{at: time.Now().Add(3 * time.Hour), reason: "nhắc anh ăn cơm", kind: slotAppointment},
	})
	if !strings.Contains(got, "nhắc anh ăn cơm") {
		t.Errorf("thiếu lý do cuộc hẹn: %q", got)
	}
	if !strings.Contains(got, "proactive_appointments") {
		t.Errorf("phải nói rõ model chép lại vào field nào: %q", got)
	}
}

func TestBuildProactiveTrigger(t *testing.T) {
	wait := buildProactiveTrigger(slotWait, "anh đang code dở", 2)
	if !strings.Contains(wait, "lần thứ 2") || strings.Contains(wait, "CUỘC HẸN") {
		t.Errorf("mốc chờ rep phải dùng trigger cũ: %q", wait)
	}

	appt := buildProactiveTrigger(slotAppointment, "nhắc anh ăn cơm", 1)
	if !strings.Contains(appt, "CUỘC HẸN") || !strings.Contains(appt, "nhắc anh ăn cơm") {
		t.Errorf("cuộc hẹn phải nhắc lại đúng lý do: %q", appt)
	}

	// Cuộc hẹn không có lý do thì quay về trigger chung, không để lọt chuỗi rỗng vào câu.
	empty := buildProactiveTrigger(slotAppointment, "  ", 1)
	if strings.Contains(empty, "CUỘC HẸN") {
		t.Errorf("cuộc hẹn rỗng lý do phải dùng trigger chung: %q", empty)
	}
}
