package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
	"github.com/in-jun/cc-relay-proxy/internal/tuner"
)

const pingModel = "claude-haiku-4-5-20251001"

// pingInterval and resetCheckInterval are vars so tests can set them to short
// durations without waiting 10 minutes or 30 seconds.
var (
	pingInterval       = 10 * time.Minute
	resetCheckInterval = 30 * time.Second
)

var pingBody []byte

func init() {
	pingBody, _ = json.Marshal(map[string]any{
		"model":      pingModel,
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "0"}},
	})
}

// Pinger sends minimal keepalive requests on a schedule.
type Pinger struct {
	pool   *accounts.Pool
	log    *logger.Logger
	server *Server
	tuner  *tuner.Tuner
}

// NewPinger creates a Pinger.
func NewPinger(pool *accounts.Pool, l *logger.Logger, server *Server) *Pinger {
	return &Pinger{pool: pool, log: l, server: server}
}

// SetTuner attaches the tuner so /status can include tune history.
func (p *Pinger) SetTuner(t *tuner.Tuner) {
	p.tuner = t
}

// TuneHistory returns serializable tune history for the status endpoint.
func (p *Pinger) TuneHistory() any {
	if p.tuner == nil {
		return []any{}
	}
	return p.tuner.History()
}

// TuneInterval returns the configured interval, or zero if no tuner.
func (p *Pinger) TuneInterval() time.Duration {
	if p.tuner == nil {
		return 0
	}
	return p.tuner.Interval()
}

// LastTuned returns when the tuner last ran (zero if never).
func (p *Pinger) LastTuned() string {
	if p.tuner == nil {
		return "never"
	}
	lt := p.tuner.LastTuned()
	if lt.IsZero() {
		return "never"
	}
	return formatAgoSimple(time.Since(lt))
}

func formatAgoSimple(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(d.Hours()))
}

// StartupPing pings all accounts once to establish baseline rate limit state.
// It waits for all pings to complete, then emits a pool_snapshot event so the
// tuner and log analysis tools have an accurate starting point.
func (p *Pinger) StartupPing(ctx context.Context) {
	snaps := p.pool.Accounts()
	var wg sync.WaitGroup
	for _, snap := range snaps {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			p.pingAccount(ctx, name)
		}(snap.Name)
	}
	wg.Wait()
	p.emitPoolSnapshot("startup")
}

// tokenRefreshInterval controls how often we proactively refresh tokens for
// all accounts, even those currently blocked by rate limits. This prevents
// idle accounts from having expired tokens when their rate limit window resets.
var tokenRefreshInterval = 30 * time.Minute

// Run starts the periodic ping loop and the reset-watcher.
func (p *Pinger) Run(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	resetTicker := time.NewTicker(resetCheckInterval)
	refreshTicker := time.NewTicker(tokenRefreshInterval)
	defer ticker.Stop()
	defer resetTicker.Stop()
	defer refreshTicker.Stop()

	// Track which accounts we've already scheduled a reset-ping for
	scheduledReset := map[string]time.Time{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pingAccount(ctx, p.pool.ActiveName())
			p.emitPoolSnapshot("periodic")
		case <-resetTicker.C:
			// Ping accounts whose reset window has arrived
			p.checkAndPingResets(ctx, scheduledReset)
		case <-refreshTicker.C:
			// Proactively refresh tokens for all accounts so blocked accounts
			// don't have expired tokens when their rate limit window resets.
			p.refreshAllTokens(ctx)
		}
	}
}

// checkAndPingResets pings blocked accounts whose reset time has passed.
func (p *Pinger) checkAndPingResets(ctx context.Context, scheduled map[string]time.Time) {
	now := time.Now()
	params := p.pool.Params()
	for _, snap := range p.pool.Accounts() {
		rl := snap.RateLimit

		// Account must actually be blocked to warrant a reset-ping.
		blocked := rl.Status == "rejected" ||
			rl.FiveHourUtil >= params.SwitchThreshold5h ||
			rl.SevenDayUtil >= params.HardBlock7d
		if !blocked {
			continue
		}

		// Find a reset time that has already passed.
		// Prefer the 5h window (shorter) so we recover sooner.
		var reset time.Time
		if !rl.FiveHourReset.IsZero() && !rl.FiveHourReset.After(now) {
			reset = rl.FiveHourReset
		} else if !rl.SevenDayReset.IsZero() && !rl.SevenDayReset.After(now) {
			reset = rl.SevenDayReset
		} else {
			continue // reset hasn't arrived yet
		}

		// Avoid pinging more than once per reset cycle.
		if last, ok := scheduled[snap.Name]; ok && last.Equal(reset) {
			continue
		}
		scheduled[snap.Name] = reset
		log.Printf("[pinger] reset arrived for %s, pinging to confirm availability", snap.Name)
		go p.pingAccount(ctx, snap.Name)
	}
}

// refreshAllTokens proactively refreshes tokens for every account so that
// blocked accounts have valid tokens ready when their rate limit resets.
func (p *Pinger) refreshAllTokens(ctx context.Context) {
	for _, snap := range p.pool.Accounts() {
		if snap.IsActive {
			continue // active account token is refreshed on every request
		}
		name := snap.Name
		go func() {
			if _, err := p.pool.TokenFor(ctx, name); err != nil {
				log.Printf("[pinger] proactive token refresh failed for %s: %v", name, err)
				p.log.Log("error", name, map[string]any{"code": "proactive_refresh_failed", "msg": err.Error()})
			}
		}()
	}
}

// PingAfterSwitch pings the previous account (to measure recovery speed).
func (p *Pinger) PingAfterSwitch(ctx context.Context, prevAccount string) {
	go p.pingAccount(ctx, prevAccount)
}

// pingAccount sends a minimal request and records the result.
func (p *Pinger) pingAccount(ctx context.Context, name string) {
	start := time.Now()

	tok, err := p.pool.TokenFor(ctx, name)
	if err != nil {
		log.Printf("[pinger] token error for %s: %v", name, err)
		p.log.Log("error", name, map[string]any{"code": "ping_token_error", "msg": err.Error()})
		return
	}

	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := p.server.DoUpstreamRequest(pingCtx, tok, "/v1/messages", pingBody)
	if err != nil {
		log.Printf("[pinger] ping error for %s: %v", name, err)
		p.log.Log("error", name, map[string]any{"code": "ping_error", "msg": err.Error()})
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	rl, hasRL := accounts.ParseRateLimitHeaders(resp.Header)
	if hasRL {
		p.pool.UpdateRateLimit(name, rl)
		params := p.pool.Params()
		p.log.Log("ping", name, map[string]any{
			"fiveHour":  rl.FiveHourUtil,
			"sevenDay":  rl.SevenDayUtil,
			"water":     accounts.WaterScore(rl, params),
			"status":    rl.Status,
			"latencyMs": time.Since(start).Milliseconds(),
		})
	}
}

// emitPoolSnapshot logs a point-in-time view of all accounts' utilization and
// water scores. Used by the tuner's log analysis to understand pool state over time.
func (p *Pinger) emitPoolSnapshot(trigger string) {
	params := p.pool.Params()
	snaps := p.pool.Accounts()
	accts := make([]map[string]any, len(snaps))
	for i, snap := range snaps {
		rl := snap.RateLimit
		accts[i] = map[string]any{
			"name":     snap.Name,
			"isActive": snap.IsActive,
			"fiveHour": rl.FiveHourUtil,
			"sevenDay": rl.SevenDayUtil,
			"water":    accounts.WaterScore(rl, params),
			"status":   rl.Status,
		}
	}
	p.log.Log("pool_snapshot", "", map[string]any{
		"trigger":  trigger,
		"accounts": accts,
	})
}
