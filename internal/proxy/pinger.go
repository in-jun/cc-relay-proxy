package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
	"github.com/in-jun/cc-relay-proxy/internal/tuner"
)

const (
	pingModel    = "claude-haiku-4-5-20251001"
	pingInterval = 10 * time.Minute
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

// StartupPing pings all accounts once to establish baseline rate limit state.
func (p *Pinger) StartupPing(ctx context.Context) {
	for _, snap := range p.pool.Accounts() {
		go p.pingAccount(ctx, snap.Name)
	}
}

// Run starts the periodic ping loop.
func (p *Pinger) Run(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pingAccount(ctx, p.pool.ActiveName())
		}
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
