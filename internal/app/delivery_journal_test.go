package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
)

func mustMarshalDeliveryPayload(t *testing.T, payload memoryJournalPayload) []byte {
	t.Helper()
	raw, err := marshalDeliveryPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDeliveryJournalHeldBlocksExtractionUntilRelease(t *testing.T) {
	db := newJournalTestDB(t)
	c := &deliveryCoordinator{}
	base := memoryJournalPayload{ChatID: 42, History: []openrouter.Message{{Role: "user", Content: "real user"}}, Trigger: "time", TurnKind: "user"}
	id, err := c.Begin(db, base)
	if err != nil {
		t.Fatal(err)
	}
	createPendingJournalJob(t, db, 43)
	calls := 0
	p := memoryJournalProcessor{db: db, deliveryActive: c.Active, extract: func(_ context.Context, payload memoryJournalPayload) (string, error) {
		calls++
		if len(payload.History) != 2 || payload.History[1].Content != "A" || !payload.SkipProactivePlan {
			t.Fatalf("recovery payload=%+v", payload)
		}
		return `{"last_message_note":"note","emotional_state":"ok","mang1":"one","mang2":"two","mang3":"three"}`, nil
	}}
	if worked, err := p.ProcessNext(context.Background()); err != nil || worked || calls != 0 {
		t.Fatalf("held processed: %v %v calls=%d", worked, err, calls)
	}
	r := journalDeliveryRecorder{db: db, jobID: id, base: base}
	if err := r.BeforeAttempt(1); err != nil {
		t.Fatal(err)
	}
	if err := r.Confirm(1, []string{"A"}, 10); err != nil {
		t.Fatal(err)
	}
	if err := r.BeforeAttempt(2); err != nil {
		t.Fatal(err)
	}
	c.Release(id, nil)
	if worked, err := p.ProcessNext(context.Background()); err != nil || !worked || calls != 0 {
		t.Fatalf("recovery=%v %v calls=%d", worked, err, calls)
	}
	job, _, _ := db.NextMemoryJob()
	var payload memoryJournalPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if job.DeliveryPending || payload.DeliveryOutcome != "unknown" {
		t.Fatalf("job=%+v payload=%+v", job, payload)
	}
	if worked, err := p.ProcessNext(context.Background()); err != nil || !worked || calls != 1 {
		t.Fatalf("extraction=%v %v calls=%d", worked, err, calls)
	}
}

func TestDeliveryJournalRecoveryDiscardsProactiveWithoutACK(t *testing.T) {
	db := newJournalTestDB(t)
	c := &deliveryCoordinator{}
	id, err := c.Begin(db, memoryJournalPayload{ChatID: 42, Trigger: "time", TurnKind: "proactive"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMemoryDeliveryAttempt(id, 1); err != nil {
		t.Fatal(err)
	}
	job, _, _ := db.NextMemoryJob()
	if err := recoverHeldMemoryJob(db, job); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.NextMemoryJob(); err != nil || ok {
		t.Fatalf("zero-ACK proactive remained: %v %v", ok, err)
	}
}

func TestDeliveryJournalProcessNextDoesNotDiscardProactiveFinalizedWhileCheckingOwner(t *testing.T) {
	db := newJournalTestDB(t)
	base := memoryJournalPayload{ChatID: 42, Trigger: "time", TurnKind: "proactive"}
	c := &deliveryCoordinator{}
	id, err := c.Begin(db, base)
	if err != nil {
		t.Fatal(err)
	}
	recorded := append([]byte(nil), mustMarshalDeliveryPayload(t, base)...)
	checked := false
	p := memoryJournalProcessor{db: db, deliveryActive: func(gotID int64) bool {
		if checked {
			return false
		}
		checked = true
		if gotID != id {
			t.Fatalf("active id = %d, want %d", gotID, id)
		}
		if err := db.MarkMemoryDeliveryAttempt(id, 1); err != nil {
			t.Fatal(err)
		}
		if err := db.RecordMemoryDeliveryPrefix(id, 1, recorded); err != nil {
			t.Fatal(err)
		}
		if err := db.FinalizeMemoryDelivery(id, recorded); err != nil {
			t.Fatal(err)
		}
		c.Release(id, nil)
		return c.Active(id)
	}}

	if worked, err := p.ProcessNext(context.Background()); err != nil || !worked {
		t.Fatalf("ProcessNext = %v, %v; want recovery observation without error", worked, err)
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok {
		t.Fatalf("finalized proactive job missing: ok=%v err=%v", ok, err)
	}
	if job.ID != id || job.DeliveryPending {
		t.Fatalf("finalized proactive job = %+v", job)
	}
}

func TestDeliveryJournalProcessNextRecoversLatestUserACKAfterFinalizeFailure(t *testing.T) {
	db := newJournalTestDB(t)
	base := memoryJournalPayload{ChatID: 42, History: []openrouter.Message{{Role: "user", Content: "u"}}, Trigger: "time", TurnKind: "user"}
	c := &deliveryCoordinator{}
	id, err := c.Begin(db, base)
	if err != nil {
		t.Fatal(err)
	}
	acked := base
	acked.History = append(append([]openrouter.Message(nil), base.History...), openrouter.Message{Role: "assistant", Content: "confirmed"})
	ackedRaw := mustMarshalDeliveryPayload(t, acked)
	checked := false
	p := memoryJournalProcessor{db: db, deliveryActive: func(int64) bool {
		if checked {
			return false
		}
		checked = true
		if err := db.MarkMemoryDeliveryAttempt(id, 1); err != nil {
			t.Fatal(err)
		}
		if err := db.RecordMemoryDeliveryPrefix(id, 1, ackedRaw); err != nil {
			t.Fatal(err)
		}
		restore := db.SetMemoryJobCommitErrorForTest(func(*sql.Tx) error { return errors.New("finalize failed") })
		if err := db.FinalizeMemoryDelivery(id, ackedRaw); err == nil {
			t.Fatal("FinalizeMemoryDelivery unexpectedly succeeded")
		}
		restore()
		c.Release(id, nil)
		return c.Active(id)
	}}

	if worked, err := p.ProcessNext(context.Background()); err != nil || !worked {
		t.Fatalf("ProcessNext = %v, %v; want recovered latest ACK", worked, err)
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.DeliveryPending {
		t.Fatalf("recovered user job = %+v, ok=%v err=%v", job, ok, err)
	}
	var got memoryJournalPayload
	if err := json.Unmarshal(job.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 2 || got.History[1].Content != "confirmed" {
		t.Fatalf("recovery overwrote latest ACK: %+v", got.History)
	}
}

func TestDeliveryJournalFailureCannotPersistOrApplyProactivePlan(t *testing.T) {
	db := newJournalTestDB(t)
	if err := db.UpdateSettings(map[string]string{"proactive_plan": "original"}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(memoryJournalPayload{ChatID: 42, Trigger: "time", SkipProactivePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateMemoryJob(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StoreExtraction(id, `{"last_message_note":"note","emotional_state":"ok","mang1":"one","mang2":"two","mang3":"three","proactive_after_minutes":30}`); err != nil {
		t.Fatal(err)
	}
	called := false
	p := memoryJournalProcessor{db: db, apply: applyStoredMemoryJob, now: time.Now, onApplied: func(memoryJournalPayload, memoryUpdateJSON) { called = true }}
	if worked, err := p.ProcessNext(context.Background()); err != nil || !worked {
		t.Fatalf("apply=%v %v", worked, err)
	}
	// The processor still notifies non-scheduler consumers; scheduler policy is in the worker callback.
	if !called {
		t.Fatal("non-scheduler completion callback lost")
	}
	got, _, err := db.GetSetting("proactive_plan")
	if err != nil || got != "original" {
		t.Fatalf("failure replaced persisted schedule: %s %v", got, err)
	}
}

func TestDeliveryJournalPrefixIsRebuiltNotDuplicated(t *testing.T) {
	base := memoryJournalPayload{ChatID: 42, History: []openrouter.Message{{Role: "user", Content: "u"}}, Trigger: "time", TurnKind: "user"}
	db := newJournalTestDB(t)
	c := &deliveryCoordinator{}
	id, err := c.Begin(db, base)
	if err != nil {
		t.Fatal(err)
	}
	r := journalDeliveryRecorder{db: db, jobID: id, base: base}
	if err := r.BeforeAttempt(1); err != nil {
		t.Fatal(err)
	}
	if err := r.Confirm(1, []string{"A"}, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Confirm(1, []string{"A"}, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.BeforeAttempt(2); err != nil {
		t.Fatal(err)
	}
	if err := r.Confirm(2, []string{"A", "B"}, 2); err != nil {
		t.Fatal(err)
	}
	job, _, _ := db.NextMemoryJob()
	var payload memoryJournalPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.History) != 2 || payload.History[1].Content != "A\nB" {
		t.Fatalf("prefix duplicated: %+v", payload.History)
	}
	if strings.Contains(base.Trigger, "hệ thống vận chuyển") || len(base.History) != 1 {
		t.Fatal("base mutated")
	}
}

var _ deliveryRecorder = (*journalDeliveryRecorder)(nil)

func TestDeliveryJournalCrashReopensOnlyPersistedACKPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	base := memoryJournalPayload{ChatID: 42, History: []openrouter.Message{{Role: "user", Content: "u"}}, Trigger: "time", TurnKind: "user"}
	id, err := (&deliveryCoordinator{}).Begin(db, base)
	if err != nil {
		t.Fatal(err)
	}
	r := journalDeliveryRecorder{db: db, jobID: id, base: base}
	if err := r.BeforeAttempt(1); err != nil {
		t.Fatal(err)
	}
	if err := r.Confirm(1, []string{"A"}, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.BeforeAttempt(2); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = memdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := recoverHeldMemoryJobs(db); err != nil {
		t.Fatal(err)
	}
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || job.DeliveryPending {
		t.Fatalf("reopen recovery: %+v %v %v", job, ok, err)
	}
	var payload memoryJournalPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.History) != 2 || payload.History[1].Content != "A" || payload.DeliveryOutcome != "unknown" || !payload.SkipProactivePlan {
		t.Fatalf("recovered unconfirmed text: %+v", payload)
	}
}
