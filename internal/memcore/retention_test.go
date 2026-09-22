package memcore

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"ani-telegram/internal/memdb"
)

func TestTrimMangLeavesShortTextUnchanged(t *testing.T) {
	input := "Em muốn nghe anh kể chuyện hôm nay."
	if got := TrimMang(input); got != input {
		t.Fatalf("TrimMang(%q) = %q, want unchanged", input, got)
	}
}

func TestTrimMangKeepsNewestCompleteSentenceWithinRuneLimit(t *testing.T) {
	old := "OLDEST-SENTENCE-MUST-DROP. " + strings.Repeat("Em vẫn nhớ điều cũ này. ", 120)
	newest := "Em muốn nghe anh kể kết quả sửa server hôm nay."
	got := TrimMang(old + newest)

	if !strings.HasPrefix(got, "…") {
		t.Fatalf("trimmed mang = %q, want leading ellipsis", got)
	}
	if !strings.HasSuffix(got, newest) {
		t.Fatalf("trimmed mang omitted newest sentence: %q", got)
	}
	if strings.Contains(got, "OLDEST-SENTENCE-MUST-DROP") {
		t.Fatalf("trimmed mang retained oldest sentence: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > MaxMangRunes {
		t.Fatalf("trimmed mang has %d runes, limit %d", n, MaxMangRunes)
	}
}

func TestTrimMangDoesNotSplitUnicodeRuneInOverlongSentence(t *testing.T) {
	input := strings.Repeat("ắ", MaxMangRunes+100)
	got := TrimMang(input)
	if !utf8.ValidString(got) {
		t.Fatalf("TrimMang returned invalid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("trimmed mang = %q, want leading ellipsis", got)
	}
	if n := utf8.RuneCountInString(got); n > MaxMangRunes {
		t.Fatalf("trimmed mang has %d runes, limit %d", n, MaxMangRunes)
	}
}

func TestCompactStoredCoreStateTrimsOnlyMangs(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	long := strings.Repeat("Câu cũ cần được xoá dần. ", 120) + "Câu mới cần giữ lại."
	if err := db.SaveCoreState(memdb.CoreState{
		LastMessageNote: "latest note",
		SleepNote:       "sleep note",
		EmotionalState:  "focused",
		Mang1:           long,
		Mang2:           long,
		Mang3:           long,
	}); err != nil {
		t.Fatal(err)
	}

	changed, err := CompactStoredCoreState(db)
	if err != nil {
		t.Fatalf("CompactStoredCoreState: %v", err)
	}
	if !changed {
		t.Fatal("CompactStoredCoreState reported no change for oversized mangs")
	}
	state, err := db.GetCoreState()
	if err != nil {
		t.Fatal(err)
	}
	if state.LastMessageNote != "latest note" || state.SleepNote != "sleep note" || state.EmotionalState != "focused" {
		t.Fatalf("compaction changed non-mang fields: %+v", state)
	}
	for _, mang := range []string{state.Mang1, state.Mang2, state.Mang3} {
		if n := utf8.RuneCountInString(mang); n > MaxMangRunes {
			t.Fatalf("stored mang has %d runes, limit %d", n, MaxMangRunes)
		}
	}
	changed, err = CompactStoredCoreState(db)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("CompactStoredCoreState changed already compact state")
	}
}

func TestApplyUpdateTrimsMangs(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	long := strings.Repeat("Câu cũ cần được xoá dần. ", 120) + "Câu mới cần giữ lại."
	if err := ApplyUpdate(db, Update{
		LastMessageNote: "latest",
		EmotionalState:  "steady",
		Mang1:           long,
		Mang2:           long,
		Mang3:           long,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := db.GetCoreState()
	if err != nil {
		t.Fatal(err)
	}
	for _, mang := range []string{state.Mang1, state.Mang2, state.Mang3} {
		if n := utf8.RuneCountInString(mang); n > MaxMangRunes {
			t.Fatalf("applied mang has %d runes, limit %d", n, MaxMangRunes)
		}
	}
}
