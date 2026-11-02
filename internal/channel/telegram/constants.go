package telegram

const (
	maxMessageLen = 4000
	// Slightly lower when an inline keyboard is attached so HTML expansion
	// and the reply_markup payload stay under Telegram limits.
	maxKeyboardMessageLen = 3500

	maxTextFileSize    = 512 * 1024
	maxImageFileSize   = 10 * 1024 * 1024
	maxGenericFileSize = 20 * 1024 * 1024
	maxArchiveEntries  = 20
)
