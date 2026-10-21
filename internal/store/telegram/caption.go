package telegram

import (
	"fmt"
	"net/url"
	"strings"
)

// Caption format (Telegrive-inspired, simplified for Bot API):
//
//	<title line>
//	┃ Awaki details
//	┃ awaki:<kind>;key=value;key=value
//
// Only the details line is machine-read. Title is human-facing.

const detailsMarker = "┃ awaki:"

type Record struct {
	Kind   string
	Fields map[string]string
}

func EncodeCaption(title, kind string, fields map[string]string) string {
	var b strings.Builder
	if title == "" {
		title = kind
	}
	// Telegram caption limit ~1024
	if len(title) > 200 {
		title = title[:197] + "…"
	}
	b.WriteString(title)
	b.WriteString("\n┃ Awaki details\n")
	b.WriteString(detailsMarker)
	b.WriteString(kind)
	for k, v := range fields {
		b.WriteByte(';')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(escapeVal(v))
	}
	s := b.String()
	if len(s) > 1024 {
		s = s[:1021] + "…"
	}
	return s
}

func DecodeCaption(caption string) (Record, bool) {
	lines := strings.Split(caption, "\n")
	var details string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, detailsMarker) {
			details = strings.TrimPrefix(line, "┃ ")
			break
		}
		// also accept without box-drawing
		if strings.HasPrefix(line, "awaki:") {
			details = line
			break
		}
	}
	if details == "" {
		return Record{}, false
	}
	details = strings.TrimPrefix(details, "awaki:")
	parts := strings.Split(details, ";")
	if len(parts) == 0 || parts[0] == "" {
		return Record{}, false
	}
	rec := Record{Kind: parts[0], Fields: map[string]string{}}
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		rec.Fields[k] = unescapeVal(v)
	}
	return rec, true
}

func escapeVal(s string) string {
	// percent-encode characters that break our delimiter grammar or Telegram tags
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '%', ';', '#', '@', '$', '\n', '\r':
			b.WriteString("%" + fmt.Sprintf("%02X", r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func unescapeVal(s string) string {
	// use url path unescape for %XX sequences
	out, err := url.PathUnescape(strings.ReplaceAll(s, "+", "%2B"))
	if err != nil {
		return s
	}
	return out
}
