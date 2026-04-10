package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"sync"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
)

const pingModel = "claude-haiku-4-5-20251001"

// pingInterval and resetCheckInterval are vars so tests can set them to short
// durations without waiting 10 minutes or 30 seconds.
var (
	pingInterval            = 10 * time.Minute
	resetCheckInterval      = 30 * time.Second
	inactivePingInterval    = 20 * time.Minute
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
}

// NewPinger creates a Pinger.
func NewPinger(pool *accounts.Pool, l *logger.Logger, server *Server) *Pinger {
	return &Pinger{pool: pool, log: l, server: server}
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
	inactiveTicker := time.NewTicker(inactivePingInterval)
	defer ticker.Stop()
	defer resetTicker.Stop()
	defer refreshTicker.Stop()
	defer inactiveTicker.Stop()

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
		case <-inactiveTicker.C:
			// Ping all non-active accounts to keep their rate limit state fresh.
			p.pingInactiveAccounts(ctx)
		}
	}
}

// checkAndPingResets pings accounts whose reset time has passed.
// With the pure water-score model, only "rejected" status hard-blocks an account.
// High utilization alone does not block, but we still ping after resets for
// accurate state updates — especially useful after a rejection clears.
func (p *Pinger) checkAndPingResets(ctx context.Context, scheduled map[string]time.Time) {
	now := time.Now()
	for _, snap := range p.pool.Accounts() {
		rl := snap.RateLimit

		// Only ping accounts that were explicitly rejected by the API.
		if rl.Status != "rejected" {
			continue
		}

		// Check that a reset time has actually passed (account may be near recovery).
		// Prefer the 5h window (shorter) so we recover sooner.
		resetPassed := (!rl.FiveHourReset.IsZero() && !rl.FiveHourReset.After(now)) ||
			(!rl.SevenDayReset.IsZero() && !rl.SevenDayReset.After(now))
		if !resetPassed {
			continue // reset hasn't arrived yet
		}

		// Avoid pinging more than once per minute, but retry after that so a
		// failed or still-rejected ping doesn't permanently block recovery detection.
		if last, ok := scheduled[snap.Name]; ok && time.Since(last) < 60*time.Second {
			continue
		}
		scheduled[snap.Name] = now
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

// pingInactiveAccounts pings all non-active accounts to keep their rate limit
// state fresh. Without this, accounts that haven't been used since startup
// carry stale utilization data, which leads to poor switching decisions.
func (p *Pinger) pingInactiveAccounts(ctx context.Context) {
	for _, snap := range p.pool.Accounts() {
		if snap.IsActive {
			continue
		}
		name := snap.Name
		go p.pingAccount(ctx, name)
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
		p.log.Log("ping", name, map[string]any{
			"fiveHour":  rl.FiveHourUtil,
			"sevenDay":  rl.SevenDayUtil,
			"water":     accounts.WaterScore(rl),
			"status":    rl.Status,
			"latencyMs": time.Since(start).Milliseconds(),
		})
	}
}

// emitPoolSnapshot logs a point-in-time view of all accounts' utilization and
// water scores. Used by the 12h analysis loop to understand pool state over time.
func (p *Pinger) emitPoolSnapshot(trigger string) {
	snaps := p.pool.Accounts()
	accts := make([]map[string]any, len(snaps))
	for i, snap := range snaps {
		rl := snap.RateLimit
		accts[i] = map[string]any{
			"name":     snap.Name,
			"isActive": snap.IsActive,
			"fiveHour": rl.FiveHourUtil,
			"sevenDay": rl.SevenDayUtil,
			"water":    accounts.WaterScore(rl),
			"status":   rl.Status,
		}
	}
	p.log.Log("pool_snapshot", "", map[string]any{
		"trigger":  trigger,
		"accounts": accts,
	})
}
