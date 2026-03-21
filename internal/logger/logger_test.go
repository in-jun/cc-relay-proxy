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
