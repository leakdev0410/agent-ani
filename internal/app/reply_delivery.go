package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
	"ani-telegram/internal/telegram"
)

// guard every attempt, including retries after /new or an idle history reset.
type turnDeliveryRecorder struct {
	ctx          context.Context
	history      *historyStore
	chatID       int64
	generation   uint64
	journal      *journalDeliveryRecorder
	memoryFailed bool
}

func (r *turnDeliveryRecorder) BeforeAttempt(sequence int) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if !r.history.IsGeneration(r.chatID, r.generation) {
		return context.Canceled
	}
	if r.journal != nil {
		if err := r.journal.BeforeAttempt(sequence); err != nil {
			r.memoryFailed = true
			return err
		}
	}
	return nil
}

func (r *turnDeliveryRecorder) Confirm(sequence int, prefix []string, messageID int) error {
	if r.journal != nil {
		if err := r.journal.Confirm(sequence, prefix, messageID); err != nil {
			r.memoryFailed = true
			return err
		}
	}
	return nil
}

func finishForegroundReply(ctx context.Context, status *botStatus, db *memdb.DB, tg *telegram.Client, history *historyStore, sched *proactiveScheduler, chatID int64, snapshot []openrouter.Message, generation uint64, userText, reply string, round int, modelFailed bool, modelCode string, wake chan<- struct{}) {
	if !history.IsGeneration(chatID, generation) {
		status.SetOutcomeContext(ctx, replyOutcome{Kind: "cancelled", ErrorCode: "cancelled"})
		return
	}
	proactive := round > 0
	base := newMemoryJournalPayload(chatID, appendDeliveredHistory(snapshot, userText, "", proactive), round, sched, time.Now())
	base.TurnKind = "user"
	if proactive {
		base.TurnKind = "proactive"
	}
	base.SkipProactivePlan = modelFailed
	coordinator := status.deliveryCoordinator()
	id, beginErr := coordinator.Begin(db, base)
	record := &turnDeliveryRecorder{ctx: ctx, history: history, chatID: chatID, generation: generation}
	if beginErr == nil {
		defer coordinator.Release(id, wake)
		record.journal = &journalDeliveryRecorder{db: db, jobID: id, base: base}
		wakeMemoryJournal(wake)
	}
	observer := func(sequence, total, attempt int, delay time.Duration) {
		action := fmt.Sprintf("đang gửi đoạn %d/%d", sequence, total)
		if delay > 0 {
			action = fmt.Sprintf("chờ gửi lại đoạn %d/%d (lần %d)", sequence, total, attempt+1)
		}
		status.SetActionContext(ctx, action)
		if delay > 0 {
			logReplyRetry(chatID, "delivery", attempt, "", delay)
		}
	}
	result, sendErr := sendReply(ctx, tg, chatID, reply, record, observer)
	outcome := replyOutcome{Kind: result.Outcome, Confirmed: len(result.Confirmed), Total: result.Total, MemoryWarning: beginErr != nil || record.memoryFailed}
	if sendErr != nil {
		outcome.ErrorCode = replyErrorCode(sendErr)
		var telegramErr *telegram.SendError
		if errors.As(sendErr, &telegramErr) {
			outcome.HTTPStatus, outcome.TelegramCode = telegramErr.HTTPStatus, telegramErr.Code
		}
	}
	if record.memoryFailed {
		outcome.ErrorCode = "journal_error"
	}
	// A known ACK is delivery success even when its journal write failed.
	if result.Total > 0 && len(result.Confirmed) == result.Total {
		outcome.Kind = "sent"
	}
	payloadOutcome := outcome.Kind
	if modelFailed && outcome.Kind == "sent" {
		outcome.Kind = "model_failed"
		outcome.ErrorCode = modelCode
		payloadOutcome = "model_failed"
	}
	if record.journal != nil {
		if err := record.journal.Finalize(result.Confirmed, payloadOutcome); err != nil {
			outcome.MemoryWarning = true
		}
	}
	history.AppendDelivered(chatID, generation, userText, strings.Join(result.Confirmed, "\n"), proactive)
	status.SetOutcomeContext(ctx, outcome)
	logReplyOutcome(chatID, outcome)
	if outcome.Kind == "sent" {
		logAssistantMessage(chatID, round, strings.Join(result.Confirmed, "\n"))
	}
}

func replyErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var completion *openrouter.CompletionError
	if errors.As(err, &completion) {
		return normalizedReplyError(completion.Kind)
	}
	var send *telegram.SendError
	if errors.As(err, &send) {
		return normalizedReplyError(send.Kind)
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return "model_error"
}
