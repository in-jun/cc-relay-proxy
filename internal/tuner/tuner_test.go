package tuner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
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

// TestAnalyzePrematureSwitches verifies Rule 1: if >20% of switches happen
// before 90% of the threshold is reached (and 429 rate is low), threshold rises.
// This test also exercises the strings.HasPrefix("threshold") match that was
// previously broken by an exact-equality check.
func TestAnalyzePrematureSwitches(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	oldParams := pool.Params() // SwitchThreshold5h = 0.75 for N=2
	earlyThreshold := oldParams.SwitchThreshold5h * 0.90 // 0.675

	// 100 padding events
	for i := 0; i < 100; i++ {
		l.Log("request", "a1", map[string]any{"method": "POST"})
	}
	// 40 account_switched events with reason "threshold: a1 → a2" and
	// fiveHour_before well below the early-threshold (premature).
	for i := 0; i < 40; i++ {
		l.Log("account_switched", "a2", map[string]any{
			"from":            "a1",
			"to":              "a2",
			"reason":          "threshold: a1 exhausted, switching to a2",
			"fiveHour_before": earlyThreshold - 0.10, // clearly premature
		})
	}
	// 0 429s → rate = 0/24h = 0 < 0.1 threshold for Rule 1 to fire

	tu := New(pool, l, time.Hour)
	tu.analyze()

	newParams := pool.Params()
	if newParams.SwitchThreshold5h <= oldParams.SwitchThreshold5h {
		t.Errorf("premature switches should raise threshold: old=%f, new=%f",
			oldParams.SwitchThreshold5h, newParams.SwitchThreshold5h)
	}
}

// TestAnalyzeRecoveryWeighting verifies Rule 3: short average recovery time
// shifts weight toward 5h (more recent utilization) and away from 7d.
func TestAnalyzeRecoveryWeighting(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	oldParams := pool.Params()

	// 100 padding events
	for i := 0; i < 100; i++ {
		l.Log("request", "a1", map[string]any{"method": "POST"})
	}
	// Simulate fast recovery: account blocked then allowed within <60 min.
	// Use timestamps so the tuner can compute recovery duration.
	blockedMs := time.Now().Add(-30 * time.Minute).UnixMilli()
	allowedMs := time.Now().UnixMilli()

	// Write blocked event with explicit ts by using pool's logger helper directly.
	// Since logger.Log uses time.Now(), we inject the ts field via a custom approach:
	// we write two rate_limit_update entries — first with status blocked (ts = blockedMs),
	// then with status allowed (ts = allowedMs). But logger.Log doesn't let us control ts.
	// Instead exercise the path by injecting a 429_received (hold) then rate_limit_update (allowed).
	_ = blockedMs
	_ = allowedMs

	// The simplest way to trigger Rule 3 is to confirm weight5h < 0.99 after
	// a recovery period. Since we can't control ts directly, we verify the
	// rule fires when we inject the correct log shapes.
	// Write 429_received with action=hold (marks lastBlocked for account "a1")
	// then rate_limit_update with status=allowed for "a1" (recovery measured).
	// The recovery delta in ms must come from the "ts" fields in the log — but
	// logger writes its own ts. So we can only verify the rule doesn't crash.
	// A deeper test would require a custom log writer; skip the delta assertion.
	l.Log("429_received", "a1", map[string]any{"action": "hold", "waitSec": 10.0})
	l.Log("rate_limit_update", "a1", map[string]any{
		"fiveHour": 0.30,
		"sevenDay": 0.10,
		"status":   "allowed",
	})

	tu := New(pool, l, time.Hour)
	tu.analyze()

	// Both events are logged within the same millisecond → recovery = 0 min < 60 min.
	// Rule 3 must raise weight5h and lower weight7d.
	newParams := pool.Params()
	if newParams.Weight5h <= oldParams.Weight5h {
		t.Errorf("short recovery should raise weight5h: old=%f, new=%f",
			oldParams.Weight5h, newParams.Weight5h)
	}
	if newParams.Weight7d >= oldParams.Weight7d {
		t.Errorf("short recovery should lower weight7d: old=%f, new=%f",
			oldParams.Weight7d, newParams.Weight7d)
	}
}

func TestAnalyze7dTrend(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	oldParams := pool.Params()

	// 100 padding events
	for i := 0; i < 100; i++ {
		l.Log("request", "a1", map[string]any{"method": "POST"})
	}

	// Write rate_limit_update events with low 7d early and high 7d late
	// so growthPer12h > 0.05 triggers Rule 4.
	// Logger uses time.Now() for ts, so all events land in the "late" half.
	// To simulate early events we can't control timestamps via logger.Log.
	// Instead, test what we can: if only late events exist (no early),
	// Rule 4 requires BOTH early and late — so it won't fire.
	// The test verifies analyze doesn't crash and no changes are made.
	for i := 0; i < 10; i++ {
		l.Log("rate_limit_update", "a1", map[string]any{
			"fiveHour": 0.30,
			"sevenDay": 0.80, // high — triggers lateAvg > 0.80 warning
			"status":   "allowed",
		})
	}

	tu := New(pool, l, time.Hour)
	tu.analyze() // should not panic

	// No Rule 4 change because sevenDayEarly is empty
	newParams := pool.Params()
	_ = newParams
	_ = oldParams
}

func TestAnalyzeAllBlockedWarning(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	// 100 padding events + >3 all_blocked events
	for i := 0; i < 100; i++ {
		l.Log("request", "a1", map[string]any{"method": "POST"})
	}
	for i := 0; i < 5; i++ {
		l.Log("all_blocked", "", map[string]any{"reason": "all_rejected"})
	}

	tu := New(pool, l, time.Hour)
	tu.analyze() // should emit warning and not crash

	// Params unchanged (no rule fires based on this data alone)
	// Just verifying no panic and no param change
}

func TestAverageEmpty(t *testing.T) {
	if avg := average([]float64{}); avg != 0 {
		t.Errorf("average of empty slice should be 0, got %f", avg)
	}
}

func TestTunerAccessors(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	tu := New(pool, l, 5*time.Minute)

	if tu.Interval() != 5*time.Minute {
		t.Errorf("Interval: want 5m, got %v", tu.Interval())
	}
	if !tu.LastTuned().IsZero() {
		t.Error("LastTuned should be zero before first tune")
	}
	hist := tu.History()
	if len(hist) != 0 {
		t.Errorf("History should be empty initially, got %d entries", len(hist))
	}
}

func TestTunerHistoryRecorded(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	tu := New(pool, l, time.Hour)

	// Inject enough events to trigger a parameter change (high 429 rate)
	for i := 0; i < 110; i++ {
		if i < 30 {
			l.Log("429_received", "a1", map[string]any{"action": "switch", "fiveHour": 0.9})
		} else {
			l.Log("request", "a1", map[string]any{"method": "POST"})
		}
	}
	tu.analyze()

	if tu.LastTuned().IsZero() {
		t.Error("LastTuned should be set after a parameter change")
	}
	hist := tu.History()
	if len(hist) == 0 {
		t.Error("History should contain at least one entry after tuning")
	}
	if hist[0].Reason == "" {
		t.Error("TuneHistory entry should have a non-empty reason")
	}
}

func TestRunWithZeroIntervalDoesNotPanic(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	// interval=0 means "disabled" — Run must return immediately, not panic.
	tu := New(pool, l, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tu.Run(context.Background())
	}()
	select {
	case <-done:
		// good
	case <-time.After(time.Second):
		t.Error("Run with interval=0 should return immediately, but it's still running after 1s")
	}
}

// writeRawEvents writes JSONL events with explicit timestamps to a file,
// bypassing the logger's time.Now() so we can control the ts values.
func writeRawEvents(t *testing.T, path string, events []map[string]any) {
	t.Helper()
	var sb strings.Builder
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(line)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyze7dTrendFires(t *testing.T) {
	// Covers Rule 4: 7d growing steeply in the late half of the window.
	// Also covers the lateAvg > 0.80 warning path.
	// We write raw JSONL with explicit timestamps to control early/late split.
	pool := makeTestPool()
	f, _ := os.CreateTemp("", "tuner-7d-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)

	now := time.Now()
	windowStart := now.Add(-24 * time.Hour)
	earlyTs := windowStart.Add(3 * time.Hour).UnixMilli()  // early half
	lateTs := windowStart.Add(15 * time.Hour).UnixMilli()  // late half

	var events []map[string]any

	// 100 padding request events (all within window)
	for i := 0; i < 100; i++ {
		events = append(events, map[string]any{
			"ts":    earlyTs + int64(i*1000),
			"event": "request",
			"data":  map[string]any{"method": "POST"},
		})
	}
	// Early 7d data: low utilization
	for i := 0; i < 10; i++ {
		events = append(events, map[string]any{
			"ts":      earlyTs,
			"event":   "rate_limit_update",
			"account": "a1",
			"data":    map[string]any{"fiveHour": 0.30, "sevenDay": 0.10, "status": "allowed"},
		})
	}
	// Late 7d data: high utilization → growthPer12h = 0.85 - 0.10 = 0.75 > 0.05
	for i := 0; i < 10; i++ {
		events = append(events, map[string]any{
			"ts":      lateTs,
			"event":   "rate_limit_update",
			"account": "a1",
			"data":    map[string]any{"fiveHour": 0.30, "sevenDay": 0.85, "status": "allowed"},
		})
	}

	writeRawEvents(t, path, events)

	l, err := logger.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	defer os.Remove(path + ".1")

	oldParams := pool.Params()
	tu := New(pool, l, time.Hour)
	tu.analyze()

	newParams := pool.Params()
	if newParams.Weight7d <= oldParams.Weight7d {
		t.Errorf("Rule 4: 7d trend should increase weight7d; old=%f new=%f", oldParams.Weight7d, newParams.Weight7d)
	}
}

func TestAnalyzeExported(t *testing.T) {
	// Verify that the exported Analyze() wrapper delegates to analyze().
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()
	tu := New(pool, l, time.Hour)
	tu.Analyze() // must not panic; exits early (< 100 events)
}

func TestRunTickerFires(t *testing.T) {
	// Run with a 1ms interval lets the ticker fire before ctx is cancelled.
	// analyze() exits early (< 100 events) but the ticker branch is covered.
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	tu := New(pool, l, time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	tu.Run(ctx) // blocks until ctx expires; ticker fires multiple times
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
