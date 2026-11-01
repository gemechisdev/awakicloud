package system

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// textFile is a text file loaded for editing: content is normalised to LF with
// BOM stripped; encode() restores the original BOM / CRLF style on write.
type textFile struct {
	Path    string
	Exists  bool
	Content string
	BOM     bool
	CRLF    bool
	Mode    os.FileMode
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

// loadTextFile reads path. A missing file yields Exists=false and no error.
func loadTextFile(path string) (textFile, error) {
	tf := textFile{Path: path, Mode: 0o644}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return tf, nil
		}
		return tf, fmt.Errorf("stat error: %v", err)
	}
	if info.IsDir() {
		return tf, fmt.Errorf("%s is a directory, not a file", path)
	}
	if info.Size() > maxEditFileBytes {
		return tf, fmt.Errorf("%s is %d bytes; limit is %d bytes — use bash for larger files", path, info.Size(), maxEditFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return tf, fmt.Errorf("read error: %v", err)
	}
	if isBinary(data) {
		return tf, fmt.Errorf("%s looks like a binary file; this tool only handles text", path)
	}
	tf.Exists = true
	tf.Mode = info.Mode().Perm()
	s := string(data)
	if strings.HasPrefix(s, utf8BOM) {
		tf.BOM = true
		s = strings.TrimPrefix(s, utf8BOM)
	}
	if n := strings.Count(s, "\r\n"); n > 0 && n == strings.Count(s, "\n") {
		tf.CRLF = true
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	tf.Content = s
	return tf, nil
}

// encode converts LF-normalised content back to the file's original style.
func (tf textFile) encode(content string) []byte {
	if tf.CRLF {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	if tf.BOM {
		content = utf8BOM + content
	}
	return []byte(content)
}
