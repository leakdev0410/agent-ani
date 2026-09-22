package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"ani-telegram/internal/openrouter"
	"ani-telegram/internal/telegram"
)

func TestDeliveryIntegrationValidatesOutputAndCommitsOnlyACK(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		texts, reasons            []string
		rejectAt                  int
		wantCalls                 int
		wantSent, kind, assistant string
	}{
		{"empty recovers", []string{"", "complete"}, []string{"stop", "stop"}, 0, 2, "complete", "sent", "complete"},
		{"three empty sends notice", []string{"", "", ""}, nil, 0, 3, "Em chưa tạo được câu trả lời đầy đủ cho tin vừa rồi. Anh thử lại giúp em nhé.", "model_failed", "Em chưa tạo được câu trả lời đầy đủ cho tin vừa rồi. Anh thử lại giúp em nhé."},
		{"truncation recovers", []string{"cut off", "complete"}, []string{"length", "stop"}, 0, 2, "complete", "sent", "complete"},
		{"three truncated sends notice", []string{"cut", "cut", "cut"}, []string{"length", "length", "length"}, 0, 3, "Em chưa tạo được câu trả lời đầy đủ cho tin vừa rồi. Anh thử lại giúp em nhé.", "model_failed", "Em chưa tạo được câu trả lời đầy đủ cho tin vừa rồi. Anh thử lại giúp em nhé."},
		{"first rejected", []string{"draft never received"}, nil, 1, 1, "", "failed", ""},
		{"second rejected", []string{"A\nB\nC"}, nil, 2, 1, "A", "partial", "A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newJournalTestDB(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				idx := calls
				calls++
				if idx >= len(tc.texts) {
					t.Error("unexpected extra model call")
					idx = len(tc.texts) - 1
				}
				reason := "stop"
				if idx < len(tc.reasons) {
					reason = tc.reasons[idx]
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": reason, "message": map[string]any{"role": "assistant", "content": tc.texts[idx]}}}})
			}))
			defer server.Close()
			var sent []string
			attempts := 0
			original := http.DefaultTransport
			http.DefaultTransport = journalRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "api.telegram.org" {
					return original.RoundTrip(req)
				}
				attempts++
				job, ok, err := db.NextMemoryJob()
				if err != nil || !ok || !job.DeliveryPending {
					t.Errorf("send before held journal: ok=%v err=%v", ok, err)
				}
				var p memoryJournalPayload
				if err := json.Unmarshal(job.Payload, &p); err != nil {
					t.Error(err)
				}
				if attempts == 1 && (len(p.History) != 1 || p.History[0].Role != "user") {
					t.Errorf("draft journaled before ACK: %v", p.History)
				}
				body := `{"ok":true,"result":{"message_id":10}}`
				code := 200
				if attempts == tc.rejectAt {
					body = `{"ok":false,"error_code":403,"description":"private provider description"}`
					code = 403
				} else {
					var request struct {
						Text string `json:"text"`
					}
					if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					sent = append(sent, request.Text)
				}
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			h := newHistoryStore()
			s := &botStatus{}
			ctx := s.StartChat(42, "test")
			processMessage(ctx, s, db, openrouter.NewClient("test", server.URL, "model"), telegram.NewClient("test-token"), newPromptStore("system"), h, nil, "", 42, "actual user", false, nil, nil)
			if calls != tc.wantCalls || strings.Join(sent, "\n") != tc.wantSent {
				t.Fatalf("calls=%d sent=%q", calls, sent)
			}
			got := h.Get(42)
			wantLen := 1
			if tc.assistant != "" {
				wantLen = 2
			}
			if len(got) != wantLen || got[0].Content != "actual user" || (wantLen == 2 && got[1].Content != tc.assistant) {
				t.Fatalf("history=%+v", got)
			}
			o := s.LastOutcome()
			if o.Kind != tc.kind || o.Confirmed != len(sent) {
				t.Fatalf("outcome=%+v", o)
			}
			if busy, _, _ := s.Snapshot(); busy {
				t.Fatal("turn left busy")
			}
			job, ok, err := db.NextMemoryJob()
			if err != nil || !ok || job.DeliveryPending {
				t.Fatalf("journal not finalized: %+v %v %v", job, ok, err)
			}
			var payload memoryJournalPayload
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.History) != wantLen || (wantLen == 2 && payload.History[1].Content != tc.assistant) {
				t.Fatalf("journal=%+v", payload)
			}
			if payload.SkipProactivePlan != (tc.kind != "sent") {
				t.Fatalf("skip schedule=%v", payload.SkipProactivePlan)
			}
		})
	}
}

func TestDeliveryIntegrationResetWhileModelRunningSendsNoLateNotice(t *testing.T) {
	db := newJournalTestDB(t)
	h := newHistoryStore()
	s := &botStatus{}
	ctx := s.StartChat(42, "test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.CancelChat(42)
		h.Delete(42)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": ""}}}})
	}))
	defer server.Close()
	original := http.DefaultTransport
	http.DefaultTransport = journalRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.telegram.org" {
			return original.RoundTrip(req)
		}
		t.Error("late notice sent after reset")
		return nil, context.Canceled
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	processMessage(ctx, s, db, openrouter.NewClient("test", server.URL, "model"), telegram.NewClient("test-token"), newPromptStore("system"), h, nil, "", 42, "u", false, nil, nil)
	if len(h.Get(42)) != 0 {
		t.Fatal("reset history resurrected")
	}
}

func installTurnTransport(t *testing.T, send func(*http.Request) (*http.Response, error)) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = journalRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.telegram.org" {
			return original.RoundTrip(req)
		}
		return send(req)
	})
	t.Cleanup(func() { http.DefaultTransport = original })
}

func turnACK(req *http.Request) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`)), Request: req}
}

func TestDeliveryIntegrationCancellationStopsSuffixAndNewKeepsHistoryEmpty(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "restart", true: "new"}[reset], func(t *testing.T) {
			db := newJournalTestDB(t)
			h := newHistoryStore()
			s := &botStatus{}
			ctx := s.StartChat(42, "test")
			snapshot, generation := h.SnapshotTurn(42)
			sent := 0
			installTurnTransport(t, func(req *http.Request) (*http.Response, error) {
				sent++
				s.CancelChat(42)
				if reset {
					h.Delete(42)
				}
				return turnACK(req), nil
			})
			finishForegroundReply(ctx, s, db, telegram.NewClient("test-token"), h, nil, 42, snapshot, generation, "real user", "A\nB\nC", 0, false, "", nil)
			s.DoneContext(ctx)
			if sent != 1 || s.LastOutcome().Confirmed != 1 {
				t.Fatalf("suffix sent or ACK lost: sends=%d outcome=%+v", sent, s.LastOutcome())
			}
			got := h.Get(42)
			if reset && len(got) != 0 {
				t.Fatalf("new resurrected history: %v", got)
			}
			if !reset && (len(got) != 2 || got[1].Content != "A") {
				t.Fatalf("restart lost known ACK: %v", got)
			}
			job, _, _ := db.NextMemoryJob()
			var payload memoryJournalPayload
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if job.DeliveryPending || len(payload.History) != 2 || payload.History[1].Content != "A" || !payload.SkipProactivePlan {
				t.Fatalf("cancel journal=%+v", payload)
			}
		})
	}
}

func TestDeliveryIntegrationFinalizeFailureReleasesHeldOwnerForRuntimeRecovery(t *testing.T) {
	db := newJournalTestDB(t)
	h := newHistoryStore()
	s := &botStatus{}
	clear := db.SetMemoryJobCommitErrorForTest(func(*sql.Tx) error { return errors.New("injected finalize failure") })
	defer clear()
	sent := 0
	installTurnTransport(t, func(req *http.Request) (*http.Response, error) { sent++; return turnACK(req), nil })
	ctx := s.StartChat(42, "test")
	snapshot, generation := h.SnapshotTurn(42)
	finishForegroundReply(ctx, s, db, telegram.NewClient("test-token"), h, nil, 42, snapshot, generation, "u", "confirmed", 0, false, "", nil)
	s.DoneContext(ctx)
	job, ok, err := db.NextMemoryJob()
	if err != nil || !ok || !job.DeliveryPending {
		t.Fatalf("expected held after finalize fault: %+v %v %v", job, ok, err)
	}
	if s.deliveryCoordinator().Active(job.ID) {
		t.Fatal("finished sender kept held ownership")
	}
	if sent != 1 || s.LastOutcome().Kind != "sent" || !s.LastOutcome().MemoryWarning {
		t.Fatalf("DB failure misreported/resend: sends=%d outcome=%+v", sent, s.LastOutcome())
	}
	clear()
	p := memoryJournalProcessor{db: db, deliveryActive: s.deliveryCoordinator().Active}
	if worked, err := p.ProcessNext(context.Background()); err != nil || !worked {
		t.Fatalf("runtime recovery=%v %v", worked, err)
	}
	job, _, _ = db.NextMemoryJob()
	var payload memoryJournalPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if job.DeliveryPending || payload.History[len(payload.History)-1].Content != "confirmed" || !payload.SkipProactivePlan || sent != 1 {
		t.Fatalf("recovery replayed/lost prefix: %+v sends=%d", payload, sent)
	}
}

func TestDeliveryIntegrationACKWriteFailureNeverResendsAndRemembersACKInRAM(t *testing.T) {
	db := newJournalTestDB(t)
	h := newHistoryStore()
	s := &botStatus{}
	sent := 0
	installTurnTransport(t, func(req *http.Request) (*http.Response, error) { sent++; _ = db.Close(); return turnACK(req), nil })
	ctx := s.StartChat(42, "test")
	snapshot, generation := h.SnapshotTurn(42)
	finishForegroundReply(ctx, s, db, telegram.NewClient("test-token"), h, nil, 42, snapshot, generation, "u", "received", 0, false, "", nil)
	s.DoneContext(ctx)
	o := s.LastOutcome()
	got := h.Get(42)
	if sent != 1 || o.Kind != "sent" || o.Confirmed != 1 || !o.MemoryWarning || len(got) != 2 || got[1].Content != "received" {
		t.Fatalf("ACK failure replay/loss: sends=%d outcome=%+v history=%v", sent, o, got)
	}
	if s.deliveryCoordinator().Active(1) {
		t.Fatal("closed DB left held ownership active")
	}
}

func TestDeliveryIntegration429RetriesOnlyPendingChunkAndFinalizesFullPrefix(t *testing.T) {
	db := newJournalTestDB(t)
	h := newHistoryStore()
	s := &botStatus{}
	var attempted []string
	installTurnTransport(t, func(req *http.Request) (*http.Response, error) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		attempted = append(attempted, body.Text)
		if len(attempted) == 2 {
			return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`)), Request: req}, nil
		}
		return turnACK(req), nil
	})
	ctx := s.StartChat(42, "test")
	snapshot, generation := h.SnapshotTurn(42)
	finishForegroundReply(ctx, s, db, telegram.NewClient("test-token"), h, nil, 42, snapshot, generation, "u", "A\nB\nC", 0, false, "", nil)
	s.DoneContext(ctx)
	if !reflect.DeepEqual(attempted, []string{"A", "B", "B", "C"}) {
		t.Fatalf("attempt order=%v", attempted)
	}
	job, _, _ := db.NextMemoryJob()
	var payload memoryJournalPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if job.DeliveryPending || job.DeliverySequence != 3 || payload.History[1].Content != "A\nB\nC" || payload.SkipProactivePlan || s.LastOutcome().Kind != "sent" {
		t.Fatalf("retry lost/duplicated prefix: job=%+v payload=%+v outcome=%+v", job, payload, s.LastOutcome())
	}
}

func TestDeliveryIntegrationUnknownNeverSendsNoticeOrUnconfirmedMemory(t *testing.T) {
	db := newJournalTestDB(t)
	h := newHistoryStore()
	s := &botStatus{}
	sent := 0
	installTurnTransport(t, func(req *http.Request) (*http.Response, error) {
		sent++
		_, _ = io.Copy(io.Discard, req.Body)
		return nil, io.ErrUnexpectedEOF
	})
	ctx := s.StartChat(42, "test")
	snapshot, generation := h.SnapshotTurn(42)
	finishForegroundReply(ctx, s, db, telegram.NewClient("test-token"), h, nil, 42, snapshot, generation, "u", "unknown delivery\nsuffix", 0, false, "", nil)
	s.DoneContext(ctx)
	job, _, _ := db.NextMemoryJob()
	var payload memoryJournalPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if sent != 1 || s.LastOutcome().Kind != "unknown" || len(h.Get(42)) != 1 || len(payload.History) != 1 || !payload.SkipProactivePlan || payload.DeliveryOutcome != "unknown" {
		t.Fatalf("unknown replay/leak: sends=%d history=%v payload=%+v outcome=%+v", sent, h.Get(42), payload, s.LastOutcome())
	}
}
