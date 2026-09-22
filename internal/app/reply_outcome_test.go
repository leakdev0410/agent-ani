package app

import (
	"strings"
	"testing"
)

func TestReplyOutcomeRetainsFailureAndShowsQueuedWork(t *testing.T) {
	sched, _, _ := newTestScheduler(t)
	s := &botStatus{}
	ctx := s.StartChat(42, "working")
	s.SetOutcome(replyOutcome{Kind: "partial", Confirmed: 1, Total: 3})
	s.DoneContext(ctx)
	if got := describeStatus(s, 2, sched); !strings.Contains(got, "đang chờ xử lý 2 tin") || !strings.Contains(got, "1/3") || strings.Contains(got, "Đang rảnh") {
		t.Fatalf("status=%s", got)
	}
	ctx = s.StartChat(42, "next")
	s.SetOutcome(replyOutcome{Kind: "sent", Confirmed: 1, Total: 1})
	s.DoneContext(ctx)
	if got := describeStatus(s, 0, sched); strings.Contains(got, "Lượt trước") {
		t.Fatalf("stale outcome: %s", got)
	}
}

func TestReplyOutcomeOldCompletionCannotClearNewTurn(t *testing.T) {
	s := &botStatus{}
	old := s.StartChat(42, "old")
	current := s.StartChat(42, "current")
	s.DoneContext(old)
	if busy, action, _ := s.Snapshot(); !busy || action != "current" {
		t.Fatalf("old callback overwrote current: %v %s", busy, action)
	}
	s.SetOutcomeContext(old, replyOutcome{Kind: "failed"})
	if s.LastOutcome().Kind != "" {
		t.Fatal("old outcome leaked into current turn")
	}
	s.DoneContext(current)
}

func TestReplyOutcomeHistoryOnlyConfirmedAndResetRejectsLateAppend(t *testing.T) {
	h := newHistoryStore()
	_, generation := h.SnapshotTurn(42)
	if !h.AppendDelivered(42, generation, "first user", "", false) {
		t.Fatal("user not stored")
	}
	got, generation := h.SnapshotTurn(42)
	if len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("empty assistant recorded: %v", got)
	}
	got[0].Content = "mutated snapshot"
	if h.Get(42)[0].Content != "first user" {
		t.Fatal("snapshot aliases history")
	}
	h.AppendDelivered(42, generation, "second user", "confirmed prefix", false)
	got = h.Get(42)
	if len(got) != 3 || got[2].Content != "confirmed prefix" {
		t.Fatalf("history=%v", got)
	}
	h.Delete(42)
	if h.AppendDelivered(42, generation, "late user", "late reply", false) || len(h.Get(42)) != 0 {
		t.Fatal("late callback resurrected reset history")
	}
}
