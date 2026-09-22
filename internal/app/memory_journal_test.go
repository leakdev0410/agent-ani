package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
	"ani-telegram/internal/telegram"
)

type journalRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f journalRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestReplyIsSentAfterJobIsDurablyEnqueued(t *testing.T) {
	db := newJournalTestDB(t)
	status, history := &botStatus{}, newHistoryStore()
	wake := make(chan struct{}, 1)
	sent := 0
	installTurnTransport(t, func(req *http.Request) (*http.Response, error) {
		job, ok, err := db.NextMemoryJob()
		if err != nil || !ok || !job.DeliveryPending || job.ExtractionJSON != "" {
			t.Fatalf("send before durable held job: %+v %v %v", job, ok, err)
		}
		processor := memoryJournalProcessor{db: db, deliveryActive: status.deliveryCoordinator().Active, extract: func(context.Context, memoryJournalPayload) (string, error) {
			t.Fatal("extractor ran while sender active")
			return "", nil
		}}
		if worked, err := processor.ProcessNext(context.Background()); err != nil || worked {
			t.Fatalf("held gate failed: %v %v", worked, err)
		}
		sent++
		return turnACK(req), nil
	})
	ctx := status.StartChat(42, "test")
	snapshot, generation := history.SnapshotTurn(42)
	finishForegroundReply(ctx, status, db, telegram.NewClient("test-token"), history, nil, 42, snapshot, generation, "user", "reply", 0, false, "", wake)
	status.DoneContext(ctx)
	if sent != 1 {
		t.Fatalf("send count=%d", sent)
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.DeliveryPending {
		t.Fatalf("journal not released after ACK: %+v %v %v", job, ok, err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("journal worker not woken")
	}
}

func TestMemoryJournalPayloadBelowLimitIsUnchanged(t *testing.T) {
	payload := memoryJournalPayload{
		ChatID: 42,
		History: []openrouter.Message{
			{Role: "user", Content: "older context"},
			{Role: "assistant", Content: "latest reply"},
		},
		Trigger: "system time",
	}

	got, err := compactMemoryJournalPayload(payload)
	if err != nil {
		t.Fatalf("compactMemoryJournalPayload: %v", err)
	}
	wantRaw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	gotRaw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotRaw) != string(wantRaw) {
		t.Fatalf("payload below limit changed:\n got %s\nwant %s", gotRaw, wantRaw)
	}
}

func TestMemoryUpdatePromptLimitsMangs(t *testing.T) {
	if !strings.Contains(memoryUpdatePromptSuffix, "1.800 ký tự") {
		t.Fatalf("memory update prompt does not state the mang limit: %q", memoryUpdatePromptSuffix)
	}
}

func TestMemoryJournalPayloadCompactsOldestHistory(t *testing.T) {
	payload := memoryJournalPayload{
		ChatID: 42,
		History: []openrouter.Message{
			{Role: "user", Content: "OLDEST-USER-DO-NOT-KEEP " + strings.Repeat("x", memdb.MaxMemoryJobPayloadBytes)},
			{Role: "assistant", Content: "OLDEST-ASSISTANT-DO-NOT-KEEP"},
			{Role: "user", Content: "LATEST-USER-MUST-KEEP"},
			{Role: "assistant", Content: "LATEST-ASSISTANT-MUST-KEEP"},
		},
		Trigger: "system time",
	}

	got, err := compactMemoryJournalPayload(payload)
	if err != nil {
		t.Fatalf("compactMemoryJournalPayload: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > memdb.MaxMemoryJobPayloadBytes {
		t.Fatalf("compacted payload = %d bytes, limit %d", len(raw), memdb.MaxMemoryJobPayloadBytes)
	}
	joined := string(raw)
	for _, want := range []string{"LATEST-USER-MUST-KEEP", "LATEST-ASSISTANT-MUST-KEEP"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("compacted payload omitted newest turn marker %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "OLDEST-USER-DO-NOT-KEEP") || strings.Contains(joined, "OLDEST-ASSISTANT-DO-NOT-KEEP") {
		t.Fatalf("compacted payload retained oldest history: %s", joined)
	}
}

func TestDeliveryIntegrationSendsWhenJournalFails(t *testing.T) {
	db := newJournalTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	history, status := newHistoryStore(), &botStatus{}
	sent := 0
	installTurnTransport(t, func(req *http.Request) (*http.Response, error) { sent++; return turnACK(req), nil })
	ctx := status.StartChat(42, "test")
	snapshot, generation := history.SnapshotTurn(42)
	finishForegroundReply(ctx, status, db, telegram.NewClient("test-token"), history, nil, 42, snapshot, generation, "user", "reply", 0, false, "", nil)
	status.DoneContext(ctx)
	outcome := status.LastOutcome()
	if sent != 1 || outcome.Kind != "sent" || !outcome.MemoryWarning {
		t.Fatalf("closed DB stopped/misreported send: count=%d outcome=%+v", sent, outcome)
	}
	if got := history.Get(42); len(got) != 2 || got[1].Content != "reply" {
		t.Fatalf("confirmed reply missing: %v", got)
	}
}

func TestRestartResumesReadyJobWithoutCallingExtractionModel(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID, err := db.CreateMemoryJob([]byte(`{"chat_id":42}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StoreExtraction(jobID, `{"last_message_note":"recovered without extraction","emotional_state":"focused","mang1":"remember","mang2":"apply once","mang3":"safe"}`); err != nil {
		t.Fatal(err)
	}
	extractionCalled := false
	processor := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			extractionCalled = true
			return "", nil
		},
		apply: applyStoredMemoryJob,
		now:   func() time.Time { return time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC) },
	}

	worked, err := processor.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want successful recovery", worked, err)
	}
	if extractionCalled {
		t.Fatal("ready recovery called the extraction model")
	}
	if _, ok, err := db.NextMemoryJob(); err != nil || ok {
		t.Fatalf("ready job was not applied and deleted: ok=%v err=%v", ok, err)
	}
}

func TestDiscardedJournalJobAllowsFollowingReadyJob(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	createPendingJournalJob(t, db, 42)
	readyID := createPendingJournalJob(t, db, 42)
	if err := db.StoreExtraction(readyID, `{"last_message_note":"must wait","emotional_state":"focused","mang1":"one","mang2":"two","mang3":"three"}`); err != nil {
		t.Fatal(err)
	}
	var appliedID int64
	extractionCalls := 0
	processor := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			extractionCalls++
			return `{}`, nil
		},
		apply: func(db *memdb.DB, id int64, _ time.Time) error {
			appliedID = id
			return db.ApplyAndDeleteMemoryJob(id, func(*sql.Tx, string) error { return nil })
		},
		now: time.Now,
	}

	worked, err := processor.ProcessNext(context.Background())
	if err != nil || !worked || extractionCalls != memoryJournalSchemaAttempts {
		t.Fatalf("first ProcessNext = (%v, %v), extraction calls=%d, want failed job discarded", worked, err, extractionCalls)
	}
	worked, err = processor.ProcessNext(context.Background())
	if err != nil || !worked || appliedID != readyID {
		t.Fatalf("second ProcessNext = (%v, %v), appliedID=%d, want ready job %d", worked, err, appliedID, readyID)
	}
	pending, ready, blocked, err := db.MemoryJobCounts()
	if err != nil || pending != 0 || ready != 0 || blocked != 0 {
		t.Fatalf("MemoryJobCounts = (%d, %d, %d, %v), want (0, 0, 0, nil)", pending, ready, blocked, err)
	}
}

func TestPendingExtractionRetriesSchemaInvalidResponse(t *testing.T) {
	db := newJournalTestDB(t)
	jobID := createPendingJournalJob(t, db, 42)
	valid := `{"last_message_note":"valid retry","emotional_state":"focused","mang1":"one","mang2":"two","mang3":"three"}`
	calls := 0
	processor := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			calls++
			if calls == 1 {
				return `{}`, nil
			}
			return valid, nil
		},
		now: time.Now,
	}

	worked, err := processor.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want successful retry", worked, err)
	}
	if calls != 2 {
		t.Fatalf("extraction calls = %d, want 2", calls)
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.ID != jobID || job.State != memdb.MemoryJobReadyToApply || job.ExtractionJSON != valid {
		t.Fatalf("retried job = %+v, ok=%v, err=%v", job, ok, err)
	}
}

func TestPendingExtractionDiscardsAfterSchemaRetryBudget(t *testing.T) {
	db := newJournalTestDB(t)
	createPendingJournalJob(t, db, 42)
	calls := 0
	processor := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			calls++
			return `{}`, nil
		},
		now: time.Now,
	}

	worked, err := processor.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want invalid extraction discarded", worked, err)
	}
	if calls != 2 {
		t.Fatalf("extraction calls = %d, want 2", calls)
	}
	pending, ready, blocked, err := db.MemoryJobCounts()
	if err != nil || pending != 0 || ready != 0 || blocked != 0 {
		t.Fatalf("MemoryJobCounts = (%d, %d, %d, %v), want (0, 0, 0, nil)", pending, ready, blocked, err)
	}
}

func TestPendingExtractionDiscardsAfterInvalidResponseThenExtractorError(t *testing.T) {
	db := newJournalTestDB(t)
	createPendingJournalJob(t, db, 42)
	calls := 0
	processor := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			calls++
			if calls == 1 {
				return `{}`, nil
			}
			return "", errors.New("transient transport failure")
		},
		now: time.Now,
	}

	worked, err := processor.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want discarded job after invalid response", worked, err)
	}
	if calls != 2 {
		t.Fatalf("extraction calls = %d, want 2", calls)
	}
	pending, ready, blocked, err := db.MemoryJobCounts()
	if err != nil || pending != 0 || ready != 0 || blocked != 0 {
		t.Fatalf("MemoryJobCounts = (%d, %d, %d, %v), want (0, 0, 0, nil)", pending, ready, blocked, err)
	}
}

func TestReadyInvalidRequiredExtractionIsDiscarded(t *testing.T) {
	db := newJournalTestDB(t)
	jobID, err := db.CreateMemoryJob([]byte(`{"chat_id":42,"history":[{"role":"user","content":"x"}],"trigger":"now"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StoreExtraction(jobID, `{}`); err != nil {
		t.Fatal(err)
	}
	processor := memoryJournalProcessor{db: db, now: time.Now}

	worked, err := processor.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want invalid ready extraction discarded", worked, err)
	}
	pending, ready, blocked, err := db.MemoryJobCounts()
	if err != nil || pending != 0 || ready != 0 || blocked != 0 {
		t.Fatalf("MemoryJobCounts = (%d, %d, %d, %v), want (0, 0, 0, nil)", pending, ready, blocked, err)
	}
}

func TestCapturedExtractionSurvivesFinalizeFailureAndRestartWithoutSecondModelCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(memdb.TopicKey("projects"), "project context"); err != nil {
		t.Fatal(err)
	}
	jobID := createPendingJournalJob(t, db, 42)
	raw := storedJobJSON
	calls := 0
	clear := db.SetMemoryJobFinalizeExtractionErrorForTest(func() error { return errors.New("forced finalize failure") })
	processor := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			calls++
			return raw, nil
		},
		apply: applyStoredMemoryJob,
		now:   func() time.Time { return time.Date(2026, time.August, 26, 15, 4, 0, 0, time.UTC) },
	}
	if _, err := processor.ProcessNext(context.Background()); err == nil {
		t.Fatal("forced finalize failure must be returned")
	}
	if calls != 1 {
		t.Fatalf("ChatJSON calls = %d, want 1", calls)
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.ID != jobID || job.State != memdb.MemoryJobPendingExtraction || job.ExtractionJSON != raw {
		t.Fatalf("captured response must survive finalization failure: job=%+v ok=%v err=%v", job, ok, err)
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
	processor.db = db
	if _, err := processor.ProcessNext(context.Background()); err != nil {
		t.Fatalf("restart finalizes captured extraction: %v", err)
	}
	if _, err := processor.ProcessNext(context.Background()); err != nil {
		t.Fatalf("restart applies stored extraction: %v", err)
	}
	if calls != 1 {
		t.Fatalf("restart re-called model: calls=%d", calls)
	}
	if _, ok, err := db.NextMemoryJob(); err != nil || ok {
		t.Fatalf("recovered job must be applied once: ok=%v err=%v", ok, err)
	}
}

func TestCaptureFailureDiscardsJobAndPreventsSecondModelCallAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	jobID := createPendingJournalJob(t, db, 42)
	calls := 0
	clear := db.SetMemoryJobCaptureExtractionErrorForTest(func() error { return errors.New("forced capture failure") })
	processor := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			calls++
			return storedJobJSON, nil
		},
		now: time.Now,
	}
	if _, err := processor.ProcessNext(context.Background()); err == nil {
		t.Fatal("forced capture failure must be returned")
	}
	if calls != 1 {
		t.Fatalf("ChatJSON calls = %d, want 1", calls)
	}
	inspect, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open inspection database: %v", err)
	}
	err = inspect.QueryRow("SELECT 1 FROM memory_jobs WHERE id = ?", jobID).Scan(new(int))
	if closeErr := inspect.Close(); closeErr != nil {
		t.Fatalf("close inspection database: %v", closeErr)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("capture failure must discard its journal job: err=%v", err)
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
	processor.db = db
	worked, err := processor.ProcessNext(context.Background())
	if err != nil || worked {
		t.Fatalf("restart ProcessNext = (%v, %v), want no actionable job", worked, err)
	}
	if calls != 1 {
		t.Fatalf("restart re-called model after capture failure: calls=%d", calls)
	}
}

func TestPendingJobCapturesThenAppliesOnNextFIFOPass(t *testing.T) {
	db := newJournalTestDB(t)
	if err := db.Write(memdb.TopicKey("projects"), "project context"); err != nil {
		t.Fatal(err)
	}
	jobID := createPendingJournalJob(t, db, 42)
	calls := 0
	processor := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			calls++
			return storedJobJSON, nil
		},
		apply: applyStoredMemoryJob,
		now:   func() time.Time { return time.Date(2026, time.August, 26, 15, 4, 0, 0, time.UTC) },
	}
	if worked, err := processor.ProcessNext(context.Background()); err != nil || !worked {
		t.Fatalf("capture pass = (%v, %v)", worked, err)
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.ID != jobID || job.State != memdb.MemoryJobReadyToApply || job.ExtractionJSON != storedJobJSON {
		t.Fatalf("pending job must retain exactly one ready extraction: job=%+v ok=%v err=%v", job, ok, err)
	}
	if worked, err := processor.ProcessNext(context.Background()); err != nil || !worked {
		t.Fatalf("apply pass = (%v, %v)", worked, err)
	}
	if calls != 1 {
		t.Fatalf("pending lifecycle called extraction %d times, want 1", calls)
	}
	if _, ok, err := db.NextMemoryJob(); err != nil || ok {
		t.Fatalf("applied pending job must be deleted: ok=%v err=%v", ok, err)
	}
}

func TestMultiplePendingJobsApplyInFIFOOrder(t *testing.T) {
	db := newJournalTestDB(t)
	if err := db.Write(memdb.TopicKey("projects"), "project context"); err != nil {
		t.Fatal(err)
	}
	firstID := createPendingJournalJob(t, db, 42)
	secondID := createPendingJournalJob(t, db, 42)
	modelCalls := 0
	processor := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			modelCalls++
			return storedJobJSON, nil
		},
		apply: applyStoredMemoryJob,
		now:   func() time.Time { return time.Date(2026, time.August, 26, 15, 4, 0, 0, time.UTC) },
	}

	for pass := 0; pass < 2; pass++ {
		if worked, err := processor.ProcessNext(context.Background()); err != nil || !worked {
			t.Fatalf("first job pass %d = (%v, %v)", pass, worked, err)
		}
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.ID != secondID || job.State != memdb.MemoryJobPendingExtraction {
		t.Fatalf("second job must wait until first is fully applied: job=%+v ok=%v err=%v first=%d", job, ok, err, firstID)
	}
	for pass := 0; pass < 2; pass++ {
		if worked, err := processor.ProcessNext(context.Background()); err != nil || !worked {
			t.Fatalf("second job pass %d = (%v, %v)", pass, worked, err)
		}
	}
	if modelCalls != 2 {
		t.Fatalf("two FIFO jobs must make two extraction calls, got %d", modelCalls)
	}
	if _, ok, err := db.NextMemoryJob(); err != nil || ok {
		t.Fatalf("both FIFO jobs must be deleted: ok=%v err=%v", ok, err)
	}
}

func TestDiscardedExtractionRestoresDefaultProactivePlanAfterNormalTurn(t *testing.T) {
	testDiscardedExtractionRestoresDefaultProactivePlan(t, 0)
}

func TestDiscardedExtractionRestoresDefaultProactivePlanAfterProactiveTurn(t *testing.T) {
	testDiscardedExtractionRestoresDefaultProactivePlan(t, 1)
}

func testDiscardedExtractionRestoresDefaultProactivePlan(t *testing.T, unanswered int) {
	t.Helper()
	db := newJournalTestDB(t)
	sched := newProactiveScheduler(make(chan chatJob, 1), db, 42)
	sched.NoteUserMessage(42) // normal turns clear a previous plan; proactive slots are already consumed.
	payload := memoryJournalPayload{
		ChatID:     42,
		History:    []openrouter.Message{{Role: "assistant", Content: "reply"}},
		Trigger:    "system time",
		Unanswered: unanswered,
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateMemoryJob(rawPayload); err != nil {
		t.Fatal(err)
	}
	processor := memoryJournalProcessor{
		db: db,
		extract: func(context.Context, memoryJournalPayload) (string, error) {
			return `{}`, nil
		},
		now: time.Now,
		onDiscard: func(payload memoryJournalPayload) {
			applyBlockedMemoryJobFallback(sched, payload)
		},
	}
	if _, err := processor.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := sched.Snapshot()
	if snap.nextAt.IsZero() || snap.chatID != 42 {
		t.Fatalf("discarded extraction must restore a private-safe default proactive plan: %+v", snap)
	}
}

func TestStatusWithMemoryJobsDoesNotExposePrivateUserText(t *testing.T) {
	private := "PRIVATE-USER-MARKER-DO-NOT-LEAK"
	status := &botStatus{}
	status.StartChat(42, userMessageStatusAction(private))
	db := newJournalTestDB(t)
	if _, err := db.CreateMemoryJob([]byte(private)); err != nil {
		t.Fatal(err)
	}
	sched := newProactiveScheduler(make(chan chatJob, 1), db, 42)
	got := describeStatusWithMemoryJobs(status, 0, sched, db)
	if strings.Contains(got, private) {
		t.Fatalf("status leaked private chat content: %q", got)
	}
}

func TestStatusWithMemoryJobsReportsOnlyJournalCountsAndAge(t *testing.T) {
	db := newJournalTestDB(t)
	pendingID := createPendingJournalJob(t, db, 42)
	readyID := createPendingJournalJob(t, db, 42)
	if err := db.StoreExtraction(readyID, storedJobJSON); err != nil {
		t.Fatal(err)
	}
	got := describeStatusWithMemoryJobs(&botStatus{}, 0, newProactiveScheduler(make(chan chatJob, 1), db, 42), db)
	for _, want := range []string{"pending 1", "ready 1", "job cũ nhất"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status %q missing %q (pending job %d)", got, want, pendingID)
		}
	}
	if strings.Contains(got, "blocked") {
		t.Fatalf("status must not report discarded state: %q", got)
	}
}

func TestMemoryJournalUsesExtractionPromptContract(t *testing.T) {
	db := newJournalTestDB(t)
	if err := db.Write(memdb.TopicKey("projects"), "PRIVATE-TOPIC-ONLY-IN-SYSTEM-PROMPT"); err != nil {
		t.Fatal(err)
	}
	got, err := memoryJournalExtractionPrompt(db, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "PRIVATE-TOPIC-ONLY-IN-SYSTEM-PROMPT") || strings.Contains(got, "# Skills có sẵn") {
		t.Fatalf("worker extraction prompt must exclude topics and skills: %q", got)
	}
}

func TestMemoryJournalWorkerDrainsExistingJobAndUsesNarrowPrompt(t *testing.T) {
	db := newJournalTestDB(t)
	if err := db.Write(memdb.TopicKey("projects"), "PRIVATE-TOPIC-ONLY-IN-SYSTEM-PROMPT"); err != nil {
		t.Fatal(err)
	}
	createPendingJournalJob(t, db, 42)

	var mu sync.Mutex
	modelCalls := 0
	extractionPrompt := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("model path = %q, want /chat/completions", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode extraction request: %v", err)
			return
		}
		mu.Lock()
		modelCalls++
		if len(request.Messages) > 0 {
			extractionPrompt, _ = request.Messages[0].Content.(string)
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": storedJobJSON}}}})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		memoryJournalWorker(ctx, db, openrouter.NewClient("test-key", server.URL, "test-model"), newPromptStore("cached full prompt must not be used"), nil, make(chan struct{}))
		close(done)
	}()
	awaitJournalCondition(t, "startup worker drain", func() bool {
		_, ok, err := db.NextMemoryJob()
		if err != nil {
			t.Errorf("read drained journal: %v", err)
			return true
		}
		return !ok
	})
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if modelCalls != 1 {
		t.Fatalf("startup drain ChatJSON calls = %d, want 1", modelCalls)
	}
	if strings.Contains(extractionPrompt, "PRIVATE-TOPIC-ONLY-IN-SYSTEM-PROMPT") || strings.Contains(extractionPrompt, "# Skills có sẵn") || !strings.Contains(extractionPrompt, "NHIỆM VỤ ĐẶC BIỆT") {
		t.Fatalf("worker supplied the wrong extraction prompt: %q", extractionPrompt)
	}
}

func TestMemoryJournalWorkerCancellationLeavesPendingJobForRecovery(t *testing.T) {
	db := newJournalTestDB(t)
	jobID := createPendingJournalJob(t, db, 42)
	requestStarted := make(chan struct{}, 1)
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requestStarted <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-releaseRequest:
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		memoryJournalWorker(ctx, db, openrouter.NewClient("test-key", server.URL, "test-model"), nil, nil, make(chan struct{}))
		close(done)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("worker did not start the pending extraction request")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after cancellation")
	}
	close(releaseRequest)
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.ID != jobID || job.State != memdb.MemoryJobPendingExtraction || job.ExtractionJSON != "" {
		t.Fatalf("cancellation must retain the pending job untouched: job=%+v ok=%v err=%v", job, ok, err)
	}
}

func TestForegroundPathsDurablyEnqueueBeforeTelegramSend(t *testing.T) {
	for _, test := range []struct {
		name           string
		invoke         func(context.Context, *botStatus, *memdb.DB, *openrouter.Client, *telegram.Client, *promptStore, *historyStore, *proactiveScheduler, chan<- struct{})
		wantUnanswered int
	}{
		{
			name: "user message",
			invoke: func(ctx context.Context, status *botStatus, db *memdb.DB, llm *openrouter.Client, tg *telegram.Client, prompt *promptStore, history *historyStore, sched *proactiveScheduler, wake chan<- struct{}) {
				processMessage(ctx, status, db, llm, tg, prompt, history, sched, "", 42, "hello", false, nil, wake)
			},
		},
		{
			name: "proactive message",
			invoke: func(ctx context.Context, status *botStatus, db *memdb.DB, llm *openrouter.Client, tg *telegram.Client, prompt *promptStore, history *historyStore, sched *proactiveScheduler, wake chan<- struct{}) {
				processProactive(ctx, status, db, llm, tg, prompt, history, sched, "", 42, 1, slotWait, "check in", wake)
			},
			wantUnanswered: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newJournalTestDB(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" {
					t.Errorf("model path = %q, want /chat/completions", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "foreground reply"}}}})
			}))
			defer server.Close()

			wake := make(chan struct{}, 1)
			var sentJob memdb.MemoryJob
			sent := false
			originalTransport := http.DefaultTransport
			http.DefaultTransport = journalRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "api.telegram.org" {
					return originalTransport.RoundTrip(req)
				}
				if req.URL.Path != "/bottest-token/sendMessage" {
					return nil, errors.New("unexpected Telegram endpoint")
				}
				job, ok, err := db.NextMemoryJob()
				if err != nil {
					return nil, err
				}
				if !ok || job.State != memdb.MemoryJobPendingExtraction || !job.DeliveryPending || job.ExtractionJSON != "" {
					return nil, errors.New("Telegram send occurred before durable journal enqueue")
				}
				select {
				case <-wake:
				default:
					return nil, errors.New("Telegram send occurred before journal worker wake")
				}
				sentJob, sent = job, true
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`)),
					Request:    req,
				}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = originalTransport })

			status := &botStatus{}
			ctx := status.StartChat(42, "integration test")
			test.invoke(ctx, status, db, openrouter.NewClient("test-key", server.URL, "test-model"), telegram.NewClient("test-token"), newPromptStore("foreground system prompt"), newHistoryStore(), nil, wake)
			if !sent {
				t.Fatal("foreground path did not send the model reply")
			}
			var payload memoryJournalPayload
			if err := json.Unmarshal(sentJob.Payload, &payload); err != nil {
				t.Fatalf("decode foreground journal payload: %v", err)
			}
			if payload.ChatID != 42 || payload.Unanswered != test.wantUnanswered || strings.TrimSpace(payload.Trigger) == "" {
				t.Fatalf("foreground path wired the wrong journal payload: %+v", payload)
			}
			if test.wantUnanswered == 0 && (len(payload.History) != 1 || payload.History[0].Role != "user") {
				t.Fatalf("user input missing or draft persisted before ACK: %+v", payload.History)
			}
			if test.wantUnanswered > 0 && len(payload.History) != 0 {
				t.Fatalf("proactive draft persisted before ACK: %+v", payload.History)
			}
		})
	}
}

func awaitJournalCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newJournalTestDB(t *testing.T) *memdb.DB {
	t.Helper()
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createPendingJournalJob(t *testing.T, db *memdb.DB, chatID int64) int64 {
	t.Helper()
	payload := memoryJournalPayload{
		ChatID:  chatID,
		History: []openrouter.Message{{Role: "user", Content: "test"}},
		Trigger: "system time",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateMemoryJob(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
