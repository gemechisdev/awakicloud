package logger

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func Init(logLevel string) {
	console := zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: "15:04:05",
		FormatMessage: func(i interface{}) string {
			return fmt.Sprintf("\033[1mgogogot\033[0m %s", i)
		},
	}

	level := parseLevel(logLevel)
	zerolog.SetGlobalLevel(level)
	zerolog.TimeFieldFormat = time.RFC3339

	log.Logger = zerolog.New(console).With().Timestamp().Logger()

	log.Info().Str("level", level.String()).Msg("logger initialized")
}

func parseLevel(s string) zerolog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace", "debug":
		return zerolog.TraceLevel
	case "warn", "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}
