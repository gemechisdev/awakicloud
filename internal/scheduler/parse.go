package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParsedSchedule is the normalized form of a schedule expression.
type ParsedSchedule struct {
	Kind     string    // cron | once | interval
	CronExpr string    // for cron / @every
	RunAt    time.Time // for once
	Interval time.Duration
	Display  string // human-readable schedule string stored on the task
}

// ParseSchedule accepts many forms:
//
//	"0 8 * * *"              → cron
//	"@daily" / "@hourly"     → cron descriptors
//	"@every 5m"              → interval (via cron)
//	"every 5m" / "every:5m"  → interval
//	"in 10m" / "once:10m" / "+10m" → one-shot relative
//	"at 2026-10-25T12:00:00Z" / RFC3339 → one-shot absolute
func ParseSchedule(raw string, now time.Time) (ParsedSchedule, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ParsedSchedule{}, fmt.Errorf("schedule is empty")
	}
	lower := strings.ToLower(raw)

	// Absolute one-shot: at <RFC3339> or bare RFC3339
	if strings.HasPrefix(lower, "at ") {
		ts := strings.TrimSpace(raw[3:])
		t, err := parseTime(ts, now)
		if err != nil {
			return ParsedSchedule{}, err
		}
		return ParsedSchedule{Kind: "once", RunAt: t, Display: "at " + t.UTC().Format(time.RFC3339)}, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return ParsedSchedule{Kind: "once", RunAt: t, Display: "at " + t.UTC().Format(time.RFC3339)}, nil
	}

	// Relative one-shot
	for _, prefix := range []string{"in ", "once:", "once ", "+"} {
		if strings.HasPrefix(lower, prefix) {
			durStr := strings.TrimSpace(raw[len(prefix):])
			d, err := parseDuration(durStr)
			if err != nil {
				return ParsedSchedule{}, fmt.Errorf("invalid duration %q: %w", durStr, err)
			}
			if d <= 0 {
				return ParsedSchedule{}, fmt.Errorf("duration must be positive")
			}
			runAt := now.Add(d)
			return ParsedSchedule{Kind: "once", RunAt: runAt, Display: fmt.Sprintf("in %s (at %s)", d, runAt.UTC().Format(time.RFC3339))}, nil
		}
	}

	// Interval: every 5m / every:5m / @every 5m
	if strings.HasPrefix(lower, "every:") || strings.HasPrefix(lower, "every ") {
		durStr := strings.TrimSpace(raw[len("every"):])
		durStr = strings.TrimPrefix(durStr, ":")
		durStr = strings.TrimSpace(durStr)
		d, err := parseDuration(durStr)
		if err != nil {
			return ParsedSchedule{}, fmt.Errorf("invalid interval %q: %w", durStr, err)
		}
		if d < time.Second {
			return ParsedSchedule{}, fmt.Errorf("interval must be at least 1s")
		}
		return ParsedSchedule{
			Kind:     "interval",
			CronExpr: "@every " + formatDur(d),
			Interval: d,
			Display:  "every " + formatDur(d),
		}, nil
	}
	if strings.HasPrefix(lower, "@every ") {
		durStr := strings.TrimSpace(raw[len("@every "):])
		d, err := parseDuration(durStr)
		if err != nil {
			return ParsedSchedule{}, err
		}
		return ParsedSchedule{
			Kind:     "interval",
			CronExpr: "@every " + formatDur(d),
			Interval: d,
			Display:  "every " + formatDur(d),
		}, nil
	}

	// Standard cron or @hourly/@daily/@weekly/@monthly/@yearly
	return ParsedSchedule{Kind: "cron", CronExpr: raw, Display: raw}, nil
}

func parseTime(ts string, now time.Time) (time.Time, error) {
	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"15:04",
		"15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, ts, now.Location()); err == nil {
			// bare time-of-day → today or tomorrow
			if layout == "15:04" || layout == "15:04:05" {
				t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location())
				if !t.After(now) {
					t = t.Add(24 * time.Hour)
				}
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q", ts)
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	// allow "5 minutes", "10 min", "2 hours"
	repl := []struct{ a, b string }{
		{" hours", "h"}, {" hour", "h"}, {" hrs", "h"}, {" hr", "h"},
		{" minutes", "m"}, {" minute", "m"}, {" mins", "m"}, {" min", "m"},
		{" seconds", "s"}, {" second", "s"}, {" secs", "s"}, {" sec", "s"},
		{" days", "h"}, // handle below
	}
	for _, r := range repl {
		s = strings.ReplaceAll(s, r.a, r.b)
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func formatDur(d time.Duration) string {
	if d%time.Hour == 0 && d >= time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 && d >= time.Minute {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}
