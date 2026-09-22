// ani-telegram is a single-owner Telegram bot with SQLite-backed durable memory
// (core state, diary, observations, topics) và auto-recall qua tool-calling.
//
// Package app chứa toàn bộ orchestration: flag/env parsing, DB seeding, scheduler,
// worker pool, và main loop. Entry point mỏng nằm ở cmd/ani-telegram/.
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"ani-telegram/internal/config"
	"ani-telegram/internal/memcore"
	"ani-telegram/internal/memdb"
	"ani-telegram/internal/openrouter"
	"ani-telegram/internal/persona"
	"ani-telegram/internal/safelog"
	"ani-telegram/internal/telegram"
)

const (
	lineDelay        = 500 * time.Millisecond
	newCommand       = "/new"
	modelCommand     = "/model"
	providerCommand  = "/provider"
	providersCommand = "/providers"
	helpCommand      = "/help"
	statusCommand    = "/status"
	restartCommand   = "/restart"
	proactiveCommand = "/proactive"
	maxHistoryTurns  = 20 // số lượt hội thoại giữ trong RAM mỗi chat, tránh prompt phình vô hạn
)

const newCommandReply = "Ok bắt đầu lại nè, đoạn chat cũ bỏ qua nha. Em vẫn nhớ hết về anh đó, đừng lo."

const statusActionReplying = "đang chuẩn bị phản hồi"

// userMessageStatusAction deliberately ignores text: /status is operational
// metadata and must never echo a private inbound message while it is queued.
func userMessageStatusAction(_ string) string { return statusActionReplying }

const helpText = `Các lệnh anh dùng được:

/new — xoá lịch sử hội thoại đoạn chat này (đỡ tốn token), vẫn giữ nguyên memory.

/model — xem model OpenRouter đang dùng.
/model <tên model> — đổi model (VD /model anthropic/claude-sonnet-4.5), không cần restart.

/providers — liệt kê nhà cung cấp đang host model hiện tại kèm giá USD/1M token.
/provider — xem đang ghim nhà cung cấp nào.
/provider <tag> — ghim đúng 1 nhà cung cấp cho model hiện tại (lấy tag từ /providers).
/provider nodata — chỉ dùng nhà cung cấp KHÔNG lưu trữ data.
/provider off — bỏ ghim, quay lại tự động chọn nhà cung cấp.

/proactive — xem kế hoạch chủ động nhắn: ⏰ mốc chờ anh rep + 📌 các cuộc hẹn em tự đặt (em tự
quyết lại mỗi lượt chat, lượt nào cũng có ít nhất 1 mốc).
/proactive off — tắt hẳn việc em chủ động nhắn; /proactive on — bật lại.
/proactive clear — dọn hết cuộc hẹn, chỉ chừa mốc chờ anh rep.
/proactive <số phút> — ép mốc chờ rep thành N phút nữa (để thử cho nhanh).

/status — xem bot đang rảnh hay đang bận làm gì, còn mấy tin đang chờ trong hàng đợi, và lúc nào
em sẽ chủ động nhắn anh.
/restart — huỷ ngay tin đang xử lý dở, bot tự chuyển sang tin kế tiếp trong hàng đợi (nếu có).

/help — hiện lại danh sách này.

Gửi ảnh trực tiếp (kèm chú thích hoặc không) để em xem — cần model hỗ trợ vision.

Gặp lỗi tạm thời từ nhà cung cấp, em tự thử lại mỗi 60 giây tới khi thành công. Nhắn tin lúc em
đang bận cũng không mất — tin xếp hàng chờ, tự xử lý ngay khi em rảnh.`

type chatJob struct {
	chatID          int64
	userText        string
	hasPhoto        bool
	photos          []telegram.PhotoSize
	proactive       bool
	proactiveRound  int
	proactiveReason string
	proactiveKind   slotKind
	proactiveEpoch  uint64
}

// Main là entry point thật của bot. Được gọi từ cmd/ani-telegram/main.go.
func Main() {
	log.SetOutput(safelog.NewWriter(os.Stderr))
	verbose := flag.Bool("log", false, "In operational metadata và lỗi đã redact")
	logDB := flag.Bool("logdb", false, "In log mỗi lần đọc/ghi memory DB, độc lập với -log")
	debugAddr := flag.String("debug-addr", "", "địa chỉ loopback cho endpoint debug runtime, rỗng = tắt")
	backfillTopics := flag.Bool("backfill-topics", false, "Gán chủ đề cho diary cũ chưa tag bằng model (chạy 1 lần rồi thoát)")
	flag.Parse()
	if !*verbose {
		log.SetOutput(io.Discard)
	}

	cfg, err := config.Load()
	if err != nil {
		fatal("lỗi cấu hình: %v", err)
	}
	if *debugAddr != "" {
		if err := startDebugServer(*debugAddr); err != nil {
			fatal("lỗi debug server: %v", err)
		}
	}

	db, err := memdb.Open(cfg.MemoryDBPath)
	if err != nil {
		fatal("lỗi mở memory DB tại %s: %v", cfg.MemoryDBPath, err)
	}
	defer db.Close()
	db.SetVerbose(*logDB)

	seeded, err := db.IsSeeded()
	if err != nil {
		fatal("lỗi kiểm tra memory DB: %v", err)
	}
	if !seeded {
		count, err := db.SeedFromDir(cfg.MemoryDir)
		if err != nil {
			fatal("lỗi nạp memory cũ (%s) vào DB lần đầu: %v", cfg.MemoryDir, err)
		}
		log.Printf("lần đầu chạy: đã nạp %d file memory cũ từ %s vào %s", count, cfg.MemoryDir, cfg.MemoryDBPath)
	}
	hasSkills, err := db.HasSkills()
	if err != nil {
		fatal("lỗi kiểm tra skills trong DB: %v", err)
	}
	if !hasSkills {
		count, err := db.SeedSkillsFromDir(cfg.MemoryDir)
		if err != nil {
			fatal("lỗi nạp skills cũ (%s) vào DB: %v", cfg.MemoryDir, err)
		}
		if count > 0 {
			log.Printf("đã nạp %d skill từ %s vào %s", count, cfg.MemoryDir, cfg.MemoryDBPath)
		}
	}
	if migrated, err := memcore.MigrateLegacyCoreMemory(db); err != nil {
		fatal("lỗi migrate core memory cũ: %v", err)
	} else if migrated {
		log.Printf("đã migrate core memory blob cũ sang core_state/diary_entries/observations")
	}
	if compacted, err := memcore.CompactStoredCoreState(db); err != nil {
		fatal("lỗi compact mang core state: %v", err)
	} else if compacted {
		log.Printf("đã compact mang1-3 cũ theo giới hạn prompt")
	}
	if err := ensureSearchIndex(db, cfg.OpenRouterEmbeddingModel); err != nil {
		fatal("lỗi dựng chỉ mục tìm kiếm memory: %v", err)
	}

	if *backfillTopics {
		llm := openrouter.NewClient(cfg.OpenRouterAPIKey, cfg.OpenRouterBaseURL, cfg.OpenRouterModel, cfg.Limits)
		if err := runBackfillTopics(db, llm); err != nil {
			fatal("lỗi backfill topic: %v", err)
		}
		return
	}

	prompt := newPromptStore("")
	reloaded, err := persona.BuildSystemPrompt(db, cfg.PersonaPath)
	if err != nil {
		fatal("lỗi nạp persona/memory: %v", err)
	}
	prompt.Set(reloaded)
	log.Printf("đã nạp persona + memory từ %s (%d ký tự)", cfg.MemoryDBPath, len(reloaded))

	tg := telegram.NewClient(cfg.BotToken, cfg.Limits)
	llm := openrouter.NewClient(cfg.OpenRouterAPIKey, cfg.OpenRouterBaseURL, cfg.OpenRouterModel, cfg.Limits)
	loadRuntimeModelSettings(db, llm)

	status := &botStatus{}
	history := newHistoryStore()
	jobs := make(chan chatJob, 100)
	memoryWake := make(chan struct{}, 1)
	embeddingWake := make(chan struct{}, 1)
	semantic := semanticSearchRuntime{embedder: llm, model: cfg.OpenRouterEmbeddingModel}
	deliveries := status.deliveryCoordinator()
	if err := recoverHeldMemoryJobs(db); err != nil {
		fatal("lỗi phục hồi delivery journal: %v", err)
	}
	sched := newProactiveScheduler(jobs, db, cfg.AllowedChatID)
	sched.Restore()
	go memoryJournalWorkerWithDelivery(context.Background(), db, llm, prompt, sched, cfg.PersonaPath, memoryWake, deliveries, embeddingWake)
	if startOptionalEmbeddingIndexer(context.Background(), db, llm, cfg.OpenRouterEmbeddingModel, embeddingWake) {
		log.Printf("semantic memory indexing enabled")
	}
	go messageWorker(jobs, status, db, llm, tg, prompt, history, sched, cfg.PersonaPath, memoryWake, semantic)
	go idleSessionWorker(db, prompt, history, cfg.PersonaPath)

	offset := loadOffset(cfg.OffsetFile)
	fmt.Println("Ani đang chạy...")
	log.Printf("ani-telegram chạy, memory_db=%s model=%s provider=%s allowed_user_id=%d",
		cfg.MemoryDBPath, llm.Model(), describeCurrentProvider(llm), cfg.AllowedUserID)
	policy := telegramAccessPolicy{allowedUserID: cfg.AllowedUserID, allowedChatID: cfg.AllowedChatID}

	for {
		updates, err := tg.GetUpdates(offset, cfg.PollTimeoutSec)
		if err != nil {
			log.Printf("lỗi getUpdates: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, update := range updates {
			offset = update.UpdateID + 1
			saveOffset(cfg.OffsetFile, offset)

			msg, eventKind, authorization := authorizedMessageFromUpdate(update, policy)
			if !authorization.allowed {
				log.Printf("[update %d] telegram update bị chặn: reason=%s", update.UpdateID, authorization.reason)
				continue
			}
			hasPhoto := len(msg.Photo) > 0
			if strings.TrimSpace(msg.Text) == "" && !hasPhoto {
				log.Printf("[update %d] bỏ qua update không có text/photo", update.UpdateID)
				continue
			}
			chatID := msg.Chat.ID
			userText := msg.Text
			if hasPhoto {
				userText = msg.Caption
			}
			logInboundMessage(int64(update.UpdateID), chatID, eventKind, userText, len(msg.Photo))

			if handleImmediateCommand(db, llm, tg, status, sched, jobs, prompt, history, cfg.PersonaPath, chatID, userText) {
				continue
			}

			sched.NoteUserMessage(chatID)
			history.NoteUserMessage(chatID, time.Now())
			jobs <- chatJob{chatID: chatID, userText: userText, hasPhoto: hasPhoto, photos: msg.Photo}
		}
	}
}

func handleImmediateCommand(db *memdb.DB, llm *openrouter.Client, tg *telegram.Client, status *botStatus, sched *proactiveScheduler, jobs chan chatJob, prompt *promptStore, history *historyStore, personaPath string, chatID int64, text string) bool {
	switch {
	case matchesCommand(text, helpCommand):
		_ = tg.SendMessage(chatID, helpText)
		return true
	case isNewCommand(text):
		status.CancelChat(chatID)
		history.Delete(chatID)
		if reloaded, err := persona.BuildSystemPrompt(db, personaPath); err != nil {
			log.Printf("[chat %d] lỗi nạp lại persona/memory: %v", chatID, err)
		} else {
			prompt.Set(reloaded)
		}
		_ = tg.SendMessage(chatID, newCommandReply)
		return true
	case matchesCommand(text, providersCommand):
		_ = tg.SendMessage(chatID, listProviders(llm, llm.Model()))
		return true
	case matchesCommand(text, statusCommand):
		_ = tg.SendMessage(chatID, describeStatusWithMemoryJobs(status, len(jobs), sched, db))
		return true
	case matchesCommand(text, restartCommand):
		if status.CancelChat(chatID) {
			_ = tg.SendMessage(chatID, "Đã hủy tin đang xử lý dở, chuyển sang tin kế tiếp trong hàng đợi (nếu có) rồi đó anh.")
		} else {
			_ = tg.SendMessage(chatID, "Em đang rảnh mà, không có gì để huỷ đâu anh.")
		}
		return true
	}

	if arg, matched := parseArgCommand(text, modelCommand); matched {
		if arg == "" {
			_ = tg.SendMessage(chatID, "Đang dùng model: "+llm.Model()+"\nNhà cung cấp: "+describeCurrentProvider(llm))
			return true
		}
		llm.SetModel(arg)
		llm.SetProvider("")
		if err := db.UpdateSettings(map[string]string{"current_model": arg, "current_provider": ""}); err != nil {
			log.Printf("/model: lỗi persist settings: %v", err)
		}
		_ = tg.SendMessage(chatID, "Đã đổi model sang: "+arg+"\n"+describeModelCapabilities(llm, arg))
		return true
	}
	if arg, matched := parseArgCommand(text, providerCommand); matched {
		_ = tg.SendMessage(chatID, handleProviderCommand(db, llm, arg))
		return true
	}
	if arg, matched := parseArgCommand(text, proactiveCommand); matched {
		_ = tg.SendMessage(chatID, handleProactiveCommand(sched, chatID, arg))
		return true
	}
	return false
}

func messageWorker(jobs <-chan chatJob, status *botStatus, db *memdb.DB, llm *openrouter.Client, tg *telegram.Client, prompt *promptStore, history *historyStore, sched *proactiveScheduler, personaPath string, memoryWake chan<- struct{}, semantic semanticSearchRuntime) {
	for job := range jobs {
		if !sched.AllowsChat(job.chatID) {
			log.Printf("bỏ queued job: reason=wrong_chat")
			continue
		}
		if job.proactive {
			if job.proactiveEpoch != sched.Epoch() {
				log.Printf("[chat %d] proactive: bỏ lượt chủ động nhắn lần %d (anh vừa nhắn tin xen vào)",
					job.chatID, job.proactiveRound)
				continue
			}
			ctx := status.StartChat(job.chatID, fmt.Sprintf("đang chủ động nhắn anh trước (lần %d)", job.proactiveRound))
			processProactive(ctx, status, db, llm, tg, prompt, history, sched, personaPath, job.chatID,
				job.proactiveRound, job.proactiveKind, job.proactiveReason, memoryWake, semantic)
			continue
		}
		ctx := status.StartChat(job.chatID, userMessageStatusAction(job.userText))
		processMessage(ctx, status, db, llm, tg, prompt, history, sched, personaPath, job.chatID, job.userText, job.hasPhoto, job.photos, memoryWake, semantic)
	}
}

func processMessage(ctx context.Context, status *botStatus, db *memdb.DB, llm *openrouter.Client, tg *telegram.Client, prompt *promptStore, history *historyStore, sched *proactiveScheduler, personaPath string, chatID int64, userText string, hasPhoto bool, photos []telegram.PhotoSize, memoryWake chan<- struct{}, semantic ...semanticSearchRuntime) {
	defer status.DoneContext(ctx)
	snapshot, generation := history.SnapshotTurn(chatID)
	historyText := userText
	if hasPhoto {
		if strings.TrimSpace(historyText) == "" {
			historyText = "[đã gửi 1 tấm ảnh]"
		} else {
			historyText = "[ảnh] " + historyText
		}
	}
	var downloaded telegram.DownloadedFile
	if hasPhoto {
		var err error
		if len(photos) == 0 {
			err = fmt.Errorf("photo metadata missing")
		} else {
			var path string
			largest := photos[len(photos)-1]
			path, err = tg.GetFile(largest.FileID)
			if err == nil {
				downloaded, err = tg.DownloadFileToTemp(path, largest.FileSize)
			}
		}
		if err != nil {
			finishForegroundReply(ctx, status, db, tg, history, sched, chatID, snapshot, generation, historyText, "Em không tải được ảnh anh gửi, thử lại giúp em nha.", 0, true, "model_error", memoryWake)
			return
		}
		defer os.Remove(downloaded.Path)
	}
	onRetry := func(attempt int, err error) {
		status.SetActionContext(ctx, fmt.Sprintf("đang thử lại lần %d do lỗi nhà cung cấp", attempt))
		logReplyRetry(chatID, "model", attempt, replyErrorCode(err), 0)
	}
	reply, err := replyWithSkills(ctx, db, llm, prompt.Get(), snapshot, userText, downloaded.Path, downloaded.MIME, onRetry, semantic...)
	failed, code := false, ""
	if err != nil {
		failed, code = true, replyErrorCode(err)
		reply = "Em chưa tạo được câu trả lời đầy đủ cho tin vừa rồi. Anh thử lại giúp em nhé."
	}
	finishForegroundReply(ctx, status, db, tg, history, sched, chatID, snapshot, generation, historyText, reply, 0, failed, code, memoryWake)
}

func processProactive(ctx context.Context, status *botStatus, db *memdb.DB, llm *openrouter.Client, tg *telegram.Client, prompt *promptStore, history *historyStore, sched *proactiveScheduler, personaPath string, chatID int64, round int, kind slotKind, slotReason string, memoryWake chan<- struct{}, semantic ...semanticSearchRuntime) {
	defer status.DoneContext(ctx)
	snapshot, generation := history.SnapshotTurn(chatID)
	onRetry := func(attempt int, err error) {
		status.SetActionContext(ctx, fmt.Sprintf("đang thử lại lần %d do lỗi nhà cung cấp", attempt))
		logReplyRetry(chatID, "model", attempt, replyErrorCode(err), 0)
	}
	reply, err := replyWithSkills(ctx, db, llm, prompt.Get(), snapshot, buildProactiveTrigger(kind, slotReason, round), "", "", onRetry, semantic...)
	if err != nil {
		outcome := replyOutcome{Kind: "model_failed", ErrorCode: replyErrorCode(err)}
		if ctx.Err() != nil || !history.IsGeneration(chatID, generation) {
			outcome.Kind = "cancelled"
		} else if sched != nil {
			applyProactivePlan(sched, chatID, 0, "", nil, false, true)
		}
		status.SetOutcomeContext(ctx, outcome)
		logReplyOutcome(chatID, outcome)
		return
	}
	finishForegroundReply(ctx, status, db, tg, history, sched, chatID, snapshot, generation, "", reply, round, false, "", memoryWake)
}

// applyProactivePlan đặt kế hoạch chủ động nhắn cho lượt vừa xong. Lượt trích xuất hỏng hẳn (lỗi
// API/parse) vẫn phải ra được 1 mốc — lượt chat nào cũng bắt buộc có hẹn nhắn lại — trừ khi cả
// lượt bị /restart huỷ giữa chừng (alive=false), lúc đó anh đang chủ động can thiệp nên đừng đặt
// hẹn sau lưng.
func applyProactivePlan(sched *proactiveScheduler, chatID int64, minutes int, reason string, appts []proactiveAppointment, ok, alive bool) {
	switch {
	case ok:
		sched.SetPlan(chatID, minutes, reason, appts)
	case alive:
		log.Printf("[chat %d] proactive: lượt lưu memory hỏng, vẫn ép mốc mặc định %d phút",
			chatID, defaultProactiveMinutes)
		sched.SetPlan(chatID, defaultProactiveMinutes, "lượt lưu memory lỗi, hẹn mặc định", nil)
	}
}

func loadRuntimeModelSettings(db *memdb.DB, llm *openrouter.Client) {
	if saved, ok, err := db.GetSetting("current_model"); err != nil {
		log.Printf("lỗi đọc current_model: %v", err)
	} else if ok && saved != "" {
		llm.SetModel(saved)
	}
	if saved, ok, err := db.GetSetting("current_provider"); err != nil {
		log.Printf("lỗi đọc current_provider: %v", err)
	} else if ok {
		llm.SetProvider(saved)
	}
}

// ensureSearchIndex rebuilds FTS5 during startup so long-lived topic files
// seeded before this feature are searchable alongside rows kept current by DB triggers.
func ensureSearchIndex(db *memdb.DB, embeddingModel string) error {
	return db.RebuildSearchIndexForEmbeddingModel(embeddingModel)
}

func loadOffset(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return n
}

func saveOffset(path string, offset int) {
	if err := os.WriteFile(path, []byte(strconv.Itoa(offset)), 0644); err != nil {
		log.Printf("lỗi lưu offset: %v", err)
	}
}
