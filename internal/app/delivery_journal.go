package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"ani-telegram/internal/memdb"
)

// Ownership is registered under the same lock consulted by the FIFO worker.
// A committed held row can therefore never be mistaken for an abandoned turn
// in the gap between INSERT and foreground registration.
type deliveryCoordinator struct {
	mu     sync.Mutex
	active map[int64]bool
}

func (c *deliveryCoordinator) Begin(db *memdb.DB, payload memoryJournalPayload) (int64, error) {
	raw, err := marshalDeliveryPayload(payload)
	if err != nil {
		return 0, err
	}
	if db == nil {
		return 0, fmt.Errorf("memory journal unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := db.CreateHeldMemoryJobContext(ctx, raw)
	if err != nil {
		return 0, err
	}
	if c.active == nil {
		c.active = make(map[int64]bool)
	}
	c.active[id] = true
	return id, nil
}

func (c *deliveryCoordinator) Active(id int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active[id]
}

func (c *deliveryCoordinator) Release(id int64, wake chan<- struct{}) {
	c.mu.Lock()
	delete(c.active, id)
	c.mu.Unlock()
	wakeMemoryJournal(wake)
}

func wakeMemoryJournal(wake chan<- struct{}) {
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func marshalDeliveryPayload(payload memoryJournalPayload) ([]byte, error) {
	compacted, err := compactMemoryJournalPayload(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(compacted)
}

func deliveryPayload(base memoryJournalPayload, prefix []string, outcome string) memoryJournalPayload {
	payload := base
	payload.History = appendDeliveredHistory(base.History, "", strings.Join(prefix, "\n"), true)
	payload.DeliveryOutcome = normalizedOutcome(outcome)
	payload.SkipProactivePlan = base.SkipProactivePlan || outcome != "sent"
	return payload
}

// Store only already ACKed text. latest survives a failed write so finalization
// can catch up the full prefix without ever repeating the Telegram request.
type journalDeliveryRecorder struct {
	db     *memdb.DB
	jobID  int64
	base   memoryJournalPayload
	latest []string
}

func (r *journalDeliveryRecorder) BeforeAttempt(sequence int) error {
	return retryDeliveryWrite(func(ctx context.Context) error { return r.db.MarkMemoryDeliveryAttemptContext(ctx, r.jobID, sequence) })
}

func (r *journalDeliveryRecorder) Confirm(sequence int, prefix []string, _ int) error {
	r.latest = append([]string(nil), prefix...)
	raw, err := marshalDeliveryPayload(deliveryPayload(r.base, r.latest, "partial"))
	if err != nil {
		return err
	}
	return retryDeliveryWrite(func(ctx context.Context) error {
		return r.db.RecordMemoryDeliveryPrefixContext(ctx, r.jobID, sequence, raw)
	})
}

func (r *journalDeliveryRecorder) Finalize(prefix []string, outcome string) error {
	r.latest = append([]string(nil), prefix...)
	if r.base.TurnKind == "proactive" && len(prefix) == 0 {
		return retryDeliveryWrite(func(ctx context.Context) error { return r.db.DiscardMemoryJobContext(ctx, r.jobID) })
	}
	raw, err := marshalDeliveryPayload(deliveryPayload(r.base, prefix, outcome))
	if err != nil {
		return err
	}
	return retryDeliveryWrite(func(ctx context.Context) error { return r.db.FinalizeMemoryDeliveryContext(ctx, r.jobID, raw) })
}

// Independent from the send context: cancellation must not erase a known ACK.
// The same deadline bounds SQL connection acquisition and all retries.
func retryDeliveryWrite(write func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	delays := []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, time.Second}
	err := write(ctx)
	for _, delay := range delays {
		if err == nil {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		err = write(ctx)
	}
	return err
}

// No network/LLM work on recovery: an unobserved response is never replayed.
func recoverHeldMemoryJob(db *memdb.DB, job memdb.MemoryJob) error {
	held, err := db.HeldMemoryJobs()
	if err != nil {
		return err
	}
	var current memdb.MemoryJob
	found := false
	for _, candidate := range held {
		if candidate.ID == job.ID {
			current = candidate
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	job = current

	var payload memoryJournalPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.ChatID == 0 || strings.TrimSpace(payload.Trigger) == "" {
		return db.DiscardMemoryJob(job.ID)
	}
	if payload.TurnKind == "proactive" && job.DeliverySequence == 0 {
		return db.DiscardMemoryJob(job.ID)
	}
	payload.SkipProactivePlan = true
	payload.DeliveryOutcome = "interrupted"
	if job.DeliveryInflight != 0 {
		payload.DeliveryOutcome = "unknown"
	}
	raw, err := marshalDeliveryPayload(payload)
	if err != nil {
		return err
	}
	return db.FinalizeMemoryDelivery(job.ID, raw)
}

func recoverHeldMemoryJobs(db *memdb.DB) error {
	jobs, err := db.HeldMemoryJobs()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := recoverHeldMemoryJob(db, job); err != nil {
			return err
		}
	}
	return nil
}

func memoryDeliveryTrigger(payload memoryJournalPayload) string {
	if !payload.SkipProactivePlan && (payload.DeliveryOutcome == "" || payload.DeliveryOutcome == "sent") {
		return payload.Trigger
	}
	return payload.Trigger + "\n\n[Metadata hệ thống vận chuyển: outcome=" + normalizedOutcome(payload.DeliveryOutcome) + "; không tạo kế hoạch proactive từ lượt lỗi này. Chỉ các tin trong history là lời đã quan sát; không suy ra người dùng đã nói hoặc Ani đã thực hiện điều gì từ nhãn này. Thông báo lỗi của bot không phải sự kiện đời sống người dùng.]"
}
