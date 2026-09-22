package memcore

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"ani-telegram/internal/memdb"
)

func TestApplyUpdateTxWritesOnlyThroughCallerTransaction(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	jobID, err := db.CreateMemoryJob([]byte("test"))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.StoreExtraction(jobID, `{}`); err != nil {
		t.Fatalf("ready job: %v", err)
	}
	update := Update{
		LastMessageNote: "transactional note",
		EmotionalState:  "steady",
		DiaryEntry:      "09:30: written inside transaction",
		DiaryTopic:      "projects",
		Mang1:           "protect durable state",
		Mang2:           "test it together",
		Mang3:           "relieved",
	}
	now := time.Date(2026, time.August, 26, 9, 30, 0, 0, time.UTC)
	err = db.ApplyAndDeleteMemoryJob(jobID, func(tx *sql.Tx, raw string) error {
		return ApplyUpdateTx(tx, update, now)
	})
	if err != nil {
		t.Fatalf("apply transactionally: %v", err)
	}

	entries, err := db.DiaryEntriesForDate("2026-08-26")
	if err != nil || len(entries) != 1 || entries[0] != "09:30: written inside transaction" {
		t.Fatalf("unexpected transactionally committed diary: %v, err=%v", entries, err)
	}
}

func TestApplyUpdate_WritesCoreState(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()

	update := Update{
		LastMessageNote:       "TEST-MARKER-last-message",
		EmotionalState:        "TEST-MARKER-emotional",
		ToneObservation:       "TEST-MARKER-tone",
		PreferenceObservation: "TEST-MARKER-preference",
		DiaryEntry:            "23:59: TEST-MARKER-diary",
		DiaryTopic:            "projects",
		Mang1:                 "TEST-MARKER-mang1",
		Mang2:                 "TEST-MARKER-mang2",
		Mang3:                 "TEST-MARKER-mang3",
	}
	if err := ApplyUpdate(db, update); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	state, err := db.GetCoreState()
	if err != nil {
		t.Fatalf("GetCoreState: %v", err)
	}
	checks := map[string]string{
		"LastMessageNote": state.LastMessageNote,
		"EmotionalState":  state.EmotionalState,
		"Mang1":           state.Mang1,
		"Mang2":           state.Mang2,
		"Mang3":           state.Mang3,
	}
	want := map[string]string{
		"LastMessageNote": "TEST-MARKER-last-message",
		"EmotionalState":  "TEST-MARKER-emotional",
		"Mang1":           "TEST-MARKER-mang1",
		"Mang2":           "TEST-MARKER-mang2",
		"Mang3":           "TEST-MARKER-mang3",
	}
	for field, got := range checks {
		if got != want[field] {
			t.Errorf("%s = %q, muốn %q", field, got, want[field])
		}
	}

	today := time.Now().Format("2006-01-02")
	entries, err := db.DiaryEntriesForDate(today)
	if err != nil {
		t.Fatalf("DiaryEntriesForDate: %v", err)
	}
	if len(entries) != 1 || entries[0] != "23:59: TEST-MARKER-diary" {
		t.Errorf("diary_entries hôm nay = %v, muốn 1 entry đúng nội dung", entries)
	}

	byTopic, err := db.DiaryEntriesForTopic("projects", 0)
	if err != nil {
		t.Fatalf("DiaryEntriesForTopic: %v", err)
	}
	if len(byTopic) != 1 || byTopic[0].Text != "23:59: TEST-MARKER-diary" || byTopic[0].EntryDate != today {
		t.Errorf("diary_entries theo chủ đề = %+v, muốn 1 entry đúng nội dung + ngày hôm nay", byTopic)
	}

	tones, err := db.Observations(memdb.ObservationTone)
	if err != nil {
		t.Fatalf("Observations(tone): %v", err)
	}
	if len(tones) != 1 || tones[0].Text != "TEST-MARKER-tone" || tones[0].ObservedAt != today {
		t.Errorf("observations(tone) = %+v, muốn 1 entry đúng nội dung + ngày hôm nay", tones)
	}

	prefs, err := db.Observations(memdb.ObservationPreference)
	if err != nil {
		t.Fatalf("Observations(preference): %v", err)
	}
	if len(prefs) != 1 || prefs[0].Text != "TEST-MARKER-preference" {
		t.Errorf("observations(preference) = %+v, muốn 1 entry đúng nội dung", prefs)
	}
}

// TestApplyUpdate_OptionalFieldsDontOverwrite kiểm tra SleepNote rỗng thì giữ nguyên giá trị cũ,
// và không có ToneObservation/PreferenceObservation/DiaryEntry thì không tự thêm row rác.
func TestApplyUpdate_OptionalFieldsDontOverwrite(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()

	if err := db.SaveCoreState(memdb.CoreState{SleepNote: "giá trị cũ"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	update := Update{
		LastMessageNote: "x",
		EmotionalState:  "x",
		Mang1:           "x",
		Mang2:           "x",
		Mang3:           "x",
		// SleepNote, ToneObservation, PreferenceObservation, DiaryEntry đều để rỗng.
	}
	if err := ApplyUpdate(db, update); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	state, err := db.GetCoreState()
	if err != nil {
		t.Fatalf("GetCoreState: %v", err)
	}
	if state.SleepNote != "giá trị cũ" {
		t.Errorf("SleepNote = %q, muốn giữ nguyên %q", state.SleepNote, "giá trị cũ")
	}

	today := time.Now().Format("2006-01-02")
	entries, err := db.DiaryEntriesForDate(today)
	if err != nil {
		t.Fatalf("DiaryEntriesForDate: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("không có DiaryEntry nhưng vẫn tạo row: %v", entries)
	}

	tones, err := db.Observations(memdb.ObservationTone)
	if err != nil {
		t.Fatalf("Observations(tone): %v", err)
	}
	if len(tones) != 0 {
		t.Errorf("không có ToneObservation nhưng vẫn tạo row: %v", tones)
	}
}

func TestApplyUpdate_MissingRequiredField(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()

	update := Update{EmotionalState: "x", Mang1: "x", Mang2: "x", Mang3: "x"} // thiếu LastMessageNote
	if err := ApplyUpdate(db, update); err == nil {
		t.Errorf("mong đợi lỗi khi thiếu field bắt buộc, nhưng không có lỗi")
	}
}

// TestApplyUpdate_InvalidDiaryTopicBecomesEmpty kiểm tra topic ngoài danh sách hợp lệ (hoặc
// "none") bị chuẩn hoá thành rỗng — tránh model tạo chủ đề lạ ngoài danh sách.
func TestApplyUpdate_InvalidDiaryTopicBecomesEmpty(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("mở db tạm: %v", err)
	}
	defer db.Close()

	update := Update{
		LastMessageNote: "x",
		EmotionalState:  "x",
		Mang1:           "x",
		Mang2:           "x",
		Mang3:           "x",
		DiaryEntry:      "22:00: TEST-MARKER-diary-topic-lạ",
		DiaryTopic:      "nhạc", // không nằm trong ValidTopics
	}
	if err := ApplyUpdate(db, update); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	all, err := db.DiaryEntriesWithoutTopic()
	if err != nil {
		t.Fatalf("DiaryEntriesWithoutTopic: %v", err)
	}
	if len(all) != 1 || all[0].Text != "22:00: TEST-MARKER-diary-topic-lạ" {
		t.Errorf("topic lạ phải bị chuẩn hoá thành rỗng, được: %+v", all)
	}
}

func TestNormalizeTopic(t *testing.T) {
	cases := map[string]string{
		"projects":      "projects",
		"Personal":      "personal",
		" preferences ": "preferences",
		"log":           "log",
		"none":          "",
		"":              "",
		"nhạc":          "",
		"PROJECTS":      "projects",
	}
	for input, want := range cases {
		if got := NormalizeTopic(input); got != want {
			t.Errorf("NormalizeTopic(%q) = %q, muốn %q", input, got, want)
		}
	}
}
