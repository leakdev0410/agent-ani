package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ani-telegram/internal/memcore"
	"ani-telegram/internal/memdb"
	"ani-telegram/internal/memsearch"
	"ani-telegram/internal/openrouter"
)

const storedJobJSON = `{
  "last_message_note":"15:04: discussed durable jobs",
  "emotional_state":"focused",
  "tone_observation":"prefers atomic persistence",
  "preference_observation":"likes SQLite transactions",
  "diary_entry":"15:04: implemented durable memory jobs",
  "diary_topic":"projects",
  "mang1":"keep memory safe",
  "mang2":"verify recovery together",
  "mang3":"calm confidence",
  "topic_category":"projects",
  "topic_note":"Durable apply now deletes its temporary payload.",
  "proactive_after_minutes":45,
  "proactive_reason":"check the test result",
  "proactive_appointments":[{"after_minutes":180,"reason":"review recovery"}]
}`

// This end-to-end reopen test fails if a crash after exact extraction capture
// loses the durable job, re-asks the model, duplicates committed memory, or
// loses the committed-only semantic indexing work.
func TestCrashAfterExtractionThenRestartAppliesSavedJSONOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(memdb.TopicKey("projects"), "Committed project context."); err != nil {
		t.Fatal(err)
	}
	payload := memoryJournalPayload{
		ChatID:  42,
		History: []openrouter.Message{{Role: "user", Content: "temporary private chat"}},
		Trigger: "extract durable memory",
	}
	jobID, err := enqueueMemoryJob(db, payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	extractions := 0
	beforeCrash := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			extractions++
			return storedJobJSON, nil
		},
		apply: applyStoredMemoryJob,
	}
	if worked, err := beforeCrash.ProcessNext(context.Background()); err != nil || !worked {
		t.Fatalf("capture extraction = worked:%v err:%v", worked, err)
	}
	if extractions != 1 {
		t.Fatalf("extraction calls before crash = %d, want 1", extractions)
	}
	assertReadyStoredJob(t, db, jobID, storedJobJSON)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 26, 15, 4, 0, 0, time.UTC)
	afterRestart := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			t.Fatal("restart must apply the saved extraction without another model call")
			return "", nil
		},
		apply: applyStoredMemoryJob,
		now:   func() time.Time { return now },
	}
	if worked, err := afterRestart.ProcessNext(context.Background()); err != nil || !worked {
		t.Fatalf("apply saved extraction after restart = worked:%v err:%v", worked, err)
	}
	if _, ok, err := db.NextMemoryJob(); err != nil || ok {
		t.Fatalf("successful apply must delete temporary job: ok=%v err=%v", ok, err)
	}
	entries, err := db.DiaryEntriesForDate("2026-08-26")
	if err != nil || len(entries) != 1 || entries[0] != "15:04: implemented durable memory jobs" {
		t.Fatalf("recovered diary entries = %v, err=%v", entries, err)
	}
	queued, indexed, err := db.EmbeddingIndexCounts()
	if err != nil || queued != 4 || indexed != 0 {
		t.Fatalf("recovered semantic queue = queued:%d indexed:%d err:%v", queued, indexed, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entries, err = db.DiaryEntriesForDate("2026-08-26")
	if err != nil || len(entries) != 1 {
		t.Fatalf("second reopen must retain one applied diary entry: %v, err=%v", entries, err)
	}
	queued, indexed, err = db.EmbeddingIndexCounts()
	if err != nil || queued != 4 || indexed != 0 {
		t.Fatalf("second reopen changed semantic queue state: queued:%d indexed:%d err:%v", queued, indexed, err)
	}
}

func TestStoredJobAppliesOnceAndDeletesPayload(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Write(memdb.TopicKey("projects"), "Committed project context."); err != nil {
		t.Fatalf("seed projects topic: %v", err)
	}
	jobID, err := db.CreateMemoryJob([]byte(`{"temporary":"must be deleted"}`))
	if err != nil {
		t.Fatalf("create ready job: %v", err)
	}
	if err := db.StoreExtraction(jobID, storedJobJSON); err != nil {
		t.Fatalf("store extraction: %v", err)
	}

	now := time.Date(2026, time.August, 26, 15, 4, 0, 0, time.UTC)
	if err := applyStoredMemoryJob(db, jobID, now); err != nil {
		t.Fatalf("apply stored job: %v", err)
	}

	if _, ok, err := db.NextMemoryJob(); err != nil || ok {
		t.Fatalf("completed job must be deleted, ok=%v err=%v", ok, err)
	}
	state, err := db.GetCoreState()
	if err != nil {
		t.Fatalf("read core state: %v", err)
	}
	if state.LastMessageNote != "15:04: discussed durable jobs" || state.SleepNote != "" || state.EmotionalState != "focused" || state.Mang1 != "keep memory safe" || state.Mang2 != "verify recovery together" || state.Mang3 != "calm confidence" {
		t.Fatalf("unexpected core state: %+v", state)
	}
	entries, err := db.DiaryEntriesForDate("2026-08-26")
	if err != nil || len(entries) != 1 || entries[0] != "15:04: implemented durable memory jobs" {
		t.Fatalf("unexpected diary entries: %v, err=%v", entries, err)
	}
	byTopic, err := db.DiaryEntriesForTopic("projects", 0)
	if err != nil || len(byTopic) != 1 || byTopic[0].Text != "15:04: implemented durable memory jobs" {
		t.Fatalf("unexpected topic-tagged diary entries: %+v, err=%v", byTopic, err)
	}
	tones, err := db.Observations(memdb.ObservationTone)
	if err != nil || len(tones) != 1 || tones[0].Text != "prefers atomic persistence" {
		t.Fatalf("unexpected tone observations: %+v, err=%v", tones, err)
	}
	prefs, err := db.Observations(memdb.ObservationPreference)
	if err != nil || len(prefs) != 1 || prefs[0].Text != "likes SQLite transactions" {
		t.Fatalf("unexpected preference observations: %+v, err=%v", prefs, err)
	}
	notes, err := db.TopicNotes("projects")
	if err != nil || len(notes) != 1 || notes[0].Text != "Durable apply now deletes its temporary payload." {
		t.Fatalf("unexpected topic notes: %+v, err=%v", notes, err)
	}
	plan, ok, err := db.GetSetting(settingProactivePlan)
	if err != nil || !ok || plan == "" {
		t.Fatalf("proactive plan must be persisted, plan=%q ok=%v err=%v", plan, ok, err)
	}
	slots := decodeProactivePlan(plan)
	if len(slots) != 2 || slots[0].kind != slotWait || !slots[0].at.Equal(now.Add(45*time.Minute)) || slots[0].reason != "check the test result" || slots[1].kind != slotAppointment || !slots[1].at.Equal(now.Add(180*time.Minute)) || slots[1].reason != "review recovery" {
		t.Fatalf("unexpected durable proactive plan: %+v", slots)
	}
	queued := drainEmbeddingQueue(t, db)
	wantQueued := map[string]string{
		"diary:1:2026-08-26":                "15:04: implemented durable memory jobs",
		"observation:tone:1":                "prefers atomic persistence",
		"observation:preference:2":          "likes SQLite transactions",
		"topic_note:1:2026-08-26T15:04:00Z": "Durable apply now deletes its temporary payload.",
	}
	if fmt.Sprint(queued) != fmt.Sprint(wantQueued) {
		t.Fatalf("unexpected embedding sources: got=%v want=%v", queued, wantQueued)
	}

	if err := applyStoredMemoryJob(db, jobID, now); err == nil {
		t.Fatal("second apply must fail because the job was deleted")
	}
	entries, err = db.DiaryEntriesForDate("2026-08-26")
	if err != nil || len(entries) != 1 {
		t.Fatalf("second apply must not duplicate durable writes: %v, err=%v", entries, err)
	}
}

func TestStoredJobUsesExactCommittedSourceIDs(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Write(memdb.TopicKey("projects"), "Committed project context."); err != nil {
		t.Fatal(err)
	}
	raw := strings.ReplaceAll(storedJobJSON, "likes SQLite transactions", "same observation text")
	raw = strings.ReplaceAll(raw, "prefers atomic persistence", "same observation text")
	raw = strings.ReplaceAll(raw, `"topic_category":"projects"`, `"topic_category":"unknown"`)
	jobID, err := db.CreateMemoryJob([]byte(`{"chat_id":42}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StoreExtraction(jobID, raw); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 26, 15, 4, 0, 0, time.UTC)
	if err := applyStoredMemoryJob(db, jobID, now); err != nil {
		t.Fatalf("apply stored job with duplicate observations and skipped topic: %v", err)
	}
	if notes, err := db.TopicNotes("projects"); err != nil || len(notes) != 0 {
		t.Fatalf("unknown topic must not create a topic note: %+v, err=%v", notes, err)
	}
	if _, ok, err := db.NextMemoryJob(); err != nil || ok {
		t.Fatalf("completed exact-source job must be deleted, ok=%v err=%v", ok, err)
	}
	queued := drainEmbeddingQueue(t, db)
	want := map[string]string{
		"diary:1:2026-08-26":       "15:04: implemented durable memory jobs",
		"observation:tone:1":       "same observation text",
		"observation:preference:2": "same observation text",
	}
	if fmt.Sprint(queued) != fmt.Sprint(want) {
		t.Fatalf("embedding source IDs must be exact and complete: got=%v want=%v", queued, want)
	}
}

func TestStoredJobCommitFailureRecoversAndAppliesExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Write(memdb.TopicKey("projects"), "Committed project context."); err != nil {
		t.Fatal(err)
	}
	jobID, err := db.CreateMemoryJob([]byte(`{"chat_id":42}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StoreExtraction(jobID, storedJobJSON); err != nil {
		t.Fatal(err)
	}
	commitSeamSawDeletedJob := false
	clear := db.SetMemoryJobCommitErrorForTest(func(tx *sql.Tx) error {
		var remaining int
		if err := tx.QueryRow("SELECT COUNT(*) FROM memory_jobs WHERE id = ?", jobID).Scan(&remaining); err != nil {
			return fmt.Errorf("inspect final job deletion: %w", err)
		}
		if remaining != 0 {
			return fmt.Errorf("commit seam ran before deleting memory job %d", jobID)
		}
		commitSeamSawDeletedJob = true
		return errors.New("forced commit failure")
	})
	now := time.Date(2026, time.August, 26, 15, 4, 0, 0, time.UTC)
	if err := applyStoredMemoryJob(db, jobID, now); err == nil {
		t.Fatal("injected commit failure must fail the apply")
	}
	if !commitSeamSawDeletedJob {
		t.Fatal("commit seam must run after the transactional job deletion")
	}
	clear()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertReadyStoredJob(t, db, jobID, storedJobJSON)
	assertNoStoredJobMutation(t, db)
	if err := applyStoredMemoryJob(db, jobID, now); err != nil {
		t.Fatalf("recovered job must apply after commit fault is cleared: %v", err)
	}
	if _, ok, err := db.NextMemoryJob(); err != nil || ok {
		t.Fatalf("recovered job must be deleted exactly once, ok=%v err=%v", ok, err)
	}
	entries, err := db.DiaryEntriesForDate("2026-08-26")
	if err != nil || len(entries) != 1 || entries[0] != "15:04: implemented durable memory jobs" {
		t.Fatalf("recovery must commit one diary row: %v err=%v", entries, err)
	}
	state, err := db.GetCoreState()
	if err != nil || state.LastMessageNote != "15:04: discussed durable jobs" || state.EmotionalState != "focused" || state.Mang1 != "keep memory safe" || state.Mang2 != "verify recovery together" || state.Mang3 != "calm confidence" {
		t.Fatalf("recovery must commit the complete core state: %+v err=%v", state, err)
	}
	byTopic, err := db.DiaryEntriesForTopic("projects", 0)
	if err != nil || len(byTopic) != 1 || byTopic[0].Text != "15:04: implemented durable memory jobs" {
		t.Fatalf("recovery must preserve diary topic: %+v err=%v", byTopic, err)
	}
	tones, err := db.Observations(memdb.ObservationTone)
	if err != nil || len(tones) != 1 || tones[0].Text != "prefers atomic persistence" {
		t.Fatalf("recovery must commit tone observation: %+v err=%v", tones, err)
	}
	prefs, err := db.Observations(memdb.ObservationPreference)
	if err != nil || len(prefs) != 1 || prefs[0].Text != "likes SQLite transactions" {
		t.Fatalf("recovery must commit preference observation: %+v err=%v", prefs, err)
	}
	notes, err := db.TopicNotes("projects")
	if err != nil || len(notes) != 1 || notes[0].Text != "Durable apply now deletes its temporary payload." {
		t.Fatalf("recovery must commit topic note: %+v err=%v", notes, err)
	}
	plan, ok, err := db.GetSetting(settingProactivePlan)
	slots := decodeProactivePlan(plan)
	if err != nil || !ok || len(slots) != 2 || slots[0].kind != slotWait || !slots[0].at.Equal(now.Add(45*time.Minute)) || slots[0].reason != "check the test result" || slots[1].kind != slotAppointment || !slots[1].at.Equal(now.Add(180*time.Minute)) || slots[1].reason != "review recovery" {
		t.Fatalf("recovery must commit proactive plan: slots=%+v ok=%v err=%v", slots, ok, err)
	}
	queued := drainEmbeddingQueue(t, db)
	wantQueued := map[string]string{
		"diary:1:2026-08-26":                "15:04: implemented durable memory jobs",
		"observation:tone:1":                "prefers atomic persistence",
		"observation:preference:2":          "likes SQLite transactions",
		"topic_note:1:2026-08-26T15:04:00Z": "Durable apply now deletes its temporary payload.",
	}
	if fmt.Sprint(queued) != fmt.Sprint(wantQueued) {
		t.Fatalf("recovery must queue each exact committed source once: got=%v want=%v", queued, wantQueued)
	}
}

func assertReadyStoredJob(t *testing.T, db *memdb.DB, wantID int64, wantJSON string) {
	t.Helper()
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.ID != wantID || job.State != memdb.MemoryJobReadyToApply || job.ExtractionJSON != wantJSON {
		t.Fatalf("ready job must survive unchanged: job=%+v ok=%v err=%v", job, ok, err)
	}
}

func drainEmbeddingQueue(t *testing.T, db *memdb.DB) map[string]string {
	t.Helper()
	queued := map[string]string{}
	for {
		source, ok, err := db.NextEmbeddingSource()
		if err != nil {
			t.Fatalf("next embedding source: %v", err)
		}
		if !ok {
			return queued
		}
		wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(source.Text)))
		if source.ContentDigest != wantDigest {
			t.Fatalf("embedding digest for %q = %q, want %q", source.SourceKey, source.ContentDigest, wantDigest)
		}
		if _, exists := queued[source.SourceKey]; exists {
			t.Fatalf("duplicate embedding source key %q", source.SourceKey)
		}
		queued[source.SourceKey] = source.Text
		if err := db.StoreEmbedding(source.ID, source.ContentDigest, "test-model", []float32{1}); err != nil {
			t.Fatalf("consume embedding source: %v", err)
		}
	}
}

func TestStoredJobApplyFailuresRollbackAfterReopen(t *testing.T) {
	cases := []struct {
		name    string
		trigger string
	}{
		{"core state", "CREATE TRIGGER fail_core BEFORE INSERT ON core_state BEGIN SELECT RAISE(ABORT, 'forced core failure'); END"},
		{"diary", "CREATE TRIGGER fail_diary BEFORE INSERT ON diary_entries BEGIN SELECT RAISE(ABORT, 'forced diary failure'); END"},
		{"topic note", "CREATE TRIGGER fail_topic BEFORE INSERT ON topic_notes BEGIN SELECT RAISE(ABORT, 'forced topic failure'); END"},
		{"proactive settings", "CREATE TRIGGER fail_settings BEFORE INSERT ON settings BEGIN SELECT RAISE(ABORT, 'forced settings failure'); END"},
		{"final commit boundary", "CREATE TRIGGER fail_delete BEFORE DELETE ON memory_jobs BEGIN SELECT RAISE(ABORT, 'forced finalization failure'); END"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "memory.db")
			db, err := memdb.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Write(memdb.TopicKey("projects"), "Committed project context."); err != nil {
				t.Fatal(err)
			}
			jobID, err := db.CreateMemoryJob([]byte("temporary"))
			if err != nil {
				t.Fatal(err)
			}
			if err := db.StoreExtraction(jobID, storedJobJSON); err != nil {
				t.Fatal(err)
			}
			injectFailureTrigger(t, path, tc.trigger)
			now := time.Date(2026, time.August, 26, 15, 4, 0, 0, time.UTC)
			if err := applyStoredMemoryJob(db, jobID, now); err == nil {
				t.Fatal("injected write failure must reject the job apply")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			db, err = memdb.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			job, ok, err := db.NextMemoryJob()
			if err != nil || !ok || job.ID != jobID || job.State != memdb.MemoryJobReadyToApply || job.ExtractionJSON != storedJobJSON {
				t.Fatalf("ready job must survive unchanged after reopen: job=%+v ok=%v err=%v", job, ok, err)
			}
			assertNoStoredJobMutation(t, db)
		})
	}
}

func injectFailureTrigger(t *testing.T, path, statement string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open failure injector: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(statement); err != nil {
		t.Fatalf("create failure injector: %v", err)
	}
}

func assertNoStoredJobMutation(t *testing.T, db *memdb.DB) {
	t.Helper()
	state, err := db.GetCoreState()
	if err != nil || state != (memdb.CoreState{}) {
		t.Fatalf("core state leaked through rollback: %+v, err=%v", state, err)
	}
	entries, err := db.DiaryEntriesForDate("2026-08-26")
	if err != nil || len(entries) != 0 {
		t.Fatalf("diary leaked through rollback: %v, err=%v", entries, err)
	}
	tones, err := db.Observations(memdb.ObservationTone)
	if err != nil || len(tones) != 0 {
		t.Fatalf("observations leaked through rollback: %+v, err=%v", tones, err)
	}
	notes, err := db.TopicNotes("projects")
	if err != nil || len(notes) != 0 {
		t.Fatalf("topic notes leaked through rollback: %+v, err=%v", notes, err)
	}
	if _, ok, err := db.GetSetting(settingProactivePlan); err != nil || ok {
		t.Fatalf("proactive plan leaked through rollback: ok=%v err=%v", ok, err)
	}
	if _, ok, err := db.NextEmbeddingSource(); err != nil || ok {
		t.Fatalf("embedding work leaked through rollback: ok=%v err=%v", ok, err)
	}
}

// TestMemoryLifecycle_LocalKeywordRecall verifies the local durable-memory path
// without HTTP or an LLM: a realistic extracted update is saved to SQLite, then
// its distinctive keywords retrieve the saved diary entry through FTS5.
func TestMemoryLifecycle_LocalKeywordRecall(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := memcore.ApplyUpdate(db, memcore.Update{
		LastMessageNote:       "Anh chốt deadline FTS5 vào thứ Sáu.",
		SleepNote:             "Anh sẽ ngủ sớm.",
		EmotionalState:        "háo hức hoàn thiện FTS5",
		ToneObservation:       "Anh thích câu trả lời có checklist ngắn.",
		PreferenceObservation: "Anh ưu tiên test offline.",
		DiaryEntry:            "14:30: Anh chốt deadline FTS5 vào thứ Sáu.",
		DiaryTopic:            "projects",
		Mang1:                 "Muốn giúp anh hoàn thiện FTS5.",
		Mang2:                 "Muốn cùng anh kiểm tra memory.",
		Mang3:                 "Vui vì có tiến triển rõ ràng.",
	}); err != nil {
		t.Fatalf("save extracted memory: %v", err)
	}
	if err := memcore.ApplyUpdate(db, memcore.Update{
		LastMessageNote: "Anh muốn xem phim tối nay.",
		EmotionalState:  "thư giãn",
		DiaryEntry:      "20:00: Anh muốn xem phim tối nay.",
		Mang1:           "Muốn nghỉ ngơi.",
		Mang2:           "Muốn chọn phim cùng anh.",
		Mang3:           "Bình yên.",
	}); err != nil {
		t.Fatalf("save unrelated memory: %v", err)
	}

	got, err := memsearch.Executor(db)(memsearch.ToolName, `{"query":"deadline FTS5 thứ Sáu"}`)
	if err != nil {
		t.Fatalf("keyword recall: %v", err)
	}
	if !strings.Contains(got, "14:30: Anh chốt deadline FTS5 vào thứ Sáu.") {
		t.Errorf("keyword recall is missing the saved memory: %q", got)
	}
	if !strings.Contains(got, "projects") {
		t.Errorf("keyword recall is missing the saved topic: %q", got)
	}
	if strings.Contains(got, "Anh muốn xem phim tối nay.") {
		t.Errorf("keyword recall contains unrelated memory: %q", got)
	}
}
