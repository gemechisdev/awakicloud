package filemgr

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	maxLineBuf   = 16 << 10 // bytes of one line kept in memory
	countCap     = 64 << 20 // stop counting lines after scanning this much
	binarySniff  = 8000
	utf8BOM      = "\uFEFF"
	tabAsSpaces  = "    "
	clippedNote  = " … [line clipped]"
	invalidRune  = "\uFFFD"
	fenceBreaker = "`\u200b``" // ``` → breaks a code fence without changing how it reads
)

// Page is one screenful of a text file.
type Page struct {
	Binary     bool
	Empty      bool
	Size       int64
	Text       string // sanitized, safe inside a ``` fence
	First      int    // first line shown (1-based); 0 if offset was past the end
	Last       int
	Total      int
	TotalExact bool
	More       bool
	Next       int // line to continue from when More
}

// ReadPage reads lines starting at the 1-based line `offset`, stopping once
// ~maxRunes runes of content are collected (always at least one line). It
// streams, so it is safe on very large files, and never loads more than
// maxLineBuf bytes of any single line.
func ReadPage(abs string, offset, maxRunes int) (Page, error) {
	if offset < 1 {
		offset = 1
	}
	if maxRunes < 1 {
		maxRunes = PreviewRunes
	}
	f, err := os.Open(abs)
	if err != nil {
		return Page{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Page{}, err
	}
	if info.IsDir() {
		return Page{}, errors.New("is a directory")
	}
	pg := Page{Size: info.Size(), TotalExact: true}
	if pg.Size == 0 {
		pg.Empty = true
		return pg, nil
	}
	head := make([]byte, binarySniff)
	n, _ := io.ReadFull(f, head)
	if looksBinary(head[:n]) {
		pg.Binary = true
		return pg, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Page{}, err
	}

	br := bufio.NewReaderSize(f, 64<<10)
	var out strings.Builder
	runes := 0
	emitted := 0
	lineNo := 0
	scanned := 0
	stopped := false

	for {
		raw, origLen, err := readLine(br)
		if err != nil && (err != io.EOF || origLen == 0) {
			if err != io.EOF {
				return Page{}, err
			}
			break
		}
		lineNo++
		scanned += origLen + 1
		if lineNo >= offset && !stopped {
			text := cleanLine(raw, lineNo == 1)
			if utf8.RuneCountInString(text) > MaxLineRunes || origLen > len(raw) {
				text = clipRunes(text, MaxLineRunes) + clippedNote
			}
			add := utf8.RuneCountInString(text) + 1
			if emitted > 0 && runes+add > maxRunes {
				stopped = true
				pg.More = true
				pg.Next = lineNo
			} else {
				out.WriteString(text)
				out.WriteByte('\n')
				runes += add
				if emitted == 0 {
					pg.First = lineNo
				}
				pg.Last = lineNo
				emitted++
			}
		}
		if err == io.EOF {
			break
		}
		if stopped && scanned > countCap {
			pg.TotalExact = false
			break
		}
	}
	pg.Total = lineNo
	pg.Text = strings.TrimRight(out.String(), "\n")
	return pg, nil
}

// readLine returns the first maxLineBuf bytes of the next line, the line's
// true length, and io.EOF with origLen>0 for a final unterminated line.
func readLine(br *bufio.Reader) (kept []byte, origLen int, err error) {
	var buf []byte
	for {
		chunk, isPrefix, rerr := br.ReadLine()
		if rerr != nil {
			return buf, origLen, rerr
		}
		origLen += len(chunk)
		if room := maxLineBuf - len(buf); room > 0 {
			if len(chunk) > room {
				chunk = chunk[:room]
			}
			buf = append(buf, chunk...)
		}
		if !isPrefix {
			// A trailing line without "\n" is reported by ReadLine as a normal
			// line followed by io.EOF on the next call; that is fine here.
			return buf, origLen, nil
		}
	}
}

func looksBinary(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	if len(b) > 0 && !utf8.Valid(b) {
		// A truncated multi-byte rune at the sniff boundary is not binary.
		bad := 0
		for i := 0; i < len(b); {
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 && len(b)-i >= utf8.UTFMax {
				bad++
			}
			i += size
		}
		return bad*10 > len(b) // >10% invalid bytes
	}
	return false
}

// cleanLine makes one line safe for a ``` fence.
func cleanLine(b []byte, first bool) string {
	s := string(b)
	if first {
		s = strings.TrimPrefix(s, utf8BOM)
	}
	s = strings.ToValidUTF8(s, invalidRune)
	s = strings.ReplaceAll(s, "\t", tabAsSpaces)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.ReplaceAll(s, "```", fenceBreaker)
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

var langByExt = map[string]string{
	"go": "go", "py": "python", "pyw": "python", "js": "javascript", "mjs": "javascript", "cjs": "javascript", "jsx": "javascript",
	"ts": "typescript", "tsx": "typescript", "json": "json", "jsonc": "json", "yml": "yaml", "yaml": "yaml", "toml": "toml",
	"md": "markdown", "markdown": "markdown", "sh": "bash", "bash": "bash", "zsh": "bash", "html": "html", "htm": "html",
	"css": "css", "scss": "scss", "less": "less", "sql": "sql", "rs": "rust", "php": "php", "java": "java", "kt": "kotlin",
	"kts": "kotlin", "swift": "swift", "c": "c", "h": "c", "cpp": "cpp", "cc": "cpp", "hpp": "cpp", "cs": "csharp",
	"rb": "ruby", "xml": "xml", "svg": "xml", "env": "ini", "ini": "ini", "cfg": "ini", "conf": "ini", "lua": "lua",
	"dart": "dart", "r": "r", "pl": "perl", "scala": "scala", "ex": "elixir", "exs": "elixir", "hs": "haskell",
	"vue": "html", "svelte": "html", "graphql": "graphql", "gql": "graphql", "proto": "protobuf", "tf": "hcl",
	"diff": "diff", "patch": "diff", "ps1": "powershell", "bat": "batch", "cmd": "batch", "csv": "csv", "tex": "latex",
	"gradle": "groovy", "groovy": "groovy", "vim": "vim", "nginx": "nginx", "dockerfile": "dockerfile", "mk": "makefile",
}

// Lang maps a file name to a code-fence language hint ("" = none).
func Lang(name string) string {
	lower := strings.ToLower(name)
	switch lower {
	case "makefile", "gnumakefile":
		return "makefile"
	case "dockerfile":
		return "dockerfile"
	}
	i := strings.LastIndexByte(lower, '.')
	if i < 0 {
		return ""
	}
	if l, ok := langByExt[lower[i+1:]]; ok {
		return l
	}
	return ""
}

func fmtTotal(p Page) string {
	if p.TotalExact {
		return fmt.Sprintf("%d", p.Total)
	}
	return fmt.Sprintf("%d+", p.Total)
}
