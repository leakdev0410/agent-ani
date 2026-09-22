package memcore

import (
	"fmt"
	"strings"

	"ani-telegram/internal/memdb"
)

// MaxMangRunes bounds each mutable inner-state field that is injected into the
// system prompt on every reply. It is measured in Unicode code points so
// Vietnamese text is never cut through a UTF-8 byte sequence.
const MaxMangRunes = 1800

// TrimMang normalizes whitespace and preserves the newest complete-sentence
// suffix of an oversized mang. The leading ellipsis is part of the limit and
// tells the model that an older prefix was intentionally discarded.
func TrimMang(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= MaxMangRunes {
		return text
	}

	firstAllowed := len(runes) - (MaxMangRunes - 1)
	for i := firstAllowed; i < len(runes)-1; i++ {
		if isSentenceTerminal(runes[i]) && runes[i+1] == ' ' {
			return "…" + string(runes[i+2:])
		}
	}
	return "…" + string(runes[firstAllowed:])
}

// CompactStoredCoreState applies the mang retention rule to an existing row.
// It avoids a write when the persisted state already satisfies the limit.
func CompactStoredCoreState(db *memdb.DB) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("memory database is nil")
	}
	state, err := db.GetCoreState()
	if err != nil {
		return false, err
	}
	compacted := state
	compacted.Mang1 = TrimMang(state.Mang1)
	compacted.Mang2 = TrimMang(state.Mang2)
	compacted.Mang3 = TrimMang(state.Mang3)
	if compacted == state {
		return false, nil
	}
	if err := db.SaveCoreState(compacted); err != nil {
		return false, err
	}
	return true, nil
}

func isSentenceTerminal(r rune) bool {
	switch r {
	case '.', '!', '?', '…':
		return true
	default:
		return false
	}
}
