package app

import (
	"fmt"
	"os"

	"ani-telegram/internal/safelog"
)

func fatal(format string, args ...any) {
	fmt.Fprintln(os.Stderr, safelog.Redact(fmt.Sprintf(format, args...)))
	os.Exit(1)
}
