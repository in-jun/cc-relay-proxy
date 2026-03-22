// Package proxy implements the HTTP reverse proxy and 429-handling logic.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
)

var debugMode = os.Getenv("CC_LOG_LEVEL") == "debug"

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

// TunerHistory is the interface the status endpoint needs from the tuner.
type TunerHistory interface {
	History() []any
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

// New creates a Server.
func New(pool *accounts.Pool, l *logger.Logger) *Server {
	target, _ := url.Parse(AnthropicTarget)
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

	// Select best account
	prevAccount := s.pool.ActiveName()
	accountName, switched, switchReason := s.pool.SelectBest()
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
		go s.pinger.PingAfterSwitch(s.bgCtx, prevAccount)
	}

	maxAttempts := len(s.pool.Accounts()) + 2
	for attempt := 0; attempt < maxAttempts; attempt++ {
		tok, err := s.pool.ActiveToken(r.Context())
		if err != nil {
			log.Printf("[proxy] token error: %v", err)
			s.log.Log("error", accountName, map[string]any{"code": "token_error", "msg": err.Error()})
			http.Error(w, "proxy: token unavailable", http.StatusBadGateway)
			return
		}
		accountName = s.pool.ActiveName()

		resp, rawBody, err := s.sendRequest(r, bodyBuf, tok)
		if err != nil {
			log.Printf("[proxy] upstream error: %v", err)
			http.Error(w, "proxy: upstream error", http.StatusBadGateway)
			return
		}

		// Parse rate limit headers
		rl, hasRL := accounts.ParseRateLimitHeaders(resp.Header)
		if hasRL {
			s.pool.UpdateRateLimit(accountName, rl)
			s.log.Log("rate_limit_update", accountName, map[string]any{
				"fiveHour": rl.FiveHourUtil,
				"sevenDay": rl.SevenDayUtil,
				"status":   rl.Status,
			})
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			s.stats.Total429.Add(1)

			// Try another account
			prev429Account := accountName
			nextName, switched2, reason2 := s.pool.SelectBest()
			if switched2 {
				s.stats.TotalSwitches.Add(1)
				s.log.Log("account_switched", nextName, map[string]any{
					"from":   prev429Account,
					"to":     nextName,
					"reason": reason2,
				})
				s.log.Log("429_received", prev429Account, map[string]any{
					"action":   "switch",
					"fiveHour": rl.FiveHourUtil,
				})
				// Ping previous account in background to measure recovery speed
				go s.pinger.PingAfterSwitch(s.bgCtx, prev429Account)
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
			s.log.Log("429_received", accountName, map[string]any{
				"action":   "forward",
				"fiveHour": rl.FiveHourUtil,
			})
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			w.Write(rawBody)

			s.log.Log("request", accountName, map[string]any{
				"method":    r.Method,
				"path":      r.URL.Path,
				"status":    resp.StatusCode,
				"latencyMs": time.Since(start).Milliseconds(),
			})
			return
		}

		// Success — stream response without buffering
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)

		if isSSE(resp) {
			flusher, canFlush := w.(http.Flusher)
			buf := make([]byte, 4096)
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
		} else {
			io.Copy(w, resp.Body)
		}
		resp.Body.Close()

		s.log.Log("request", accountName, map[string]any{
			"method":    r.Method,
			"path":      r.URL.Path,
			"status":    resp.StatusCode,
			"latencyMs": time.Since(start).Milliseconds(),
		})
		return
	}
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
	outReq.Header.Set("Authorization", "Bearer "+tok)

	if debugMode {
		tokPfx := tok
		if len(tokPfx) > 20 {
			tokPfx = tokPfx[:20] + "..."
		}
		log.Printf("[proxy] → %s %s | auth=Bearer %s | beta=%s",
			r.Method, r.URL.Path, tokPfx,
			outReq.Header.Get("anthropic-beta"))
	}

	resp, err := http.DefaultClient.Do(outReq)
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

// DoUpstreamRequest sends a request to the Anthropic API with the given token.
// Used by the pinger.
func (s *Server) DoUpstreamRequest(ctx context.Context, tok, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AnthropicTarget+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	// OAuth tokens require this beta header — without it the API returns 401
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	return http.DefaultClient.Do(req)
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
	params := s.pool.Params()
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
		Status         string       `json:"status"`
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
			Status:   rl.Status,
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

	type paramsStatus struct {
		SwitchThreshold5h float64 `json:"switchThreshold5h"`
		HardBlock7d       float64 `json:"hardBlock7d"`
		Weight5h          float64 `json:"weight5h"`
		Weight7d          float64 `json:"weight7d"`
	}

	status := map[string]any{
		"active":        s.pool.ActiveName(),
		"uptime":        formatDuration(uptime),
		"totalRequests": s.stats.TotalRequests.Load(),
		"totalSwitches": s.stats.TotalSwitches.Load(),
		"total429":      s.stats.Total429.Load(),
		"params": paramsStatus{
			SwitchThreshold5h: params.SwitchThreshold5h,
			HardBlock7d:       params.HardBlock7d,
			Weight5h:          params.Weight5h,
			Weight7d:          params.Weight7d,
		},
		"accounts":    accts,
		"tuneHistory": s.pinger.TuneHistory(),
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
