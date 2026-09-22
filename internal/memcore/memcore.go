// Package memcore updates Ani's durable core state after each turn.
package memcore

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ani-telegram/internal/memdb"
)

type Update struct {
	LastMessageNote       string
	SleepNote             string
	EmotionalState        string
	ToneObservation       string
	PreferenceObservation string
	DiaryEntry            string
	DiaryTopic            string
	Mang1                 string
	Mang2                 string
	Mang3                 string
}

// CommittedSource identifies one exact searchable row written by an update.
// SourceKey is canonical for memdb's embedding queue.
type CommittedSource struct {
	SourceKey string
	Text      string
}

var ValidTopics = []string{"projects", "personal", "preferences", "log"}

const (
	lastMessagePrefix = "- Lần nhắn gần nhất của anh:"
	sleepPrefix       = "- Lần anh bảo đi ngủ gần nhất:"
	emotionalPrefix   = "- Hiện tại:"
	diaryMarker       = "(phân tầng theo ngày):"
	mang1Prefix       = "1. **Mong muốn của em**:"
	mang2Prefix       = "2. **Điều em muốn làm cùng anh**:"
	mang3Prefix       = "3. **Cảm xúc cá nhân tự hình thành**:"
)

func NormalizeTopic(topic string) string {
	topic = strings.ToLower(strings.TrimSpace(topic))
	if topic == "none" {
		return ""
	}
	for _, valid := range ValidTopics {
		if topic == valid {
			return topic
		}
	}
	return ""
}

// ApplyUpdate is the backwards-compatible wrapper for callers that do not
// already own a transaction.
func ApplyUpdate(db *memdb.DB, u Update) error {
	return db.InTx(func(tx *sql.Tx) error {
		return ApplyUpdateTx(tx, u, time.Now())
	})
}

// ApplyUpdateTx writes u using only tx, so it can share an atomic commit with
// job deletion and related durable work.
func ApplyUpdateTx(tx *sql.Tx, u Update, now time.Time) error {
	_, err := ApplyUpdateTxWithSources(tx, u, now)
	return err
}

// ApplyUpdateTxWithSources returns the stable source identities created by
// this update. Callers must queue these exact keys rather than reconstructing
// ownership from untrusted extraction text.
func ApplyUpdateTxWithSources(tx *sql.Tx, u Update, now time.Time) ([]CommittedSource, error) {
	if tx == nil {
		return nil, fmt.Errorf("memory update transaction is nil")
	}
	if strings.TrimSpace(u.LastMessageNote) == "" || strings.TrimSpace(u.EmotionalState) == "" ||
		strings.TrimSpace(u.Mang1) == "" || strings.TrimSpace(u.Mang2) == "" || strings.TrimSpace(u.Mang3) == "" {
		return nil, fmt.Errorf("missing required core-state field")
	}

	var state memdb.CoreState
	err := tx.QueryRow(`SELECT last_message_note, sleep_note, emotional_state, mang1, mang2, mang3
		FROM core_state WHERE id = 1`).Scan(
		&state.LastMessageNote, &state.SleepNote, &state.EmotionalState,
		&state.Mang1, &state.Mang2, &state.Mang3,
	)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("read core state: %w", err)
	}
	state.LastMessageNote = strings.TrimSpace(u.LastMessageNote)
	if strings.TrimSpace(u.SleepNote) != "" {
		state.SleepNote = strings.TrimSpace(u.SleepNote)
	}
	state.EmotionalState = strings.TrimSpace(u.EmotionalState)
	state.Mang1 = TrimMang(u.Mang1)
	state.Mang2 = TrimMang(u.Mang2)
	state.Mang3 = TrimMang(u.Mang3)

	createdAt := now.UTC().Format(time.RFC3339)
	if _, err := tx.Exec(
		`INSERT INTO core_state (id, last_message_note, sleep_note, emotional_state, mang1, mang2, mang3, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   last_message_note = excluded.last_message_note,
		   sleep_note = excluded.sleep_note,
		   emotional_state = excluded.emotional_state,
		   mang1 = excluded.mang1,
		   mang2 = excluded.mang2,
		   mang3 = excluded.mang3,
		   updated_at = excluded.updated_at`,
		state.LastMessageNote, state.SleepNote, state.EmotionalState, state.Mang1, state.Mang2, state.Mang3, createdAt,
	); err != nil {
		return nil, fmt.Errorf("write core state: %w", err)
	}

	today := now.Format("2006-01-02")
	var sources []CommittedSource
	if text := strings.TrimSpace(u.ToneObservation); text != "" {
		result, err := tx.Exec("INSERT INTO observations (kind, text, observed_at, created_at) VALUES (?, ?, ?, ?)", memdb.ObservationTone, text, today, createdAt)
		if err != nil {
			return nil, fmt.Errorf("write tone observation: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read tone observation ID: %w", err)
		}
		sources = append(sources, CommittedSource{SourceKey: "observation:" + memdb.ObservationTone + ":" + strconv.FormatInt(id, 10), Text: text})
	}
	if text := strings.TrimSpace(u.PreferenceObservation); text != "" {
		result, err := tx.Exec("INSERT INTO observations (kind, text, observed_at, created_at) VALUES (?, ?, ?, ?)", memdb.ObservationPreference, text, today, createdAt)
		if err != nil {
			return nil, fmt.Errorf("write preference observation: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read preference observation ID: %w", err)
		}
		sources = append(sources, CommittedSource{SourceKey: "observation:" + memdb.ObservationPreference + ":" + strconv.FormatInt(id, 10), Text: text})
	}
	if text := strings.TrimSpace(u.DiaryEntry); text != "" {
		result, err := tx.Exec("INSERT INTO diary_entries (entry_date, text, topic, created_at) VALUES (?, ?, ?, ?)", today, text, NormalizeTopic(u.DiaryTopic), createdAt)
		if err != nil {
			return nil, fmt.Errorf("write diary entry: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read diary entry ID: %w", err)
		}
		sources = append(sources, CommittedSource{SourceKey: "diary:" + strconv.FormatInt(id, 10) + ":" + today, Text: text})
	}
	return sources, nil
}
