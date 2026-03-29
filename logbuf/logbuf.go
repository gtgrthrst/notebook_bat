// Package logbuf provides a thread-safe in-memory ring buffer for log lines.
package logbuf

import (
	"bytes"
	"sync"
)

const maxLines = 500

// Buffer is a fixed-size ring buffer that implements io.Writer.
// It stores the last maxLines log lines.
type Buffer struct {
	mu    sync.RWMutex
	lines []string
}

// Write appends a log line to the buffer, dropping the oldest if full.
func (b *Buffer) Write(p []byte) (int, error) {
	s := string(bytes.TrimRight(p, "\r\n"))
	if s == "" {
		return len(p), nil
	}
	b.mu.Lock()
	b.lines = append(b.lines, s)
	if len(b.lines) > maxLines {
		b.lines = b.lines[len(b.lines)-maxLines:]
	}
	b.mu.Unlock()
	return len(p), nil
}

// Lines returns a copy of all buffered lines in chronological order.
func (b *Buffer) Lines() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}
