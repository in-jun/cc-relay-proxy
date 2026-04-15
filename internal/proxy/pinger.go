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
	pingInterval         = 10 * time.Minute
	resetCheckInterval   = 30 * time.Second
	inactivePingInterval = 5 * time.Minute
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
// After all pings complete it calls SelectBest so the initial active account
// is chosen by priority and water score rather than by JSON array order.
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

	// Pick the best initial active account based on fresh ping data.
	if name, prev, switched, reason := p.pool.SelectBest(); switched {
		p.log.Log("account_switched", name, map[string]any{
			"from":   prev,
			"to":     name,
			"reason": "startup: " + reason,
		})
		log.Printf("[pinger] startup: switched active from %s to %s (%s)", prev, name, reason)
	}

	p.emitPoolSnapshot("startup")
}

// tokenRefreshInterval controls how often we proactively refresh tokens for
// all non-active accounts, even those currently blocked by rate limits. This
// prevents idle accounts from having expired tokens when their rate limit
// window resets. The active account's token is refreshed on every request.
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
			// Ping all non-active accounts to keep their rate limit state fresh,
			// then check if a proactive switch is warranted.
			p.pingInactiveAccounts(ctx)
			if name, prev, switched, reason := p.pool.SelectBest(); switched {
				p.server.stats.TotalSwitches.Add(1)
				p.log.Log("account_switched", name, map[string]any{
					"from":   prev,
					"to":     name,
					"reason": reason,
				})
				go p.pingAccount(ctx, prev)
			}
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

		// Check that a reset window has passed so recovery is possible.
		resetPassed := (!rl.FiveHourReset.IsZero() && !rl.FiveHourReset.After(now)) ||
			(!rl.SevenDayReset.IsZero() && !rl.SevenDayReset.After(now))
		if !resetPassed {
			continue // reset hasn't arrived yet
		}

		// Rate-limit pings to one per resetCheckInterval. Storing the fire time
		// (not the reset time) means a failed ping (no LastSeen update) is retried
		// after the cooldown without any fragile LastSeen comparison.
		if lastFired, ok := scheduled[snap.Name]; ok && time.Since(lastFired) < resetCheckInterval {
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

// pingInactiveAccounts pings all non-active accounts in parallel and waits for
// all pings to complete before returning. Callers that check SelectBest()
// immediately after need fresh data, so the wait is intentional.
func (p *Pinger) pingInactiveAccounts(ctx context.Context) {
	var wg sync.WaitGroup
	for _, snap := range p.pool.Accounts() {
		if snap.IsActive {
			continue
		}
		wg.Add(1)
		name := snap.Name
		go func() {
			defer wg.Done()
			p.pingAccount(ctx, name)
		}()
	}
	wg.Wait()
}

// PingAfterSwitch pings the previous account (to measure recovery speed).
// Uses context.Background() because the ping must outlive the HTTP request context
// that triggers the switch.
func (p *Pinger) PingAfterSwitch(prevAccount string) {
	go p.pingAccount(context.Background(), prevAccount)
}

// pingAccount sends a minimal request and records the result.
func (p *Pinger) pingAccount(ctx context.Context, name string) {
	start := time.Now()

	tok, err := p.pool.TokenFor(ctx, name)
	if err != nil {
		log.Printf("[pinger] token error for %s: %v", name, err)
		p.log.Log("error", name, map[string]any{"code": "ping_token_error", "msg": err.Error()})
		// Permanent errors (invalid_grant, invalid_client, 401) mean the refresh
		// token is dead. Mark the account rejected so SelectBest stops routing
		// traffic to it.
		if accounts.IsPermTokenError(err) {
			existing := p.pool.RateLimitFor(name)
			existing.Status = "rejected"
			p.pool.UpdateRateLimit(name, existing)
			log.Printf("[pinger] marked %s as rejected (permanent token error)", name)
		}
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
// water scores. Used for offline log analysis to understand pool state over time.
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
