package memcore

import (
	"fmt"
	"strings"

	"ani-telegram/internal/memdb"
)

// MigrateLegacyCoreMemory tách blob core/memory đã seed theo định dạng cũ sang các bảng thật.
// DB đã có core_state được giữ nguyên tuyệt đối; phần ghi nguyên tử do memdb đảm nhiệm.
func MigrateLegacyCoreMemory(db *memdb.DB) (bool, error) {
	hasState, err := db.HasCoreState()
	if err != nil {
		return false, err
	}
	if hasState {
		return false, nil
	}
	raw, err := db.Read(memdb.KeyCoreMemory)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(raw) == "" {
		return false, nil
	}

	parts, err := ParseCoreMemoryForMigration(raw)
	if err != nil {
		return false, fmt.Errorf("memcore: tách core/memory cũ lỗi: %w", err)
	}
	migration := memdb.LegacyCoreMigration{
		Template: parts.Template,
		State: memdb.CoreState{
			LastMessageNote: parts.LastMessageNote,
			SleepNote:       parts.SleepNote,
			EmotionalState:  parts.EmotionalState,
			Mang1:           parts.Mang1,
			Mang2:           parts.Mang2,
			Mang3:           parts.Mang3,
		},
	}
	for _, entry := range parts.DiaryEntries {
		migration.DiaryEntries = append(migration.DiaryEntries, memdb.DiaryEntry{
			EntryDate: entry.Date,
			Text:      entry.Text,
		})
	}
	for _, observation := range parts.ToneObservations {
		migration.Observations = append(migration.Observations, memdb.SeedObservation{
			Kind:       memdb.ObservationTone,
			Text:       observation.Text,
			ObservedAt: observation.Date,
		})
	}
	for _, observation := range parts.PreferenceObservations {
		migration.Observations = append(migration.Observations, memdb.SeedObservation{
			Kind:       memdb.ObservationPreference,
			Text:       observation.Text,
			ObservedAt: observation.Date,
		})
	}
	return db.ApplyLegacyCoreMigration(migration)
}
