package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
func (p *Pinger) StartupPing(ctx context.Context) {
	for _, snap := range p.pool.Accounts() {
		go p.pingAccount(ctx, snap.Name)
	}
}

// Run starts the periodic ping loop and the reset-watcher.
func (p *Pinger) Run(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	resetTicker := time.NewTicker(resetCheckInterval)
	defer ticker.Stop()
	defer resetTicker.Stop()

	// Track which accounts we've already scheduled a reset-ping for
	scheduledReset := map[string]time.Time{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pingAccount(ctx, p.pool.ActiveName())
		case <-resetTicker.C:
			// Ping accounts whose 5h reset window has arrived
			p.checkAndPingResets(ctx, scheduledReset)
		}
	}
}

// checkAndPingResets pings blocked accounts whose reset time has passed.
func (p *Pinger) checkAndPingResets(ctx context.Context, scheduled map[string]time.Time) {
	now := time.Now()
	for _, snap := range p.pool.Accounts() {
		rl := snap.RateLimit
		// Only ping if account was blocked (status rejected or high util) and reset has passed
		if rl.Status != "rejected" {
			continue
		}
		reset := rl.FiveHourReset
		if reset.IsZero() || reset.After(now) {
			continue
		}
		// Avoid pinging more than once per reset cycle
		if last, ok := scheduled[snap.Name]; ok && last.Equal(reset) {
			continue
		}
		scheduled[snap.Name] = reset
		log.Printf("[pinger] reset arrived for %s, pinging to confirm availability", snap.Name)
		go p.pingAccount(ctx, snap.Name)
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
			"latencyMs": time.Since(start).Milliseconds(),
		})
	}
}
