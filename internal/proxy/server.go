// Package proxy implements the HTTP reverse proxy and 429-handling logic.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
)

var debugMode = os.Getenv("CC_LOG_LEVEL") == "debug"

// upstreamClient is a shared HTTP client tuned for reverse-proxy use.
// Key design choices:
//   - ForceAttemptHTTP2 preserves HTTP/2 even with a custom DialContext
//   - KeepAlive on the dialer maintains long-lived TCP connections to the API
//   - Large idle pool (single upstream host) avoids TLS handshake on each request
//   - TLSHandshakeTimeout prevents indefinite hangs on bad connections
var upstreamClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 100, // single upstream host — keep many connections warm
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true, // required when DialContext is set
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	},
}

// ssePool is a pool of 32KB byte slices for SSE forwarding.
// Reusing buffers reduces allocations and GC pressure during streaming.
const sseBufSize = 32 * 1024

var ssePool = sync.Pool{
	New: func() any {
		buf := make([]byte, sseBufSize)
		return &buf
	},
}

const (
	AnthropicTarget = "https://api.anthropic.com"
	ProxyHoldMax    = 9*time.Minute + 50*time.Second
)

// Stats holds runtime counters.
type Stats struct {
	TotalRequests atomic.Int64
	TotalSwitches atomic.Int64
	Total429      atomic.Int64
	StartTime     time.Time
}

// Server is the reverse proxy HTTP server.
type Server struct {
	pool    *accounts.Pool
	log     *logger.Logger
	stats   Stats
	target  *url.URL
	pinger  *Pinger
	bgCtx   context.Context // long-lived context for background work (pings etc.)
}

// New creates a Server targeting the real Anthropic API.
func New(pool *accounts.Pool, l *logger.Logger) *Server {
	return NewWithTarget(pool, l, AnthropicTarget)
}

// NewWithTarget creates a Server targeting the given base URL.
// Primarily used in tests to point at a fake upstream.
func NewWithTarget(pool *accounts.Pool, l *logger.Logger, targetURL string) *Server {
	target, _ := url.Parse(targetURL)
	s := &Server{
		pool:   pool,
		log:    l,
		target: target,
		bgCtx:  context.Background(),
	}
	s.stats.StartTime = time.Now()
	s.pinger = NewPinger(pool, l, s)
	return s
}

// Handler returns the main HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/", s.handleProxy)
	return mux
}

// Pinger returns the ping scheduler.
func (s *Server) Pinger() *Pinger {
	return s.pinger
}

// handleProxy is the core reverse proxy handler.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	s.stats.TotalRequests.Add(1)
	start := time.Now()

	// Buffer request body so it can be replayed on retry
	var bodyBuf []byte
	if r.Body != nil {
		var err error
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "proxy: read request body", http.StatusBadGateway)
			return
		}
		r.Body.Close()
	}

	// Select best account (prevAccount captured atomically inside SelectBest)
	accountName, prevAccount, switched, switchReason := s.pool.SelectBest()
	if switched {
		s.stats.TotalSwitches.Add(1)
		prevRL := s.pool.RateLimitFor(prevAccount) // rate limit of old account (reason for switch)
		s.log.Log("account_switched", accountName, map[string]any{
			"from":            prevAccount,
			"to":              accountName,
			"reason":          switchReason,
			"fiveHour_before": prevRL.FiveHourUtil,
		})
		// Ping the previous account in background to measure its recovery speed
		s.pinger.PingAfterSwitch(s.bgCtx, prevAccount)
	}

	maxAttempts := s.pool.Len() + 2
	for attempt := 0; attempt < maxAttempts; attempt++ {
		tok, name, err := s.pool.ActiveTokenWithName(r.Context())
		accountName = name
		if err != nil {
			log.Printf("[proxy] token error: %v", err)
			s.log.Log("error", accountName, map[string]any{"code": "token_error", "msg": err.Error()})
			http.Error(w, "proxy: token unavailable", http.StatusBadGateway)
			return
		}

		resp, rawBody, err := s.sendRequest(r, bodyBuf, tok)
		if err != nil {
			log.Printf("[proxy] upstream error: %v", err)
			s.log.Log("error", accountName, map[string]any{"code": "upstream_error", "msg": err.Error()})
			http.Error(w, "proxy: upstream error", http.StatusBadGateway)
			return
		}

		// Parse rate limit headers
		rl, hasRL := accounts.ParseRateLimitHeaders(resp.Header)
		if hasRL {
			s.pool.UpdateRateLimit(accountName, rl)
			s.log.Log("rate_limit_update", accountName, map[string]any{
				"fiveHour":      rl.FiveHourUtil,
				"sevenDay":      rl.SevenDayUtil,
				"status":        rl.Status,
				"water":         accounts.WaterScore(rl),
				"5hResetInMins": int(time.Until(rl.FiveHourReset).Minutes()),
				"7dResetInHrs":  int(time.Until(rl.SevenDayReset).Hours()),
			})
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			// If 429 but no rate limit headers, force status to "rejected" so SelectBest switches
			if !hasRL {
				existing := s.pool.RateLimitFor(accountName)
				existing.Status = "rejected"
				s.pool.UpdateRateLimit(accountName, existing)
			}
			resp.Body.Close()
			s.stats.Total429.Add(1)

			// Try another account
			prev429Account := accountName
			nextName, _, switched2, reason2 := s.pool.SelectBest()
			if switched2 {
				s.stats.TotalSwitches.Add(1)
				actualRL := s.pool.RateLimitFor(prev429Account)
				s.log.Log("account_switched", nextName, map[string]any{
					"from":            prev429Account,
					"to":              nextName,
					"reason":          reason2,
					"fiveHour_before": actualRL.FiveHourUtil,
				})
				s.log.Log("429_received", prev429Account, map[string]any{
					"action":   "switch",
					"fiveHour": actualRL.FiveHourUtil,
				})
				// Ping previous account in background to measure recovery speed
				s.pinger.PingAfterSwitch(s.bgCtx, prev429Account)
				accountName = nextName
				continue
			}

			// No switch available — all accounts blocked or in caution
			allRejected := s.pool.AllRejected()
			if allRejected {
				soonest := s.pool.SoonestReset()
				waitDur := time.Until(soonest)
				if waitDur > 0 && waitDur <= ProxyHoldMax {
					s.log.Log("all_blocked", "", map[string]any{"reason": "all_rejected", "waitSec": waitDur.Seconds()})
					s.log.Log("429_received", accountName, map[string]any{
						"action":  "hold",
						"waitSec": waitDur.Seconds(),
					})
					select {
					case <-time.After(waitDur):
					case <-r.Context().Done():
						return
					}
					continue
				}
				s.log.Log("all_blocked", "", map[string]any{"reason": "all_rejected_wait_too_long"})
			} else {
				s.log.Log("all_blocked", "", map[string]any{"reason": "all_in_caution"})
			}

			// Forward 429
			fwdRL := s.pool.RateLimitFor(accountName)
			s.log.Log("429_received", accountName, map[string]any{
				"action":   "forward",
				"fiveHour": fwdRL.FiveHourUtil,
			})
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			w.Write(rawBody)

			s.log.Log("request", accountName, map[string]any{
				"method":    r.Method,
				"path":      r.URL.Path,
				"status":    resp.StatusCode,
				"latencyMs": time.Since(start).Milliseconds(),
				"fiveHour":  fwdRL.FiveHourUtil,
				"sevenDay":  fwdRL.SevenDayUtil,
				"water":     accounts.WaterScore(fwdRL),
			})
			return
		}

		// 401: token rejected by API — invalidate and retry with a fresh token.
		// This handles the rare race where a token expires between Ensure and
		// the actual network call, without surfacing the 401 to the client.
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			log.Printf("[proxy] 401 from API for account %s — invalidating token and retrying", accountName)
			s.log.Log("error", accountName, map[string]any{
				"code":    "api_401_token_invalidated",
				"attempt": attempt,
			})
			s.pool.InvalidateToken(accountName)
			continue
		}

		// Success (or non-retryable error) — stream response without buffering
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)

		if isSSE(resp) {
			flusher, canFlush := w.(http.Flusher)
			bufPtr := ssePool.Get().(*[]byte)
			buf := *bufPtr
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					w.Write(buf[:n])
					if canFlush {
						flusher.Flush()
					}
				}
				if readErr != nil {
					break
				}
			}
			ssePool.Put(bufPtr)
		} else {
			io.Copy(w, resp.Body)
		}
		resp.Body.Close()

		// Include current rate-limit state in every request log for analysis.
		reqRL := s.pool.RateLimitFor(accountName)
		s.log.Log("request", accountName, map[string]any{
			"method":    r.Method,
			"path":      r.URL.Path,
			"status":    resp.StatusCode,
			"latencyMs": time.Since(start).Milliseconds(),
			"fiveHour":  reqRL.FiveHourUtil,
			"sevenDay":  reqRL.SevenDayUtil,
			"water":     accounts.WaterScore(reqRL),
		})
		return
	}
	// Loop exhausted without sending a response — all accounts blocked and
	// every attempt either failed or held. Return 502 to avoid silent closure.
	s.log.Log("error", "", map[string]any{"code": "max_attempts_exhausted"})
	http.Error(w, "proxy: all accounts exhausted", http.StatusBadGateway)
}

// sendRequest clones the incoming request, replaces Authorization, and sends to upstream.
func (s *Server) sendRequest(r *http.Request, bodyBuf []byte, tok string) (*http.Response, []byte, error) {
	upstreamURL := *s.target
	upstreamURL.Path = r.URL.Path
	upstreamURL.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), bytes.NewReader(bodyBuf))
	if err != nil {
		return nil, nil, err
	}
	for k, vv := range r.Header {
		for _, v := range vv {
			outReq.Header.Add(k, v)
		}
	}
	// Rewrite auth headers for OAuth — see auth_adapter.go for details.
	adaptClientAuth(outReq, tok)

	if debugMode {
		tokPfx := tok
		if len(tokPfx) > 20 {
			tokPfx = tokPfx[:20] + "..."
		}
		log.Printf("[proxy] → %s %s | auth=Bearer %s | beta=%s",
			r.Method, r.URL.Path, tokPfx,
			outReq.Header.Get("anthropic-beta"))
	}

	resp, err := upstreamClient.Do(outReq)
	if err != nil {
		return nil, nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, body, nil
	}

	return resp, nil, nil
}

// DoUpstreamRequest sends a request to the upstream target with the given token.
// Used by the pinger. Uses s.target so tests with fake upstreams work correctly.
func (s *Server) DoUpstreamRequest(ctx context.Context, tok, path string, body []byte) (*http.Response, error) {
	targetURL := s.target.String() + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	adaptClientAuth(req, tok)
	return upstreamClient.Do(req)
}

func isSSE(resp *http.Response) bool {
	return strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// handleStatus returns a JSON status response.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	uptime := time.Since(s.stats.StartTime)
	acctSnaps := s.pool.Accounts()

	type fiveHourInfo struct {
		Utilization  float64 `json:"utilization"`
		Percent      string  `json:"percent"`
		ResetsInMins int     `json:"resetsInMins"`
	}
	type sevenDayInfo struct {
		Utilization   float64 `json:"utilization"`
		Percent       string  `json:"percent"`
		ResetsInHours int     `json:"resetsInHours"`
	}
	type acctStatus struct {
		Name           string       `json:"name"`
		IsActive       bool         `json:"isActive"`
		Priority       int          `json:"priority"`
		Status         string       `json:"status"`
		Water          float64      `json:"water"`          // effective water (used for selection)
		RawWater       float64      `json:"rawWater"`       // raw water without priority offset
		FiveHour       fiveHourInfo `json:"fiveHour"`
		SevenDay       sevenDayInfo `json:"sevenDay"`
		TokenExpiresIn string       `json:"tokenExpiresIn"`
		LastSeen       string       `json:"lastSeen"`
	}

	accts := make([]acctStatus, len(acctSnaps))
	for i, a := range acctSnaps {
		rl := a.RateLimit
		fiveReset := maxInt(0, int(time.Until(rl.FiveHourReset).Minutes()))
		sevenReset := maxInt(0, int(time.Until(rl.SevenDayReset).Hours()))
		lastSeenStr := "never"
		if !rl.LastSeen.IsZero() {
			lastSeenStr = formatAgo(time.Since(rl.LastSeen))
		}
		accts[i] = acctStatus{
			Name:     a.Name,
			IsActive: a.IsActive,
			Priority: a.Priority,
			Status:   rl.Status,
			Water:    accounts.EffectiveWater(a.Water, a.Priority),
			RawWater: a.Water,
			FiveHour: fiveHourInfo{
				Utilization:  rl.FiveHourUtil,
				Percent:      fmt.Sprintf("%.0f%%", rl.FiveHourUtil*100),
				ResetsInMins: fiveReset,
			},
			SevenDay: sevenDayInfo{
				Utilization:   rl.SevenDayUtil,
				Percent:       fmt.Sprintf("%.0f%%", rl.SevenDayUtil*100),
				ResetsInHours: sevenReset,
			},
			TokenExpiresIn: a.TokenExpiry,
			LastSeen:       lastSeenStr,
		}
	}

	status := map[string]any{
		"active":        s.pool.ActiveName(),
		"uptime":        formatDuration(uptime),
		"totalRequests": s.stats.TotalRequests.Load(),
		"totalSwitches": s.stats.TotalSwitches.Load(),
		"total429":      s.stats.Total429.Load(),
		"accounts":      accts,
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(status)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func formatAgo(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(d.Hours()))
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
