package local

import (
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var tzRegex = regexp.MustCompile(`(?im)^.*timezone:\s*(\S+)`)

func (s *LocalStore) ReadSoul() string {
	data, err := os.ReadFile(s.soulPath())
	if err != nil {
		return ""
	}
	return string(data)
}

func (s *LocalStore) WriteSoul(content string) error {
	return os.WriteFile(s.soulPath(), []byte(content), 0o644)
}

func (s *LocalStore) ReadUser() string {
	data, err := os.ReadFile(s.userPath())
	if err != nil {
		return ""
	}
	return string(data)
}

func (s *LocalStore) WriteUser(content string) error {
	return os.WriteFile(s.userPath(), []byte(content), 0o644)
}

func (s *LocalStore) LoadTimezone() *time.Location {
	if content := s.ReadUser(); content != "" {
		if m := tzRegex.FindStringSubmatch(content); len(m) > 1 {
			if loc, err := time.LoadLocation(m[1]); err == nil {
				return loc
			}
		}
	}
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.UTC
}

func (s *LocalStore) soulPath() string { return filepath.Join(s.dataDir, "soul.md") }
func (s *LocalStore) userPath() string { return filepath.Join(s.dataDir, "user.md") }
