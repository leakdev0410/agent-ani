// Package limits defines the byte and cardinality budgets shared by the bot's
// network, prompt and memory layers. Keeping them in one value makes it hard
// for a new call site to accidentally fall back to an unbounded read.
package limits

const MiB = 1024 * 1024

type Limits struct {
	TelegramResponseBytes   int64
	OpenRouterResponseBytes int64
	ErrorPreviewBytes       int64
	ImageBytes              int64
	SystemPromptBytes       int
	UserMessageBytes        int
	ModelReplyBytes         int
	MemoryItemBytes         int
	ToolResultBytes         int
	ToolTotalBytes          int
	ToolCalls               int
	ImportFileBytes         int64
	OpenRouterRequestBytes  int64
	ViewerPageSize          int
	ViewerMaxPageSize       int
	HistoryMaxChats         int
}

func Default() Limits {
	return Limits{
		TelegramResponseBytes:   4 * MiB,
		OpenRouterResponseBytes: 4 * MiB,
		ErrorPreviewBytes:       8 * 1024,
		ImageBytes:              20 * MiB,
		SystemPromptBytes:       256 * 1024,
		UserMessageBytes:        32 * 1024,
		ModelReplyBytes:         64 * 1024,
		MemoryItemBytes:         8 * 1024,
		ToolResultBytes:         64 * 1024,
		ToolTotalBytes:          256 * 1024,
		ToolCalls:               16,
		ImportFileBytes:         2 * MiB,
		OpenRouterRequestBytes:  32 * MiB,
		ViewerPageSize:          50,
		ViewerMaxPageSize:       100,
		HistoryMaxChats:         32,
	}
}
