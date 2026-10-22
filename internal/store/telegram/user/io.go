//go:build !nogotd

package user

import (
	"bytes"
	"io"
)

func bytesNewReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}

type writerBuffer struct {
	buf bytes.Buffer
}

func (w *writerBuffer) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *writerBuffer) Bytes() []byte               { return w.buf.Bytes() }
