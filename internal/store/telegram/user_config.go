package telegram

// UserConfig configures Telegrive-like user-account storage.
type UserConfig struct {
	APIID        int
	APIHash      string
	Phone        string
	Password     string
	SessionPath  string
	CodeFile     string
	CacheDir     string
	ChannelTitle string
}
