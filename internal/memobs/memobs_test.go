package memobs

import (
	"path/filepath"
	"strings"
	"testing"

	"ani-telegram/internal/memdb"
)

func TestExecutor_ReturnsAllOfKind(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.AddObservation(memdb.ObservationTone, "câu ngắn", "2026-08-10"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddObservation(memdb.ObservationPreference, "thích mưa", "2026-08-11"); err != nil {
		t.Fatal(err)
	}

	got, err := Executor(db)(ToolName, `{"kind":"tone"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "câu ngắn") {
		t.Errorf("thiếu tone: %q", got)
	}
	if strings.Contains(got, "thích mưa") {
		t.Errorf("không được trộn preference vào tone: %q", got)
	}
}

func TestTool_HiddenWhenEmpty(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, ok, err := Tool(db)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("không nên đăng ký tool khi chưa có observation")
	}
}
