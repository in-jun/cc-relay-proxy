package tuner

import (
	"os"
	"testing"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
)

func makeTestPool() *accounts.Pool {
	accts, _ := accounts.ParseAccounts(`[{"name":"a1","refreshToken":"rt1"},{"name":"a2","refreshToken":"rt2"}]`)
	return accounts.NewPool(accts)
}

func makeTestLogger(t *testing.T) (*logger.Logger, func()) {
	f, _ := os.CreateTemp("", "tuner-test-*.jsonl")
	f.Close()
	l, _ := logger.New(f.Name())
	cleanup := func() {
		l.Close()
		os.Remove(f.Name())
		os.Remove(f.Name() + ".1")
	}
	return l, cleanup
}

func TestAnalyzeSkipsIfTooFewEvents(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	oldParams := pool.Params()

	tu := New(pool, l, time.Hour)
	tu.analyze() // only 0 events — should skip

	if pool.Params() != oldParams {
		t.Error("params should not change with too few events")
	}
}

func TestAnalyzeRaises429Rate(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	oldParams := pool.Params()

	// Inject 100+ events including >1/hour 429s
	now := time.Now()
	for i := 0; i < 110; i++ {
		event := "request"
		data := map[string]any{"method": "POST"}
		if i < 30 {
			event = "429_received"
			data = map[string]any{"action": "switch", "fiveHour": 0.9}
		}
		_ = now
		l.Log(event, "a1", data)
	}

	tu := New(pool, l, time.Hour)
	tu.analyze()

	newParams := pool.Params()
	if newParams.SwitchThreshold5h >= oldParams.SwitchThreshold5h {
		t.Errorf("high 429 rate should lower threshold: old=%f, new=%f",
			oldParams.SwitchThreshold5h, newParams.SwitchThreshold5h)
	}
}

func TestClamp(t *testing.T) {
	if clamp(1.5, 0, 1) != 1 {
		t.Error("clamp max failed")
	}
	if clamp(-0.5, 0, 1) != 0 {
		t.Error("clamp min failed")
	}
	if clamp(0.5, 0, 1) != 0.5 {
		t.Error("clamp mid failed")
	}
}
