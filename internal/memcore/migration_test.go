package memcore

import (
	"path/filepath"
	"strings"
	"testing"

	"ani-telegram/internal/memdb"
)

func TestMigrateLegacyCoreMemory_IsAtomicAndRunsOnce(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Write(memdb.KeyCoreMemory, templateTestFixture); err != nil {
		t.Fatal(err)
	}

	migrated, err := MigrateLegacyCoreMemory(db)
	if err != nil {
		t.Fatalf("MigrateLegacyCoreMemory: %v", err)
	}
	if !migrated {
		t.Fatal("DB chưa có core_state phải được migrate")
	}
	state, err := db.GetCoreState()
	if err != nil {
		t.Fatal(err)
	}
	if state.LastMessageNote != "10:00 ngày 11/08/2026: test" || state.Mang3 != "cảm xúc C" {
		t.Errorf("core_state migrate sai: %+v", state)
	}
	template, err := db.Read(memdb.KeyCoreMemory)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(template, PlaceholderDiary) || strings.Contains(template, "sự kiện hôm nay") {
		t.Errorf("core/memory phải trở thành template, được: %q", template)
	}
	dates, err := db.DiaryDates()
	if err != nil || len(dates) != 2 {
		t.Fatalf("diary migrate sai: dates=%v err=%v", dates, err)
	}
	tone, err := db.Observations(memdb.ObservationTone)
	if err != nil || len(tone) != 2 {
		t.Fatalf("tone observations migrate sai: count=%d err=%v", len(tone), err)
	}
	prefs, err := db.Observations(memdb.ObservationPreference)
	if err != nil || len(prefs) != 1 {
		t.Fatalf("preference observations migrate sai: count=%d err=%v", len(prefs), err)
	}

	migrated, err = MigrateLegacyCoreMemory(db)
	if err != nil {
		t.Fatalf("migration lần 2: %v", err)
	}
	if migrated {
		t.Fatal("migration lần 2 phải là no-op")
	}
	entries, err := db.DiaryEntriesForDate("2026-08-11")
	if err != nil || len(entries) != 2 {
		t.Fatalf("migration lần 2 không được nhân đôi diary: entries=%v err=%v", entries, err)
	}
	tone, err = db.Observations(memdb.ObservationTone)
	if err != nil || len(tone) != 2 {
		t.Fatalf("migration lần 2 không được nhân đôi observations: count=%d err=%v", len(tone), err)
	}
}

func TestMigrateLegacyCoreMemory_PreservesAlreadyMigratedDB(t *testing.T) {
	db, err := memdb.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Write(memdb.KeyCoreMemory, "template đang dùng"); err != nil {
		t.Fatal(err)
	}
	want := memdb.CoreState{LastMessageNote: "dữ liệu đang sống", Mang1: "không được ghi đè"}
	if err := db.SaveCoreState(want); err != nil {
		t.Fatal(err)
	}

	migrated, err := MigrateLegacyCoreMemory(db)
	if err != nil {
		t.Fatal(err)
	}
	if migrated {
		t.Fatal("DB đã có core_state không được migrate")
	}
	got, err := db.GetCoreState()
	if err != nil {
		t.Fatal(err)
	}
	if got.LastMessageNote != want.LastMessageNote || got.Mang1 != want.Mang1 {
		t.Fatalf("dữ liệu hiện tại bị ghi đè: %+v", got)
	}
}
