// Package logger provides structured JSONL logging with file rotation.
package logger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxLogSize = 10 * 1024 * 1024 // 10 MB

// Logger writes structured JSONL events to a rotating log file.
type Logger struct {
	mu      sync.Mutex
	file    *os.File
	path    string
	written int64
}

// New opens (or creates) the log file at path.
func New(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("logger: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("logger: open: %w", err)
	}
	info, _ := f.Stat()
	var written int64
	if info != nil {
		written = info.Size()
	}
	return &Logger{file: f, path: path, written: written}, nil
}

// Log writes a single JSONL event. data may be nil.
func (l *Logger) Log(event, account string, data map[string]any) {
	entry := map[string]any{
		"ts":    time.Now().UnixMilli(),
		"event": event,
	}
	if account != "" {
		entry["account"] = account
	}
	if data != nil {
		entry["data"] = data
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.written+int64(len(line)) > maxLogSize {
		l.rotate()
	}

	n, _ := l.file.Write(line)
	l.written += int64(n)
}

// rotate renames the current file to .1 and opens a fresh file.
func (l *Logger) rotate() {
	l.file.Close()
	_ = os.Rename(l.path, l.path+".1")
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	l.file = f
	l.written = 0
}

// Close flushes and closes the underlying file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

// ReadLines returns all log entries (from current + rotated file) as raw JSON objects.
// Entries are sorted oldest-first. Used by the auto-tuner.
//
// The mutex is held only long enough to read raw bytes from disk; JSON parsing
// happens outside the lock so log writes are not blocked during CPU-bound parsing.
func (l *Logger) ReadLines() []map[string]any {
	// Capture raw file contents under the lock, then parse outside it.
	l.mu.Lock()
	var snapshots [][]byte
	for _, p := range []string{l.path + ".1", l.path} {
		data, err := os.ReadFile(p)
		if err == nil {
			snapshots = append(snapshots, data)
		}
	}
	l.mu.Unlock()

	var result []map[string]any
	for _, data := range snapshots {
		dec := json.NewDecoder(bytes.NewReader(data))
		for dec.More() {
			var obj map[string]any
			if err := dec.Decode(&obj); err != nil {
				break
			}
			result = append(result, obj)
		}
	}
	return result
}

