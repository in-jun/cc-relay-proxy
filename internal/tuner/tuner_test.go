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

// --- Core correctness ---

func TestAnalyzeSkipsIfTooFewEvents(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	oldParams := pool.Params()
	tu := New(pool, l, time.Hour)
	tu.analyze() // 0 events — must skip
	if pool.Params() != oldParams {
		t.Error("params should not change with too few events")
	}
}

// TestThresholdPControllerLowers verifies the P-controller lowers switchThreshold5h
// when the 429 rate is high. Mathematical expectation:
//
//	Δ = η*(prematureRatio - target) - κ*rate429
//	  = 0.02*(0 - 0.10) - 0.10*(30/24)
//	  ≈ -0.127  →  threshold decreases
func TestThresholdPControllerLowers(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	oldParams := pool.Params()

	for i := 0; i < 110; i++ {
		event := "request"
		data := map[string]any{"method": "POST"}
		if i < 30 {
			event = "429_received"
			data = map[string]any{"action": "switch", "fiveHour": 0.9}
		}
		l.Log(event, "a1", data)
	}

	tu := New(pool, l, time.Hour)
	tu.analyze()

	if pool.Params().SwitchThreshold5h >= oldParams.SwitchThreshold5h {
		t.Errorf("high 429 rate should lower threshold: old=%f new=%f",
			oldParams.SwitchThreshold5h, pool.Params().SwitchThreshold5h)
	}
}

// TestThresholdPControllerRaises verifies the P-controller raises switchThreshold5h
// when the premature-switch ratio exceeds the target and 429s are absent.
//
//	Δ = η*(prematureRatio - target) - κ*0
//	  = 0.02*(1.0 - 0.10)
//	  = 0.018  →  threshold increases
func TestThresholdPControllerRaises(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	oldParams := pool.Params()
	earlyThreshold := oldParams.SwitchThreshold5h * 0.90

	for i := 0; i < 100; i++ {
		l.Log("request", "a1", map[string]any{"method": "POST"})
	}
	for i := 0; i < 40; i++ {
		l.Log("account_switched", "a2", map[string]any{
			"from":            "a1",
			"to":              "a2",
			"reason":          "threshold: a1 exhausted, switching to a2",
			"fiveHour_before": earlyThreshold - 0.10,
		})
	}

	tu := New(pool, l, time.Hour)
	tu.analyze()

	if pool.Params().SwitchThreshold5h <= oldParams.SwitchThreshold5h {
		t.Errorf("high premature-switch ratio should raise threshold: old=%f new=%f",
			oldParams.SwitchThreshold5h, pool.Params().SwitchThreshold5h)
	}
}

// TestThresholdClampedAtMax verifies that when the threshold is already at the
// upper bound (0.98), the P-controller cannot raise it further.
func TestThresholdClampedAtMax(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	pool.SetParams(accounts.Params{
		SwitchThreshold5h: 0.98,
		HardBlock7d:       0.90,
		Weight5h:          0.80,
		Weight7d:          0.20,
	})
	oldParams := pool.Params()

	for i := 0; i < 100; i++ {
		l.Log("request", "a1", map[string]any{"method": "POST"})
	}
	// 100% premature switches → Δ = 0.018, clamped 0.98+0.018→0.98
	for i := 0; i < 40; i++ {
		l.Log("account_switched", "a2", map[string]any{
			"from":            "a1",
			"to":              "a2",
			"reason":          "threshold: early",
			"fiveHour_before": 0.10,
		})
	}

	tu := New(pool, l, time.Hour)
	tu.analyze()

	if pool.Params() != oldParams {
		t.Errorf("threshold already at max, should not change: old=%v new=%v", oldParams, pool.Params())
	}
}

// --- Hysteresis P-controller ---

// TestHysteresisRisesOnLowEfficiency verifies that when proactive switches
// are mostly unjustified (efficiency < 75%), hysteresis increases.
//
//	Δ = η*(target - efficiency) = 0.02*(0.75 - 0.0) = 0.015  →  rises
func TestHysteresisRisesOnLowEfficiency(t *testing.T) {
	pool := makeTestPool()
	f, _ := os.CreateTemp("", "tuner-hyst-low-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)
	defer os.Remove(path + ".1")

	pool.SetParams(accounts.Params{
		SwitchThreshold5h:   0.75,
		HardBlock7d:         0.90,
		Weight5h:            0.80,
		Weight7d:            0.20,
		ProactiveHysteresis: 0.20,
	})
	oldHyst := pool.Params().ProactiveHysteresis

	now := time.Now()
	switchTs := now.Add(-30 * time.Minute).UnixMilli()
	// pool_snapshot 11 minutes after each switch, showing to-account (a2) is WORSE
	snapTs := now.Add(-30*time.Minute + 11*time.Minute).UnixMilli()

	var events []map[string]any

	// 100 padding
	for i := 0; i < 100; i++ {
		events = append(events, map[string]any{
			"ts":    now.Add(-time.Duration(i) * time.Minute).UnixMilli(),
			"event": "request",
			"data":  map[string]any{"method": "POST"},
		})
	}
	// 6 proactive switches from a1→a2
	for i := 0; i < 6; i++ {
		events = append(events, map[string]any{
			"ts":      switchTs + int64(i*100),
			"event":   "account_switched",
			"account": "a2",
			"data": map[string]any{
				"from": "a1", "to": "a2",
				"reason":          "proactive: a1 water=0.70 → a2 water=0.40",
				"fiveHour_before": 0.56,
			},
		})
		// snapshot: a2 (to) is worse → switch was NOT justified → bad switch
		events = append(events, map[string]any{
			"ts":    snapTs + int64(i*100),
			"event": "pool_snapshot",
			"data": map[string]any{
				"trigger": "periodic",
				"accounts": []any{
					map[string]any{"name": "a1", "water": 0.30, "fiveHour": 0.24, "sevenDay": 0.10, "status": "allowed", "isActive": false},
					map[string]any{"name": "a2", "water": 0.80, "fiveHour": 0.64, "sevenDay": 0.20, "status": "allowed", "isActive": true},
				},
			},
		})
	}

	writeRawEvents(t, path, events)
	l, err := logger.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	tu := New(pool, l, time.Hour)
	tu.analyze()

	newHyst := pool.Params().ProactiveHysteresis
	if newHyst <= oldHyst {
		t.Errorf("low proactive efficiency should raise hysteresis: old=%f new=%f", oldHyst, newHyst)
	}
}

// TestHysteresisDropsOnHighEfficiency verifies that when proactive switches
// are consistently justified (efficiency > 75%), hysteresis decreases.
//
//	Δ = η*(target - efficiency) = 0.02*(0.75 - 1.0) = -0.005  →  drops
func TestHysteresisDropsOnHighEfficiency(t *testing.T) {
	pool := makeTestPool()
	f, _ := os.CreateTemp("", "tuner-hyst-high-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)
	defer os.Remove(path + ".1")

	pool.SetParams(accounts.Params{
		SwitchThreshold5h:   0.75,
		HardBlock7d:         0.90,
		Weight5h:            0.80,
		Weight7d:            0.20,
		ProactiveHysteresis: 0.20,
	})
	oldHyst := pool.Params().ProactiveHysteresis

	now := time.Now()
	switchTs := now.Add(-30 * time.Minute).UnixMilli()
	snapTs := now.Add(-30*time.Minute + 11*time.Minute).UnixMilli()

	var events []map[string]any

	for i := 0; i < 100; i++ {
		events = append(events, map[string]any{
			"ts":    now.Add(-time.Duration(i) * time.Minute).UnixMilli(),
			"event": "request",
			"data":  map[string]any{"method": "POST"},
		})
	}
	// 6 proactive switches from a1→a2, all justified (a2 stays cheaper 11min later)
	for i := 0; i < 6; i++ {
		events = append(events, map[string]any{
			"ts":      switchTs + int64(i*100),
			"event":   "account_switched",
			"account": "a2",
			"data": map[string]any{
				"from": "a1", "to": "a2",
				"reason":          "proactive: a1 water=0.70 → a2 water=0.40",
				"fiveHour_before": 0.56,
			},
		})
		// snapshot: a2 (to) is still better → switch WAS justified → good
		events = append(events, map[string]any{
			"ts":    snapTs + int64(i*100),
			"event": "pool_snapshot",
			"data": map[string]any{
				"trigger": "periodic",
				"accounts": []any{
					map[string]any{"name": "a1", "water": 0.80, "fiveHour": 0.64, "sevenDay": 0.20, "status": "allowed", "isActive": false},
					map[string]any{"name": "a2", "water": 0.30, "fiveHour": 0.24, "sevenDay": 0.10, "status": "allowed", "isActive": true},
				},
			},
		})
	}

	writeRawEvents(t, path, events)
	l, err := logger.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	tu := New(pool, l, time.Hour)
	tu.analyze()

	newHyst := pool.Params().ProactiveHysteresis
	if newHyst >= oldHyst {
		t.Errorf("high proactive efficiency should lower hysteresis: old=%f new=%f", oldHyst, newHyst)
	}
}

// TestHysteresisUnchangedWithTooFewEvaluations verifies that the hysteresis
// P-controller skips when fewer than 5 proactive switches have follow-up snapshots.
func TestHysteresisUnchangedWithTooFewEvaluations(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	pool.SetParams(accounts.Params{
		SwitchThreshold5h:   0.75,
		HardBlock7d:         0.90,
		Weight5h:            0.80,
		Weight7d:            0.20,
		ProactiveHysteresis: 0.15,
	})
	oldHyst := pool.Params().ProactiveHysteresis

	for i := 0; i < 100; i++ {
		l.Log("request", "a1", map[string]any{"method": "POST"})
	}
	// Only 3 proactive switches, no follow-up snapshots → proactiveTotal < 5
	for i := 0; i < 3; i++ {
		l.Log("account_switched", "a2", map[string]any{
			"from": "a1", "to": "a2",
			"reason":          "proactive: a1 water=0.70 → a2 water=0.40",
			"fiveHour_before": 0.56,
		})
	}

	tu := New(pool, l, time.Hour)
	tu.analyze()

	if pool.Params().ProactiveHysteresis != oldHyst {
		t.Errorf("hysteresis should not change with < 5 evaluable switches: old=%f new=%f",
			oldHyst, pool.Params().ProactiveHysteresis)
	}
}

// --- 7d growth warning ---

// TestSevenDayGrowthWarning verifies that steep 7d growth emits a warning log
// but does NOT change any parameter (the water-fill time-decay handles routing).
func TestSevenDayGrowthWarning(t *testing.T) {
	pool := makeTestPool()
	f, _ := os.CreateTemp("", "tuner-7d-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)
	defer os.Remove(path + ".1")

	now := time.Now()
	windowStart := now.Add(-24 * time.Hour)
	earlyTs := windowStart.Add(3 * time.Hour).UnixMilli()
	lateTs := windowStart.Add(15 * time.Hour).UnixMilli()

	var events []map[string]any
	for i := 0; i < 100; i++ {
		events = append(events, map[string]any{
			"ts": earlyTs + int64(i*1000), "event": "request",
			"data": map[string]any{"method": "POST"},
		})
	}
	// Early 7d=0.10, late 7d=0.85 → growth = 0.75/12h >> 0.005/hr threshold
	for i := 0; i < 10; i++ {
		events = append(events, map[string]any{
			"ts": earlyTs, "event": "rate_limit_update", "account": "a1",
			"data": map[string]any{"fiveHour": 0.30, "sevenDay": 0.10, "status": "allowed"},
		})
	}
	for i := 0; i < 10; i++ {
		events = append(events, map[string]any{
			"ts": lateTs, "event": "rate_limit_update", "account": "a1",
			"data": map[string]any{"fiveHour": 0.30, "sevenDay": 0.85, "status": "allowed"},
		})
	}

	writeRawEvents(t, path, events)
	l, err := logger.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	oldParams := pool.Params()
	tu := New(pool, l, time.Hour)
	tu.analyze()

	// Parameters must not change — 7d growth is informational only.
	if pool.Params() != oldParams {
		t.Errorf("7d growth warning should not change params: old=%v new=%v", oldParams, pool.Params())
	}
}

// --- Gini coefficient ---

func TestGiniCoefficient(t *testing.T) {
	// Perfect equality: all accounts get same load → G = 0
	eq := map[string]int{"a1": 100, "a2": 100, "a3": 100}
	if g := giniCoefficient(eq); g > 1e-9 {
		t.Errorf("equal distribution: want G=0, got %f", g)
	}

	// Maximum inequality: all load on one account → G = (N-1)/N
	ineq := map[string]int{"a1": 300, "a2": 0, "a3": 0}
	g := giniCoefficient(ineq)
	want := 2.0 / 3.0 // (N-1)/N for N=3
	if g < want-0.01 || g > want+0.01 {
		t.Errorf("maximum inequality: want G≈%.3f, got %f", want, g)
	}

	// Empty map → 0
	if g := giniCoefficient(map[string]int{}); g != 0 {
		t.Errorf("empty map: want 0, got %f", g)
	}
}

// --- Infrastructure tests ---

func TestMeanEmpty(t *testing.T) {
	if m := mean([]float64{}); m != 0 {
		t.Errorf("mean of empty slice should be 0, got %f", m)
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
	if len(tu.History()) != 0 {
		t.Errorf("History should be empty initially, got %d entries", len(tu.History()))
	}
}

func TestTunerHistoryRecorded(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	tu := New(pool, l, time.Hour)

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

	tu := New(pool, l, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tu.Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Run with interval=0 should return immediately")
	}
}

func TestRunTickerFires(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	tu := New(pool, l, time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	tu.Run(ctx)
}

func TestAnalyzeExported(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()
	tu := New(pool, l, time.Hour)
	tu.Analyze() // exits early (< 100 events) — must not panic
}

func TestAnalyzeSkipsMalformedTsEvents(t *testing.T) {
	pool := makeTestPool()
	f, _ := os.CreateTemp("", "tuner-malf-*.jsonl")
	f.Close()
	path := f.Name()
	defer os.Remove(path)

	var events []map[string]any
	events = append(events, map[string]any{"ts": "not-a-number", "event": "request"})
	now := time.Now()
	for i := 0; i < 100; i++ {
		events = append(events, map[string]any{
			"ts":    now.Add(-time.Duration(i) * time.Minute).UnixMilli(),
			"event": "request",
			"data":  map[string]any{"method": "POST"},
		})
	}
	writeRawEvents(t, path, events)

	l, err := logger.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	oldParams := pool.Params()
	tu := New(pool, l, time.Hour)
	tu.analyze()

	if pool.Params() != oldParams {
		t.Error("params should not change with only request events and no malformed ts")
	}
}

func TestAllBlockedWarningDoesNotChangeParams(t *testing.T) {
	pool := makeTestPool()
	l, cleanup := makeTestLogger(t)
	defer cleanup()

	oldParams := pool.Params()
	for i := 0; i < 100; i++ {
		l.Log("request", "a1", map[string]any{"method": "POST"})
	}
	for i := 0; i < 5; i++ {
		l.Log("all_blocked", "", map[string]any{"reason": "all_rejected"})
	}

	tu := New(pool, l, time.Hour)
	tu.analyze()

	if pool.Params() != oldParams {
		t.Error("all_blocked warnings should not change params")
	}
}
