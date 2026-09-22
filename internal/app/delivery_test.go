package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ani-telegram/internal/telegram"
)

type deliveryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f deliveryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type recorderStub struct {
	before  func(int) error
	confirm func(int, []string, int) error
}

func (r recorderStub) BeforeAttempt(sequence int) error {
	if r.before != nil {
		return r.before(sequence)
	}
	return nil
}
func (r recorderStub) Confirm(sequence int, prefix []string, messageID int) error {
	if r.confirm != nil {
		return r.confirm(sequence, prefix, messageID)
	}
	return nil
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func testClient(t *testing.T, fn deliveryRoundTripFunc) *telegram.Client {
	t.Helper()
	return telegram.NewClientWithHTTPClient("test-token", &http.Client{Transport: fn})
}

func requestText(t *testing.T, req *http.Request) string {
	t.Helper()
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Text
}

func noWait(context.Context, time.Duration) error { return nil }

func TestDeliveryRetriesRateLimitAndContinuesFromCorrectChunk(t *testing.T) {
	var attempted []string
	client := testClient(t, func(req *http.Request) (*http.Response, error) {
		text := requestText(t, req)
		attempted = append(attempted, text)
		if text == "B" && len(attempted) == 2 {
			return response(429, `{"ok":false,"error_code":429,"parameters":{"retry_after":2}}`), nil
		}
		return response(200, `{"ok":true,"result":{"message_id":7}}`), nil
	})

	result, err := sendReplyWithOptions(context.Background(), client, 22, "A\nB\nC", nil, nil, deliveryOptions{wait: noWait})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "B", "B", "C"}
	if !reflect.DeepEqual(attempted, want) {
		t.Fatalf("attempt order=%v", attempted)
	}
	if result.Outcome != "sent" || strings.Join(result.Confirmed, "\n") != "A\nB\nC" {
		t.Fatalf("unexpected outcome=%s confirmed=%d", result.Outcome, len(result.Confirmed))
	}
}

func TestDeliveryStopsAfterRejectedOrUnknownWithoutReplay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure func() (*http.Response, error)
		outcome string
	}{
		{name: "rejected", failure: func() (*http.Response, error) {
			return response(403, `{"ok":false,"error_code":403,"description":"Forbidden"}`), nil
		}, outcome: "partial"},
		{name: "unknown after send", failure: func() (*http.Response, error) { return nil, errors.New("response lost") }, outcome: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempted []string
			client := testClient(t, func(req *http.Request) (*http.Response, error) {
				text := requestText(t, req)
				attempted = append(attempted, text)
				if text == "B" {
					return tc.failure()
				}
				return response(200, `{"ok":true,"result":{"message_id":7}}`), nil
			})
			result, err := sendReplyWithOptions(context.Background(), client, 22, "A\nB\nC", nil, nil, deliveryOptions{wait: noWait})
			if err == nil {
				t.Fatal("expected error")
			}
			if !reflect.DeepEqual(attempted, []string{"A", "B"}) || !reflect.DeepEqual(result.Confirmed, []string{"A"}) || result.Outcome != tc.outcome {
				t.Fatalf("attempted=%v result=%+v", attempted, result)
			}
		})
	}
}

func TestDeliveryTLSRecordErrorAfterBodyIsUnknownWithoutReplay(t *testing.T) {
	attempts := 0
	client := testClient(t, func(req *http.Request) (*http.Response, error) {
		attempts++
		if _, err := io.ReadAll(req.Body); err != nil {
			t.Fatal(err)
		}
		return nil, tls.RecordHeaderError{Msg: "bad record after request body"}
	})
	result, err := sendReplyWithOptions(context.Background(), client, 22, "A", nil, nil, deliveryOptions{wait: noWait})
	if err == nil || attempts != 1 || result.Outcome != "unknown" || len(result.Confirmed) != 0 {
		t.Fatalf("attempts=%d result=%+v err=%v", attempts, result, err)
	}
}

func TestDeliveryHonorsMaximumAttempts(t *testing.T) {
	attempts := 0
	client := testClient(t, func(*http.Request) (*http.Response, error) {
		attempts++
		return response(429, `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`), nil
	})
	result, err := sendReplyWithOptions(context.Background(), client, 22, "A", nil, nil, deliveryOptions{wait: noWait})
	if err == nil || attempts != 5 || result.Outcome != "failed" {
		t.Fatalf("attempts=%d result=%+v err=%v", attempts, result, err)
	}
}

func TestDeliveryCancellationDuringWaitAndInFlight(t *testing.T) {
	t.Run("between chunks", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		client := testClient(t, func(*http.Request) (*http.Response, error) {
			attempts++
			return response(200, `{"ok":true,"result":{"message_id":7}}`), nil
		})
		wait := func(ctx context.Context, _ time.Duration) error { cancel(); <-ctx.Done(); return ctx.Err() }
		result, err := sendReplyWithOptions(ctx, client, 22, "A\nB", nil, nil, deliveryOptions{wait: wait})
		if !errors.Is(err, context.Canceled) || attempts != 1 || result.Outcome != "cancelled" {
			t.Fatalf("attempts=%d result=%+v err=%v", attempts, result, err)
		}
	})
	t.Run("429 wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		client := testClient(t, func(*http.Request) (*http.Response, error) {
			attempts++
			return response(429, `{"ok":false,"error_code":429,"parameters":{"retry_after":2}}`), nil
		})
		wait := func(ctx context.Context, _ time.Duration) error { cancel(); <-ctx.Done(); return ctx.Err() }
		result, err := sendReplyWithOptions(ctx, client, 22, "A", nil, nil, deliveryOptions{wait: wait})
		if !errors.Is(err, context.Canceled) || attempts != 1 || result.Outcome != "cancelled" {
			t.Fatalf("attempts=%d result=%+v err=%v", attempts, result, err)
		}
	})
	t.Run("HTTP in flight", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		client := testClient(t, func(req *http.Request) (*http.Response, error) {
			cancel()
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
		result, err := sendReplyWithOptions(ctx, client, 22, "A", nil, nil, deliveryOptions{wait: noWait})
		if !errors.Is(err, context.Canceled) || result.Outcome != "unknown" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
}

func TestDeliveryDoesNotWaitPastSharedDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	attempts := 0
	client := testClient(t, func(*http.Request) (*http.Response, error) {
		attempts++
		return response(429, `{"ok":false,"error_code":429,"parameters":{"retry_after":3600}}`), nil
	})
	result, err := sendReply(ctx, client, 22, "A", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || attempts != 1 || result.Outcome != "cancelled" {
		t.Fatalf("attempts=%d result=%+v err=%v", attempts, result, err)
	}
}

func TestDeliveryConfirmsBeforeRecorderFailureWithoutDuplicate(t *testing.T) {
	attempts := 0
	client := testClient(t, func(*http.Request) (*http.Response, error) {
		attempts++
		return response(200, `{"ok":true,"result":{"message_id":9}}`), nil
	})
	recorderErr := errors.New("database unavailable")
	var gotPrefix []string
	recorder := recorderStub{confirm: func(_ int, prefix []string, _ int) error {
		gotPrefix = append([]string(nil), prefix...)
		return recorderErr
	}}
	result, err := sendReplyWithOptions(context.Background(), client, 22, "A\nB", recorder, nil, deliveryOptions{wait: noWait})
	if !errors.Is(err, recorderErr) || attempts != 1 || !reflect.DeepEqual(result.Confirmed, []string{"A"}) || !reflect.DeepEqual(gotPrefix, []string{"A"}) || result.Outcome != "partial" {
		t.Fatalf("attempts=%d prefix=%v result=%+v err=%v", attempts, gotPrefix, result, err)
	}
}

func TestDeliveryFinalACKRecorderFailureRemainsSent(t *testing.T) {
	client := testClient(t, func(*http.Request) (*http.Response, error) {
		return response(200, `{"ok":true,"result":{"message_id":9}}`), nil
	})
	recorderErr := errors.New("database unavailable")
	recorder := recorderStub{confirm: func(_ int, _ []string, _ int) error { return recorderErr }}
	result, err := sendReplyWithOptions(context.Background(), client, 22, "A", recorder, nil, deliveryOptions{wait: noWait})
	if !errors.Is(err, recorderErr) || result.Outcome != "sent" || !reflect.DeepEqual(result.Confirmed, []string{"A"}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestDeliveryCallsBeforeAttemptAndObserver(t *testing.T) {
	client := testClient(t, func(*http.Request) (*http.Response, error) {
		return response(200, `{"ok":true,"result":{"message_id":9}}`), nil
	})
	var mu sync.Mutex
	var before []int
	var observed []int
	recorder := recorderStub{before: func(sequence int) error { mu.Lock(); defer mu.Unlock(); before = append(before, sequence); return nil }}
	observe := func(sequence, total, attempt int, delay time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, sequence, total, attempt, int(delay/time.Second))
	}
	_, err := sendReplyWithOptions(context.Background(), client, 22, "A\nB", recorder, observe, deliveryOptions{wait: noWait})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, []int{1, 2}) {
		t.Fatalf("before=%v", before)
	}
	if len(observed) == 0 || observed[0] != 1 {
		t.Fatalf("observer=%v, want one-based sequence", observed)
	}
}

func TestDeliveryRateLimitWithoutRetryAfterWaitsTwoSeconds(t *testing.T) {
	attempts := 0
	client := testClient(t, func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return response(429, `{"ok":false,"error_code":429}`), nil
		}
		return response(200, `{"ok":true,"result":{"message_id":9}}`), nil
	})
	var delays []time.Duration
	wait := func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil }
	result, err := sendReplyWithOptions(context.Background(), client, 22, "A", nil, nil, deliveryOptions{wait: wait})
	if err != nil || result.Outcome != "sent" || !reflect.DeepEqual(delays, []time.Duration{2 * time.Second}) {
		t.Fatalf("delays=%v result=%+v err=%v", delays, result, err)
	}
}

func TestDeliveryRejectsEmptyReply(t *testing.T) {
	client := testClient(t, func(*http.Request) (*http.Response, error) { t.Fatal("HTTP must not be called"); return nil, nil })
	result, err := sendReply(context.Background(), client, 22, " \n ", nil, nil)
	if err == nil || result.Outcome != "failed" || result.Total != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
