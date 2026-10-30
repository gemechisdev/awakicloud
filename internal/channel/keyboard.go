package channel

// Button is one inline keyboard cell.
// Prefer CallbackData for in-chat actions, or URL to open a browser link
// (e.g. MCP OAuth authorize).
type Button struct {
	Text         string
	CallbackData string
	URL          string // if set, Telegram shows a URL button (opens browser)
}

// KeyboardMessage is a text reply with optional inline buttons (Telegram).
type KeyboardMessage struct {
	Text string
	Rows [][]Button
	// ReplyKeyboard is a persistent bottom reply keyboard (label rows).
	ReplyKeyboard [][]string
	// RemoveReplyKeyboard clears the persistent keyboard when true.
	RemoveReplyKeyboard bool
}
