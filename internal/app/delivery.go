package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"ani-telegram/internal/telegram"
)

type deliveryResult struct {
	Confirmed []string
	Total     int
	Outcome   string
}

type deliveryObserver func(sequence, total, attempt int, delay time.Duration)

type deliveryRecorder interface {
	BeforeAttempt(sequence int) error
	Confirm(sequence int, prefix []string, messageID int) error
}

type deliveryOptions struct {
	wait func(context.Context, time.Duration) error
}

func sendReply(ctx context.Context, tg *telegram.Client, chatID int64, reply string, record deliveryRecorder, observe deliveryObserver) (deliveryResult, error) {
	return sendReplyWithOptions(ctx, tg, chatID, reply, record, observe, deliveryOptions{})
}

func sendReplyWithOptions(ctx context.Context, tg *telegram.Client, chatID int64, reply string, record deliveryRecorder, observe deliveryObserver, options deliveryOptions) (deliveryResult, error) {
	result := deliveryResult{Outcome: "failed"}
	chunks := replyChunks(reply)
	result.Total = len(chunks)
	if len(chunks) == 0 {
		return result, errors.New("telegram reply is empty")
	}
	if tg == nil {
		return result, errors.New("telegram client is nil")
	}
	wait := options.wait
	if wait == nil {
		wait = waitForDelivery
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	for sequence, chunk := range chunks {
		if sequence > 0 {
			if err := wait(ctx, time.Second); err != nil {
				result.Outcome = "cancelled"
				return result, err
			}
		}
		for attempt := 1; attempt <= 5; attempt++ {
			if err := ctx.Err(); err != nil {
				result.Outcome = "cancelled"
				return result, err
			}
			if record != nil {
				if err := record.BeforeAttempt(sequence + 1); err != nil {
					result.Outcome = outcomeWithPrefix(result.Confirmed, "failed")
					return result, err
				}
			}
			if observe != nil {
				observe(sequence+1, len(chunks), attempt, 0)
			}
			message, err := tg.SendMessageResultContext(ctx, chatID, chunk)
			if err == nil {
				result.Confirmed = append(result.Confirmed, chunk)
				if record != nil {
					prefix := append([]string(nil), result.Confirmed...)
					if err := record.Confirm(sequence+1, prefix, message.MessageID); err != nil {
						if len(result.Confirmed) == result.Total {
							result.Outcome = "sent"
						} else {
							result.Outcome = outcomeWithPrefix(result.Confirmed, "failed")
						}
						return result, err
					}
				}
				break
			}
			var sendErr *telegram.SendError
			if !errors.As(err, &sendErr) {
				result.Outcome = "unknown"
				return result, err
			}
			switch sendErr.Kind {
			case "unknown":
				result.Outcome = "unknown"
				return result, err
			case "rejected":
				result.Outcome = outcomeWithPrefix(result.Confirmed, "failed")
				return result, err
			case "rate_limited", "not_sent":
				if ctxErr := ctx.Err(); ctxErr != nil {
					result.Outcome = "cancelled"
					return result, ctxErr
				}
				if attempt == 5 {
					result.Outcome = outcomeWithPrefix(result.Confirmed, "failed")
					return result, err
				}
				delay := retryDelay(sendErr, attempt)
				logDeliveryRetry(sequence+1, len(chunks), attempt, sendErr.HTTPStatus, sendErr.Code, delay)
				if observe != nil {
					observe(sequence+1, len(chunks), attempt, delay)
				}
				if waitErr := wait(ctx, delay); waitErr != nil {
					result.Outcome = "cancelled"
					return result, waitErr
				}
			default:
				result.Outcome = "unknown"
				return result, err
			}
		}
	}
	result.Outcome = "sent"
	return result, nil
}

func replyChunks(reply string) []string {
	var chunks []string
	for _, line := range strings.Split(reply, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			chunks = append(chunks, splitTelegramText(line, 4096)...)
		}
	}
	return chunks
}

func retryDelay(err *telegram.SendError, attempt int) time.Duration {
	if err.Kind == "rate_limited" {
		if err.RetryAfter > 0 {
			return err.RetryAfter
		}
		return 2 * time.Second
	}
	return time.Second << (attempt - 1)
}

func waitForDelivery(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func outcomeWithPrefix(confirmed []string, fallback string) string {
	if len(confirmed) > 0 {
		return "partial"
	}
	return fallback
}
