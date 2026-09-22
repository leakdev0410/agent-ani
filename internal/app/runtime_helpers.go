package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"ani-telegram/internal/memdb"
	"ani-telegram/internal/telegram"
)

type botStatus struct {
	mu             sync.Mutex
	busy           bool
	action         string
	since          time.Time
	cancel         context.CancelFunc
	currentChatID  int64
	currentContext context.Context
	lastOutcome    replyOutcome
	deliveries     *deliveryCoordinator
}

func (s *botStatus) Start(action string) context.Context { return s.StartChat(0, action) }

func (s *botStatus) StartChat(chatID int64, action string) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.busy, s.action, s.since, s.cancel, s.currentChatID = true, action, time.Now(), cancel, chatID
	s.currentContext = ctx
	return ctx
}

func (s *botStatus) SetAction(action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		s.action = action
	}
}

func (s *botStatus) Done() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	s.busy, s.action, s.cancel, s.currentChatID = false, "", nil, 0
	s.currentContext = nil
}

func (s *botStatus) CancelChat(chatID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.busy || s.cancel == nil || (s.currentChatID != 0 && s.currentChatID != chatID) {
		return false
	}
	s.cancel()
	return true
}

func (s *botStatus) Snapshot() (bool, string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy, s.action, s.since
}

func (s *botStatus) Cancel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.busy || s.cancel == nil {
		return false
	}
	s.cancel()
	return true
}

func describeStatus(status *botStatus, queueLen int, sched *proactiveScheduler) string {
	busy, action, since := status.Snapshot()
	queue := ""
	if queueLen > 0 {
		queue = fmt.Sprintf(" (%d job đang chờ)", queueLen)
	}
	head := "🟢 Đang rảnh." + queue
	if queueLen > 0 {
		head = fmt.Sprintf("🟡 đang chờ xử lý %d tin.", queueLen)
	}
	if busy {
		head = fmt.Sprintf("🟡 Đang bận: %s (%s trước).%s", action, time.Since(since).Round(time.Second), queue)
	}
	if outcome := describeReplyOutcome(status.LastOutcome()); outcome != "" {
		head += "\n" + outcome
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return head + "\n" + describeProactiveLine(sched.Snapshot()) +
		fmt.Sprintf(" · 🧠 heap %.1f MiB, %d goroutine", float64(stats.HeapAlloc)/(1024*1024), runtime.NumGoroutine())
}

// describeStatusWithMemoryJobs adds journal-only operational metadata. It
// intentionally exposes counts and age, never payloads, extraction JSON, or
// model output.
func describeStatusWithMemoryJobs(status *botStatus, queueLen int, sched *proactiveScheduler, db *memdb.DB) string {
	base := describeStatus(status, queueLen, sched)
	if db == nil {
		return base
	}
	pending, ready, _, err := db.MemoryJobCounts()
	if err != nil {
		return base + "\n🧾 Memory journal: không đọc được trạng thái."
	}
	line := fmt.Sprintf("🧾 Memory journal: pending %d, ready %d", pending, ready)
	if createdAt, ok, err := db.OldestMemoryJobCreatedAt(); err == nil && ok {
		age := time.Since(createdAt).Round(time.Second)
		if age < 0 {
			age = 0
		}
		line += fmt.Sprintf(", job cũ nhất %s", age)
	}
	embeddingLine := "🔎 Semantic index: không đọc được trạng thái."
	if queued, indexed, err := db.EmbeddingIndexCounts(); err == nil {
		embeddingLine = fmt.Sprintf("🔎 Semantic index: queued %d, indexed %d", queued, indexed)
		if createdAt, ok, err := db.OldestEmbeddingQueueCreatedAt(); err == nil && ok {
			age := time.Since(createdAt).Round(time.Second)
			if age < 0 {
				age = 0
			}
			embeddingLine += fmt.Sprintf(", queue cũ nhất %s", age)
		}
	}
	return base + "\n" + line + "\n" + embeddingLine
}

func startDebugServer(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("debug address phải có dạng host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("debug server chỉ được bind loopback, got %q", host)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/runtime", func(w http.ResponseWriter, _ *http.Request) {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"heap_bytes": stats.HeapAlloc, "goroutines": runtime.NumGoroutine()})
	})
	go func() { _ = http.Serve(listener, mux) }()
	return nil
}

func sendSplitByLines(tg *telegram.Client, chatID int64, reply string) error {
	return sendSplitByLinesRecorded(tg, chatID, reply, nil)
}

func sendSplitByLinesRecorded(tg *telegram.Client, chatID int64, reply string, record func(sequence int, text string, sent telegram.Message) error) error {
	var chunks []string
	for _, line := range strings.Split(reply, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			chunks = append(chunks, splitTelegramText(line, 4096)...)
		}
	}
	for sequence, line := range chunks {
		if sequence > 0 {
			time.Sleep(lineDelay)
		}
		sent, err := tg.SendMessageResult(chatID, line)
		if err != nil {
			return err
		}
		if record != nil {
			if err := record(sequence, line, sent); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitTelegramText(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" || limit <= 0 {
		return nil
	}
	var out []string
	for len(text) > 0 {
		if utf16Units(text) <= limit {
			out = append(out, text)
			break
		}
		cut, units, best := 0, 0, -1
		for index, r := range text {
			width := 1
			if r > 0xFFFF {
				width = 2
			}
			if units+width > limit {
				break
			}
			units += width
			cut = index + len(string(r))
			if r == '\n' || r == ' ' || r == '\t' || r == '.' || r == '!' || r == '?' {
				best = cut
			}
		}
		if best > 0 {
			cut = best
		}
		if cut == 0 {
			return nil
		}
		out = append(out, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	return out
}

func utf16Units(text string) int {
	total := 0
	for _, r := range text {
		if r > 0xFFFF {
			total += 2
		} else {
			total++
		}
	}
	return total
}
