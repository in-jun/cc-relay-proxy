package logger

import (
	"os"
	"testing"
)

func TestLogAndRead(t *testing.T) {
	f, _ := os.CreateTemp("", "proxy-log-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)
	defer os.Remove(path + ".1")

	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Log("startup", "", map[string]any{"numAccounts": 2})
	l.Log("request", "acct1", map[string]any{"method": "POST", "path": "/v1/messages"})

	lines := l.ReadLines()
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	if lines[0]["event"] != "startup" {
		t.Errorf("first event should be startup, got %v", lines[0]["event"])
	}
}

func TestLogNilData(t *testing.T) {
	f, _ := os.CreateTemp("", "proxy-log-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)

	l, _ := New(path)
	defer l.Close()

	l.Log("startup", "acct1", nil)

	lines := l.ReadLines()
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if _, hasData := lines[0]["data"]; hasData {
		t.Error("data field should be absent when nil")
	}
}

func TestLogEmptyAccount(t *testing.T) {
	f, _ := os.CreateTemp("", "proxy-log-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)

	l, _ := New(path)
	defer l.Close()

	l.Log("startup", "", map[string]any{"key": "val"})

	lines := l.ReadLines()
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if _, hasAcct := lines[0]["account"]; hasAcct {
		t.Error("account field should be absent for empty account string")
	}
}

func TestReadLinesFromRotatedFile(t *testing.T) {
	f, _ := os.CreateTemp("", "proxy-log-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)
	defer os.Remove(path + ".1")

	// Write to first logger, then close and rename to simulate rotation
	l, _ := New(path)
	l.Log("event1", "acct1", nil)
	l.Close()
	os.Rename(path, path+".1")

	// Open fresh logger (creates new .log file)
	l2, _ := New(path)
	defer l2.Close()
	l2.Log("event2", "acct2", nil)

	// ReadLines should include both .1 and .log
	lines := l2.ReadLines()
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (from .1 and .log), got %d", len(lines))
	}
	if lines[0]["event"] != "event1" {
		t.Errorf("first line should be from rotated file, got %v", lines[0]["event"])
	}
	if lines[1]["event"] != "event2" {
		t.Errorf("second line should be from current file, got %v", lines[1]["event"])
	}
}

func TestReadLinesMalformedJSON(t *testing.T) {
	f, _ := os.CreateTemp("", "proxy-log-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)

	l, _ := New(path)
	defer l.Close()

	// Write one valid line then malformed JSON directly to the file.
	l.Log("startup", "", map[string]any{"ok": true})
	// Append garbage after a valid line so the decoder hits a decode error.
	os.WriteFile(path, append(func() []byte {
		data, _ := os.ReadFile(path)
		return data
	}(), []byte("not-valid-json\n")...), 0o644)

	lines := l.ReadLines()
	// Should get 1 valid line; malformed entry is silently skipped.
	if len(lines) != 1 {
		t.Fatalf("want 1 valid line (malformed skipped), got %d", len(lines))
	}
}

func TestRotation(t *testing.T) {
	f, _ := os.CreateTemp("", "proxy-log-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)
	defer os.Remove(path + ".1")

	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Write enough to trigger rotation
	big := make([]byte, 512)
	for i := range big {
		big[i] = 'x'
	}
	bigStr := string(big)

	for i := 0; i < 25000; i++ {
		l.Log("test", "", map[string]any{"payload": bigStr})
	}

	// Rotated file should exist
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Error("rotated file not created")
	}
}
