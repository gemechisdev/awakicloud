package channel

// Button is one inline keyboard cell.
type Button struct {
	Text         string
	CallbackData string
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
