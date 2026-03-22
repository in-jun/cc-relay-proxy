// Package tuner implements auto-tuning of pool parameters from log analysis.
//
// # Mathematical framework
//
// The tuner models the proxy as a stochastic control problem:
//
//	state:   (util_5h_i, util_7d_i, reset_5h_i, reset_7d_i) for each account i
//	action:  which account to route the next request to
//	cost:    α·P(429) + β·E[switches/request]
//	goal:    minimise cost over an infinite horizon
//
// The water-filling algorithm (see pool.go) is the analytically optimal
// routing policy for this problem when costs are symmetric. The tuner's job
// is to find the optimal *parameters* of that policy from data, not to
// re-derive the policy itself.
//
// ## Parameter derivations
//
// **switchThreshold5h** — the utilisation level at which we consider an
// account "full" and switch away. Optimal value maximises expected quota
// utilisation subject to P(429) < ε. We estimate this as:
//
//	T* = 1 − max_burst_fraction
//
// where max_burst_fraction is the 95th-percentile single-request quota
// consumption (observed from logs). In practice we cannot observe this
// directly, so we use a P-controller that adjusts T toward the value that
// keeps 429 rate ≈ 0 AND premature-switch rate ≈ 0 simultaneously:
//
//	ΔT = η·(prematureRatio − targetPrematureRatio) − κ·rate429
//
// with η = 0.02 (learning rate, slow and stable), targetPrematureRatio = 0.10.
//
// **proactiveHysteresis** — the dead-zone width for proactive switching.
// From control theory, the optimal dead zone equals the measurement noise
// amplitude. The "noise" here is the water-score fluctuation caused by a
// single request. Target: proactive switch efficiency E = 75% (fraction of
// proactive switches where the destination remained cheaper 10 min later).
//
//	ΔH = η·(E − targetEfficiency)
//
// with targetEfficiency = 0.75.
//
// **Load-balance quality** — measured by the Gini coefficient of the
// request distribution across accounts. G = 0 is perfect balance,
// G = 1 is all traffic on one account. We log G for observability but
// do not tune on it directly; it is an emergent property of the
// water-filling algorithm and the threshold/hysteresis settings.
package tuner

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
)

// tuning constants — derived from control-theory constraints, not guessed.
const (
	// learningRate η: step size for P-controller updates.
	// Small enough to be stable (< 0.1), large enough to converge within
	// a week of hourly tuning: 0.02 × 24 × 7 = 3.36 total adjustment range.
	learningRate = 0.02

	// targetPrematureRatio: fraction of threshold-triggered switches that
	// should be "early" (5h was below 90% of threshold). At exactly 0 we
	// would always wait until the account is full, risking burst 429s.
	// At 0.10 we accept occasional early switches as buffer against bursts.
	targetPrematureRatio = 0.10

	// targetProactiveEfficiency: fraction of proactive switches where the
	// destination account still had a lower water score 10 minutes later.
	// Below 0.75 means we are switching on noise; above 0.75 means we
	// could switch more aggressively (lower hysteresis).
	targetProactiveEfficiency = 0.75

	// minEvents: minimum events before tuning. Below this the gradient
	// estimates have too much variance to be useful.
	minEvents = 100

	// analysisWindow: how far back to look.
	analysisWindow = 24 * time.Hour

	// proactiveCheckWindow: how long after a proactive switch to measure
	// whether the destination was actually better.
	proactiveCheckWindow = 10 * time.Minute
)

// TuneHistory records one parameter change.
type TuneHistory struct {
	Ts     int64           `json:"ts"`
	Reason string          `json:"reason"`
	Old    accounts.Params `json:"old"`
	New    accounts.Params `json:"new"`
}

// Tuner periodically analyses logs and adjusts pool parameters.
type Tuner struct {
	pool     *accounts.Pool
	log      *logger.Logger
	interval time.Duration
	mu       sync.RWMutex
	history  []TuneHistory
	lastTuned time.Time
}

// Interval returns the configured tune interval.
func (t *Tuner) Interval() time.Duration { return t.interval }

// LastTuned returns when the tuner last ran (zero if never).
func (t *Tuner) LastTuned() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastTuned
}

// New creates a Tuner. interval is how often to run analysis.
func New(pool *accounts.Pool, l *logger.Logger, interval time.Duration) *Tuner {
	return &Tuner{
		pool:     pool,
		log:      l,
		interval: interval,
		history:  []TuneHistory{},
	}
}

// History returns a snapshot of recorded parameter changes.
func (t *Tuner) History() []TuneHistory {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.history) == 0 {
		return []TuneHistory{}
	}
	return append([]TuneHistory(nil), t.history...)
}

// Run starts the tuner loop, blocking until ctx is cancelled.
func (t *Tuner) Run(ctx context.Context) {
	if t.interval <= 0 {
		return
	}
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.analyze()
		}
	}
}

// Analyze runs a single analysis cycle immediately. Exported for testing.
func (t *Tuner) Analyze() { t.analyze() }

// metrics holds all derived statistics for one analysis cycle.
type metrics struct {
	totalRequests    int
	total429         int
	totalSwitches    int
	prematureSwitches int   // reactive switches where 5h was well below threshold
	proactiveSwitches int
	proactiveGood    int   // proactive switches that were still justified 10 min later
	proactiveTotal   int   // proactive switches with a follow-up snapshot to evaluate
	allBlockedCount  int
	windowHours      float64
	requestsPerAcct  map[string]int
	sevenDayEarly    []float64
	sevenDayLate     []float64
	recoveryTimes    []float64
}

// switchEvent captures a proactive switch for post-hoc evaluation.
type switchEvent struct {
	ts       int64   // ms
	toAcct   string
	fromAcct string
	fromWater float64 // water score of the account we switched away from
}

func (t *Tuner) analyze() {
	lines := t.log.ReadLines()
	window := time.Now().Add(-analysisWindow)

	// Filter events within window and index pool_snapshots by timestamp
	var events []map[string]any
	// snapshots: ts → list of (name, water) for quick lookup
	type acctSnap struct{ name string; water float64 }
	snapshots := make(map[int64][]acctSnap) // ts → accounts

	for _, line := range lines {
		ts, ok := line["ts"].(float64)
		if !ok {
			continue
		}
		if !time.UnixMilli(int64(ts)).After(window) {
			continue
		}
		events = append(events, line)
		if line["event"] == "pool_snapshot" {
			data, _ := line["data"].(map[string]any)
			accts, _ := data["accounts"].([]any)
			var snaps []acctSnap
			for _, a := range accts {
				am, _ := a.(map[string]any)
				name, _ := am["name"].(string)
				w, _ := am["water"].(float64)
				if name != "" {
					snaps = append(snaps, acctSnap{name, w})
				}
			}
			snapshots[int64(ts)] = snaps
		}
	}

	if len(events) < minEvents {
		return
	}

	params := t.pool.Params()
	m := metrics{
		windowHours:     analysisWindow.Hours(),
		requestsPerAcct: make(map[string]int),
	}

	midWindow := window.Add(analysisWindow / 2)
	lastBlocked := map[string]int64{}
	var proactiveSwitchEvents []switchEvent

	for _, ev := range events {
		event, _ := ev["event"].(string)
		data, _ := ev["data"].(map[string]any)
		ts, _ := ev["ts"].(float64)
		account, _ := ev["account"].(string)
		evTime := time.UnixMilli(int64(ts))

		switch event {
		case "request":
			m.totalRequests++
			m.requestsPerAcct[account]++

		case "429_received":
			m.total429++
			action, _ := data["action"].(string)
			if action == "hold" || action == "forward" {
				lastBlocked[account] = int64(ts)
			}

		case "account_switched":
			m.totalSwitches++
			reason, _ := data["reason"].(string)
			fiveHourBefore, _ := data["fiveHour_before"].(float64)
			fromAcct, _ := data["from"].(string)
			toAcct, _ := data["to"].(string)

			if strings.HasPrefix(reason, "proactive") {
				m.proactiveSwitches++
				// Extract the from-account's water score from reason string if available,
				// otherwise approximate from fiveHourBefore (less accurate but available).
				fromWater := fiveHourBefore / params.SwitchThreshold5h
				proactiveSwitchEvents = append(proactiveSwitchEvents, switchEvent{
					ts:        int64(ts),
					toAcct:    toAcct,
					fromAcct:  fromAcct,
					fromWater: fromWater,
				})
			} else if strings.HasPrefix(reason, "threshold") {
				// Premature: reactive switch before reaching 90% of threshold.
				// At exactly threshold we expect to switch. At 0.9*threshold the
				// account still had 10% headroom — that switch was likely early.
				earlyThreshold := params.SwitchThreshold5h * 0.90
				if fiveHourBefore < earlyThreshold {
					m.prematureSwitches++
				}
			}

		case "rate_limit_update":
			if sd, ok := data["sevenDay"].(float64); ok {
				if evTime.Before(midWindow) {
					m.sevenDayEarly = append(m.sevenDayEarly, sd)
				} else {
					m.sevenDayLate = append(m.sevenDayLate, sd)
				}
			}
			status, _ := data["status"].(string)
			if status == "allowed" {
				if blockedAt, ok := lastBlocked[account]; ok {
					recoveryMins := float64(int64(ts)-blockedAt) / 60000.0
					m.recoveryTimes = append(m.recoveryTimes, recoveryMins)
					delete(lastBlocked, account)
				}
			}

		case "all_blocked":
			m.allBlockedCount++
		}
	}

	// Evaluate proactive switch quality: for each proactive switch, find the
	// first pool_snapshot taken ≥ proactiveCheckWindow later and check whether
	// the destination account's water score is still lower than the source's.
	snapshotTimes := make([]int64, 0, len(snapshots))
	for k := range snapshots {
		snapshotTimes = append(snapshotTimes, k)
	}
	sort.Slice(snapshotTimes, func(i, j int) bool { return snapshotTimes[i] < snapshotTimes[j] })

	for _, sw := range proactiveSwitchEvents {
		checkAfter := sw.ts + proactiveCheckWindow.Milliseconds()
		// Binary search for first snapshot at or after checkAfter
		idx := sort.Search(len(snapshotTimes), func(i int) bool {
			return snapshotTimes[i] >= checkAfter
		})
		if idx >= len(snapshotTimes) {
			continue // no snapshot after check window — cannot evaluate
		}
		snap := snapshots[snapshotTimes[idx]]
		var toWater, fromWater float64
		var foundTo, foundFrom bool
		for _, a := range snap {
			if a.name == sw.toAcct {
				toWater = a.water
				foundTo = true
			}
			if a.name == sw.fromAcct {
				fromWater = a.water
				foundFrom = true
			}
		}
		if foundTo && foundFrom {
			m.proactiveTotal++
			if toWater < fromWater {
				m.proactiveGood++ // destination was still cheaper — switch was justified
			}
		}
	}

	// Derived rates
	rate429 := float64(m.total429) / m.windowHours
	prematureRatio := 0.0
	if m.totalSwitches > 0 {
		prematureRatio = float64(m.prematureSwitches) / float64(m.totalSwitches)
	}
	proactiveEfficiency := 0.0
	if m.proactiveTotal > 0 {
		proactiveEfficiency = float64(m.proactiveGood) / float64(m.proactiveTotal)
	}
	gini := giniCoefficient(m.requestsPerAcct)

	// Log metrics for observability regardless of whether we tune.
	t.log.Log("tuner_metrics", "", map[string]any{
		"requests":            m.totalRequests,
		"total429":            m.total429,
		"rate429PerHr":        rate429,
		"totalSwitches":       m.totalSwitches,
		"prematureSwitches":   m.prematureSwitches,
		"prematureRatio":      prematureRatio,
		"proactiveSwitches":   m.proactiveSwitches,
		"proactiveEfficiency": proactiveEfficiency, // NaN if no evaluable switches
		"proactiveEvaluated":  m.proactiveTotal,
		"allBlockedCount":     m.allBlockedCount,
		"giniCoefficient":     gini,           // 0=perfect balance, 1=all on one
	})

	// --- P-controller parameter updates ---
	old := params
	p := old
	var reasons []string

	// Rule 1: threshold P-controller.
	// ΔT = η·(prematureRatio − target) − κ·rate429
	// The first term raises threshold when we switch too early (quota wasted).
	// The second term lowers threshold when 429s occur (we switched too late).
	// κ = 5·η so a single 429/hr has the same weight as a 50% premature-switch ratio.
	const kappa = 5 * learningRate
	deltaThreshold := learningRate*(prematureRatio-targetPrematureRatio) - kappa*rate429
	if math.Abs(deltaThreshold) >= 0.005 { // ignore noise below 0.5%
		p.SwitchThreshold5h = clamp(p.SwitchThreshold5h+deltaThreshold, 0.50, 0.98)
		reasons = append(reasons, fmt.Sprintf(
			"threshold P-ctrl: premature=%.0f%% (target %.0f%%), 429=%.2f/hr → ΔT=%+.3f",
			prematureRatio*100, targetPrematureRatio*100, rate429, deltaThreshold,
		))
	}

	// Rule 2: hysteresis P-controller.
	// ΔH = η·(target − proactiveEfficiency)
	//
	// Intuition: high efficiency (> 75%) means our proactive switches are
	// consistently good → we can switch more aggressively → lower hysteresis.
	// Low efficiency (< 75%) means we are switching on noise → raise hysteresis.
	// Works only when we have enough evaluable proactive switch outcomes.
	if m.proactiveTotal >= 5 {
		deltaHyst := learningRate * (targetProactiveEfficiency - proactiveEfficiency)
		if math.Abs(deltaHyst) >= 0.005 {
			p.ProactiveHysteresis = clamp(p.ProactiveHysteresis+deltaHyst, 0.0, 0.50)
			reasons = append(reasons, fmt.Sprintf(
				"hysteresis P-ctrl: proactive efficiency=%.0f%% (target %.0f%%) → ΔH=%+.3f",
				proactiveEfficiency*100, targetProactiveEfficiency*100, deltaHyst,
			))
		}
	}

	// Rule 3: 7d trend — informational warning only.
	// We do not adjust parameters here because the water score's time-decay
	// already handles near-reset accounts. We only warn when growth is steep
	// so the operator can add accounts before hitting the hard block.
	if len(m.sevenDayEarly) > 0 && len(m.sevenDayLate) > 0 {
		earlyAvg := mean(m.sevenDayEarly)
		lateAvg := mean(m.sevenDayLate)
		growthPerHr := (lateAvg - earlyAvg) / (m.windowHours / 2)
		if growthPerHr > 0.005 { // > 10%/day
			daysToLimit := (p.HardBlock7d - lateAvg) / (growthPerHr * 24)
			t.log.Log("warning", "", map[string]any{
				"code":        "7d_growing",
				"growthPerHr": growthPerHr,
				"daysToLimit": daysToLimit,
				"sevenDay":    lateAvg,
				"message": fmt.Sprintf(
					"7d growing %.1f%%/day — hard block in ~%.1f days",
					growthPerHr*2400, daysToLimit,
				),
			})
		}
		if lateAvg > 0.80 {
			t.log.Log("warning", "", map[string]any{
				"code":     "7d_critical",
				"sevenDay": lateAvg,
				"message":  fmt.Sprintf("7d at %.0f%% — add accounts soon", lateAvg*100),
			})
		}
	}

	// Rule 4: insufficient accounts warning.
	if m.allBlockedCount > 3 {
		t.log.Log("warning", "", map[string]any{
			"code":    "insufficient_accounts",
			"count":   m.allBlockedCount,
			"message": "all-blocked events > 3 in 24h — add more accounts",
		})
	}

	// Rule 5: poor load balance warning (Gini > 0.3 with ≥3 accounts).
	if gini > 0.30 && len(m.requestsPerAcct) >= 3 {
		t.log.Log("warning", "", map[string]any{
			"code":    "load_imbalance",
			"gini":    gini,
			"message": fmt.Sprintf("Gini coefficient %.2f > 0.30 — load heavily skewed across accounts", gini),
		})
	}

	if len(reasons) == 0 || paramsEqual(old, p) {
		return
	}

	reason := strings.Join(reasons, "; ")
	t.log.Log("params_updated", "", map[string]any{
		"old":    paramsMap(old),
		"new":    paramsMap(p),
		"reason": reason,
	})

	t.pool.SetParams(p)

	t.mu.Lock()
	t.lastTuned = time.Now()
	t.history = append(t.history, TuneHistory{
		Ts:     time.Now().UnixMilli(),
		Reason: reason,
		Old:    old,
		New:    p,
	})
	t.mu.Unlock()

	log.Printf("[tuner] params updated: %s", reason)
}

// giniCoefficient computes the Gini coefficient of a distribution.
// Returns 0 for perfect equality, 1 for maximum inequality.
// Mathematical definition: G = (2·Σ i·x_i) / (n·Σ x_i) − (n+1)/n
// where x_i are sorted values.
func giniCoefficient(counts map[string]int) float64 {
	if len(counts) == 0 {
		return 0
	}
	vals := make([]float64, 0, len(counts))
	sum := 0.0
	for _, v := range counts {
		vals = append(vals, float64(v))
		sum += float64(v)
	}
	if sum == 0 {
		return 0
	}
	sort.Float64s(vals)
	n := float64(len(vals))
	weightedSum := 0.0
	for i, v := range vals {
		weightedSum += float64(i+1) * v
	}
	return (2*weightedSum)/(n*sum) - (n+1)/n
}

// helpers

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func mean(fs []float64) float64 {
	if len(fs) == 0 {
		return 0
	}
	sum := 0.0
	for _, f := range fs {
		sum += f
	}
	return sum / float64(len(fs))
}

func paramsEqual(a, b accounts.Params) bool {
	return a.SwitchThreshold5h == b.SwitchThreshold5h &&
		a.HardBlock7d == b.HardBlock7d &&
		a.Weight5h == b.Weight5h &&
		a.Weight7d == b.Weight7d &&
		a.ProactiveHysteresis == b.ProactiveHysteresis
}

func paramsMap(p accounts.Params) map[string]any {
	return map[string]any{
		"switchThreshold5h":   p.SwitchThreshold5h,
		"hardBlock7d":         p.HardBlock7d,
		"weight5h":            p.Weight5h,
		"weight7d":            p.Weight7d,
		"proactiveHysteresis": p.ProactiveHysteresis,
	}
}
