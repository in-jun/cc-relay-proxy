// Package tuner implements auto-tuning of pool parameters from log analysis.
package tuner

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
)

// TuneHistory records one parameter change.
type TuneHistory struct {
	Ts     int64              `json:"ts"`
	Reason string             `json:"reason"`
	Old    accounts.Params    `json:"old"`
	New    accounts.Params    `json:"new"`
}

// Tuner periodically analyzes logs and adjusts pool parameters.
type Tuner struct {
	pool     *accounts.Pool
	log      *logger.Logger
	interval time.Duration
	history  []TuneHistory
}

// New creates a Tuner. interval is how often to run analysis.
func New(pool *accounts.Pool, l *logger.Logger, interval time.Duration) *Tuner {
	return &Tuner{
		pool:     pool,
		log:      l,
		interval: interval,
	}
}

// History returns a snapshot of recorded parameter changes.
func (t *Tuner) History() []TuneHistory {
	return append([]TuneHistory(nil), t.history...)
}

// Run starts the tuner loop, blocking until ctx is cancelled.
func (t *Tuner) Run(ctx context.Context) {
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

// analyze is the core analysis and adjustment function.
func (t *Tuner) analyze() {
	lines := t.log.ReadLines()
	window := time.Now().Add(-24 * time.Hour)

	// Filter to events within analysis window
	var events []map[string]any
	for _, line := range lines {
		ts, ok := line["ts"].(float64)
		if !ok {
			continue
		}
		if time.UnixMilli(int64(ts)).After(window) {
			events = append(events, line)
		}
	}

	if len(events) < 100 {
		return // not enough data
	}

	params := t.pool.Params()

	var (
		total429          int
		prematureSwitches int
		totalSwitches     int
		allBlockedCount   int
		recoveryTimes     []float64
		windowHours       = 24.0
		// 7d trend: sample early vs late in window
		sevenDayEarly []float64
		sevenDayLate  []float64
		midWindow     = window.Add(12 * time.Hour)
	)

	// Track last switch time per account for recovery measurement
	lastBlocked := map[string]int64{}

	for _, ev := range events {
		event, _ := ev["event"].(string)
		data, _ := ev["data"].(map[string]any)
		ts, _ := ev["ts"].(float64)
		account, _ := ev["account"].(string)
		evTime := time.UnixMilli(int64(ts))

		switch event {
		case "429_received":
			total429++
			action, _ := data["action"].(string)
			if action == "hold" || action == "forward" {
				lastBlocked[account] = int64(ts)
			}

		case "account_switched":
			totalSwitches++
			reason, _ := data["reason"].(string)
			fiveHourBefore, _ := data["fiveHour_before"].(float64)
			// Premature: switched before reaching 90% of the threshold
			earlyThreshold := params.SwitchThreshold5h * 0.90
			if reason == "threshold" && fiveHourBefore < earlyThreshold {
				prematureSwitches++
			}

		case "rate_limit_update":
			// Collect 7d samples for trend analysis
			if sd, ok := data["sevenDay"].(float64); ok {
				if evTime.Before(midWindow) {
					sevenDayEarly = append(sevenDayEarly, sd)
				} else {
					sevenDayLate = append(sevenDayLate, sd)
				}
			}
			// Measure recovery: if this account was previously blocked and now allowed
			status, _ := data["status"].(string)
			if status == "allowed" {
				if blockedAt, ok := lastBlocked[account]; ok {
					recoveryMs := int64(ts) - blockedAt
					recoveryMins := float64(recoveryMs) / 60000.0
					recoveryTimes = append(recoveryTimes, recoveryMins)
					delete(lastBlocked, account)
				}
			}

		case "all_blocked":
			allBlockedCount++
		}
	}

	old := params // already read at top of analyze()
	p := old
	var reasons []string

	// Rule 1: premature switches > 20% of total AND 429 rate < 0.1/hour
	rate429 := float64(total429) / windowHours
	prematureRatio := 0.0
	if totalSwitches > 0 {
		prematureRatio = float64(prematureSwitches) / float64(totalSwitches)
	}
	if prematureRatio > 0.20 && rate429 < 0.1 {
		p.SwitchThreshold5h = clamp(p.SwitchThreshold5h+0.03, 0.50, 0.98)
		reasons = append(reasons, sprintf("premature switch ratio %.0f%% → threshold +0.03", prematureRatio*100))
	}

	// Rule 2: 429 rate > 1.0/hour → lower threshold
	if rate429 > 1.0 {
		p.SwitchThreshold5h = clamp(p.SwitchThreshold5h-0.05, 0.50, 0.98)
		reasons = append(reasons, sprintf("429 rate %.2f/hr → threshold -0.05", rate429))
	}

	// Rule 3: average recovery < 60 minutes → increase weight5h, decrease weight7d
	if len(recoveryTimes) > 0 {
		avg := average(recoveryTimes)
		if avg < 60.0 {
			delta := 0.05
			p.Weight5h = clamp(p.Weight5h+delta, 0.10, 0.99)
			p.Weight7d = clamp(p.Weight7d-delta, 0.01, 0.90)
			// Normalize to sum to 1.0
			sum := p.Weight5h + p.Weight7d
			p.Weight5h /= sum
			p.Weight7d /= sum
			reasons = append(reasons, sprintf("avg recovery %.0fmin < 60min → weight5h↑ weight7d↓", avg))
		}
	}

	// Rule 4: 7d utilization trending up steeply → increase weight7d to prefer
	// accounts with more 7d headroom, and log a warning
	if len(sevenDayEarly) > 0 && len(sevenDayLate) > 0 {
		earlyAvg := average(sevenDayEarly)
		lateAvg := average(sevenDayLate)
		growthPer12h := lateAvg - earlyAvg
		// If 7d grew > 0.05 per 12h, that's ~0.10/day — at this rate cap in ~4 days from 50%
		if growthPer12h > 0.05 {
			delta := 0.10
			p.Weight7d = clamp(p.Weight7d+delta, 0.01, 0.90)
			p.Weight5h = clamp(p.Weight5h-delta, 0.10, 0.99)
			sum := p.Weight5h + p.Weight7d
			p.Weight5h /= sum
			p.Weight7d /= sum
			reasons = append(reasons, sprintf("7d growing %.3f/12h → weight7d↑ to prioritize headroom", growthPer12h))
		}
		if lateAvg > 0.80 {
			t.log.Log("warning", "", map[string]any{
				"code":    "7d_critical",
				"message": sprintf("7d utilization at %.0f%% — limit imminent", lateAvg*100),
				"sevenDay": lateAvg,
			})
		}
	}

	// Rule 5: all-blocked events > 3 → warning
	if allBlockedCount > 3 {
		t.log.Log("warning", "", map[string]any{
			"code":    "insufficient_accounts",
			"message": "allBlocked events > 3 in 24h — consider adding more accounts",
			"count":   allBlockedCount,
		})
	}

	if len(reasons) == 0 || paramsEqual(old, p) {
		return
	}

	reason := join(reasons, "; ")
	t.log.Log("params_updated", "", map[string]any{
		"old":    paramsMap(old),
		"new":    paramsMap(p),
		"reason": reason,
	})

	t.pool.SetParams(p)

	t.history = append(t.history, TuneHistory{
		Ts:     time.Now().UnixMilli(),
		Reason: reason,
		Old:    old,
		New:    p,
	})

	log.Printf("[tuner] params updated: %s", reason)
}

// helpers

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func average(fs []float64) float64 {
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
		a.Weight7d == b.Weight7d
}

func paramsMap(p accounts.Params) map[string]any {
	return map[string]any{
		"switchThreshold5h": p.SwitchThreshold5h,
		"hardBlock7d":       p.HardBlock7d,
		"weight5h":          p.Weight5h,
		"weight7d":          p.Weight7d,
	}
}

// sprintf wraps fmt.Sprintf.
func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// join concatenates strings with sep.
func join(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}
