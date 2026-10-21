package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
)

// loadDotEnv loads KEY=VALUE pairs from .env files into the process environment
// without overwriting variables that are already set (shell/export wins).
//
// Search order (first existing file wins for a given key; later files skip set keys):
//  1. AWAKI_ENV_FILE if set
//  2. ./.env
//  3. ./.env.local
func loadDotEnv() {
	var paths []string
	if p := os.Getenv("AWAKI_ENV_FILE"); p != "" {
		paths = append(paths, p)
	}
	paths = append(paths, ".env", ".env.local")

	for _, path := range paths {
		if err := applyEnvFile(path); err != nil {
			if !os.IsNotExist(err) {
				log.Warn().Err(err).Str("path", path).Msg("dotenv: could not read env file")
			}
			continue
		}
		abs, _ := filepath.Abs(path)
		log.Info().Str("path", abs).Msg("dotenv: loaded")
	}
}

func applyEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// optional "export KEY=..."
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)

		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "" {
			continue
		}
		// strip matching quotes
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		// do not override existing environment
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, val)
	}
	return sc.Err()
}