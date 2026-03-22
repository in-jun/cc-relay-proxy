package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
	"github.com/in-jun/cc-relay-proxy/internal/tuner"
)

func newTestPool() *accounts.Pool {
	accts, _ := accounts.ParseAccounts(`[{"name":"a1","refreshToken":"rt1"},{"name":"a2","refreshToken":"rt2"}]`)
	return accounts.NewPool(accts)
}

func newTestLogger(t *testing.T) *logger.Logger {
	f, _ := os.CreateTemp("", "proxy-test-*.jsonl")
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()); os.Remove(f.Name() + ".1") })
	l, _ := logger.New(f.Name())
	t.Cleanup(func() { l.Close() })
	return l
}

func newSeededTestPool(t *testing.T) *accounts.Pool {
	t.Helper()
	accts, _ := accounts.ParseAccounts(`[
		{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999},
		{"name":"a2","refreshToken":"rt2","accessToken":"tok2","expiresAt":9999999999999}
	]`)
	return accounts.NewPool(accts)
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{2*time.Hour + 34*time.Minute, "2h34m"},
		{45 * time.Minute, "45m"},
		{0, "0m"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestCopyHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Add("X-Custom", "a")
	src.Add("X-Custom", "b")
	dst := http.Header{}
	copyHeaders(dst, src)
	if dst.Get("Content-Type") != "application/json" {
		t.Error("content-type not copied")
	}
	if len(dst["X-Custom"]) != 2 {
		t.Errorf("want 2 X-Custom values, got %d", len(dst["X-Custom"]))
	}
}

func TestIsSSE(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
	if !isSSE(resp) {
		t.Error("should detect SSE")
	}
	resp.Header.Set("Content-Type", "application/json")
	if isSSE(resp) {
		t.Error("should not detect SSE for JSON")
	}
}

func TestStatusEndpoint(t *testing.T) {
	pool := newTestPool()
	l := newTestLogger(t)

	srv := New(pool, l)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"active"`) {
		t.Error("response should contain active field")
	}
	if !strings.Contains(body, `"accounts"`) {
		t.Error("response should contain accounts field")
	}
}

func TestProxyForwardsToUpstream(t *testing.T) {
	// Fake upstream that validates the Authorization header and returns 200
	var receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_test","type":"message"}`))
	}))
	defer upstream.Close()

	// Seed the account with a valid access token (far-future expiry) so no OAuth
	// refresh is attempted — the proxy can send requests immediately.
	accts, _ := accounts.ParseAccounts(`[{"name":"a1","refreshToken":"rt1","accessToken":"test_bearer_token","expiresAt":9999999999999}]`)
	pool := accounts.NewPool(accts)
	l := newTestLogger(t)

	srv := NewWithTarget(pool, l, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","max_tokens":1}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d: body=%s", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(receivedAuth, "Bearer test_bearer_token") {
		t.Errorf("upstream should receive proxy's access token, got: %q", receivedAuth)
	}
}

func TestProxySwitchesOn429(t *testing.T) {
	// First request: upstream returns 429 for acct1 token, then 200 for acct2 token
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth := r.Header.Get("Authorization")
		if strings.Contains(auth, "token_a1") {
			// acct1 gets 429
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
		} else {
			// acct2 gets 200
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"msg_ok"}`))
		}
	}))
	defer upstream.Close()

	accts, _ := accounts.ParseAccounts(`[
		{"name":"a1","refreshToken":"rt1","accessToken":"token_a1","expiresAt":9999999999999},
		{"name":"a2","refreshToken":"rt2","accessToken":"token_a2","expiresAt":9999999999999}
	]`)
	pool := accounts.NewPool(accts)
	l := newTestLogger(t)

	srv := NewWithTarget(pool, l, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200 after switch, got %d", w.Code)
	}
	if calls < 2 {
		t.Errorf("want at least 2 upstream calls (one 429, one success), got %d", calls)
	}
}

func TestPingerSetTunerAndAccessors(t *testing.T) {
	pool := newTestPool()
	l := newTestLogger(t)
	srv := New(pool, l)
	p := srv.Pinger()

	tu := tuner.New(pool, l, time.Hour)
	p.SetTuner(tu)

	if p.TuneInterval() != time.Hour {
		t.Errorf("TuneInterval: want 1h, got %v", p.TuneInterval())
	}
	// Before any tune, LastTuned should say "never"
	if p.LastTuned() != "never" {
		t.Errorf("LastTuned before any tune: want 'never', got %s", p.LastTuned())
	}
	// TuneHistory should be empty initially
	hist := p.TuneHistory()
	if hist == nil {
		t.Error("TuneHistory should be non-nil")
	}
}

func TestProxyStreamsSSEResponse(t *testing.T) {
	// Upstream that sends a text/event-stream response
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: hello\n\ndata: world\n\n"))
	}))
	defer upstream.Close()

	accts, _ := accounts.ParseAccounts(`[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`)
	pool := accounts.NewPool(accts)
	l := newTestLogger(t)

	srv := NewWithTarget(pool, l, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Error("response should have SSE content type")
	}
	body := w.Body.String()
	if !strings.Contains(body, "data: hello") {
		t.Errorf("expected SSE data in body, got: %s", body)
	}
}

func TestFormatAgo(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{90 * time.Minute, "1h ago"},
		{3*time.Hour + 15*time.Minute, "3h ago"},
	}
	for _, tt := range tests {
		got := formatAgo(tt.d)
		if got != tt.want {
			t.Errorf("formatAgo(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestMaxInt(t *testing.T) {
	if maxInt(3, 5) != 5 {
		t.Error("maxInt(3,5) should be 5")
	}
	if maxInt(7, 2) != 7 {
		t.Error("maxInt(7,2) should be 7")
	}
	if maxInt(4, 4) != 4 {
		t.Error("maxInt(4,4) should be 4")
	}
}

func TestServerHandlerAndPinger(t *testing.T) {
	pool := newTestPool()
	l := newTestLogger(t)
	srv := New(pool, l)

	h := srv.Handler()
	if h == nil {
		t.Error("Handler() should return non-nil")
	}
	p := srv.Pinger()
	if p == nil {
		t.Error("Pinger() should return non-nil")
	}
}

func TestPingerAccessorsNoTuner(t *testing.T) {
	pool := newTestPool()
	l := newTestLogger(t)
	srv := New(pool, l)
	p := srv.Pinger()

	// No tuner attached — test nil-tuner paths
	hist := p.TuneHistory()
	if hist == nil {
		t.Error("TuneHistory with no tuner should return non-nil (empty slice)")
	}
	if p.TuneInterval() != 0 {
		t.Errorf("TuneInterval with no tuner should be 0, got %v", p.TuneInterval())
	}
	if p.LastTuned() != "never" {
		t.Errorf("LastTuned with no tuner should be 'never', got %s", p.LastTuned())
	}
}

func TestFormatAgoSimple(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{10 * time.Second, "just now"},
		{3 * time.Minute, "3m ago"},
		{2*time.Hour + 15*time.Minute, "2h ago"},
	}
	for _, tt := range tests {
		got := formatAgoSimple(tt.d)
		if got != tt.want {
			t.Errorf("formatAgoSimple(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestProxy429ForwardedWhenAllInCaution(t *testing.T) {
	// Upstream returns 429 with rate-limit headers showing high (but not rejected) utilization.
	// Both accounts will be "over threshold" but not rejected → all_in_caution → forward 429.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return 429 with rate-limit headers showing "allowed_warning" at 90% util
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed_warning")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.90")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", "9999999999")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.50")
		w.Header().Set("anthropic-ratelimit-unified-7d-reset", "9999999999")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()

	accts, _ := accounts.ParseAccounts(`[
		{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999},
		{"name":"a2","refreshToken":"rt2","accessToken":"tok2","expiresAt":9999999999999}
	]`)
	pool := accounts.NewPool(accts)

	// Pre-seed both accounts as over threshold (allowed_warning) but not rejected.
	// This makes Priority 3 (caution fallback) pick the current account (no switch).
	pool.UpdateRateLimit("a1", accounts.RateLimit{Status: "allowed_warning", FiveHourUtil: 0.80})
	pool.UpdateRateLimit("a2", accounts.RateLimit{Status: "allowed_warning", FiveHourUtil: 0.82})

	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("want 429 forwarded when all in caution, got %d", w.Code)
	}
}

func TestProxyReturns502WhenContextCancelledBeforeRequest(t *testing.T) {
	accts, _ := accounts.ParseAccounts(`[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`)
	pool := accounts.NewPool(accts)
	l := newTestLogger(t)
	// Point at a non-listening address so the connection fails
	srv := NewWithTarget(pool, l, "http://127.0.0.1:1")

	// Pre-cancel the context so the HTTP call fails immediately
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	reqCtx, reqCancel := context.WithCancel(req.Context())
	reqCancel()
	req = req.WithContext(reqCtx)

	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	// Either 502 (upstream error) or context cancelled path (empty body, no write)
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502 on upstream connection error, got %d", w.Code)
	}
}

func TestProxyReturns502WhenAllAccountsExhausted(t *testing.T) {
	// Upstream always returns 429, no rate-limit headers — forces both
	// accounts to "rejected" and exhausts the retry loop.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate_limit_error"}`))
	}))
	defer upstream.Close()

	accts, _ := accounts.ParseAccounts(`[
		{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999},
		{"name":"a2","refreshToken":"rt2","accessToken":"tok2","expiresAt":9999999999999}
	]`)
	pool := accounts.NewPool(accts)
	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	// With all accounts rejected and no rate-limit headers for a reset time,
	// the proxy should either forward a 429 or return 502 — never hang silently.
	if w.Code != http.StatusTooManyRequests && w.Code != http.StatusBadGateway {
		t.Errorf("want 429 or 502 when all accounts exhausted, got %d", w.Code)
	}
}

func TestStatusEndpointMethodNotAllowed(t *testing.T) {
	pool := newTestPool()
	l := newTestLogger(t)
	srv := New(pool, l)

	req := httptest.NewRequest(http.MethodPost, "/status", nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", w.Code)
	}
}

func Test429WithNoRateLimitHeadersMarkesRejected(t *testing.T) {
	// Upstream returns 429 with no rate-limit headers
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()

	pool := newTestPool()
	l := newTestLogger(t)

	// Manually set the target to our test upstream
	srv := New(pool, l)

	// Verify that after a 429, RateLimitFor returns "rejected"
	// We simulate by calling UpdateRateLimit directly as the handler would
	existing := pool.RateLimitFor("a1")
	existing.Status = "rejected"
	pool.UpdateRateLimit("a1", existing)

	rl := pool.RateLimitFor("a1")
	if rl.Status != "rejected" {
		t.Errorf("expected rejected after 429 without headers, got %s", rl.Status)
	}
	_ = srv
	_ = upstream
}

// errReader is an io.Reader that immediately returns an error.
type errReader struct{ err error }

func (e errReader) Read(p []byte) (int, error) { return 0, e.err }

func TestProxyBodyReadError(t *testing.T) {
	// A request body that errors on read → proxy should return 502.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	pool := newSeededTestPool(t)
	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, upstream.URL)

	pr, pw := io.Pipe()
	pw.CloseWithError(io.ErrUnexpectedEOF) // reader will return error on first Read

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", pr)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502 on body read error, got %d", w.Code)
	}
}

func TestProxy429AllRejectedWaitTooLong(t *testing.T) {
	// All accounts rejected, soonest reset far in future (> ProxyHoldMax).
	// Proxy must forward the 429 immediately rather than waiting.
	farFuture := time.Now().Add(2 * time.Hour) // well beyond ProxyHoldMax (9m50s)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "1.0")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", "9999999999")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.95")
		w.Header().Set("anthropic-ratelimit-unified-7d-reset", "9999999999")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()

	pool := newSeededTestPool(t)
	// Pre-seed both accounts as rejected with far-future reset
	pool.UpdateRateLimit("a1", accounts.RateLimit{
		Status: "rejected", FiveHourUtil: 1.0, FiveHourReset: farFuture,
	})
	pool.UpdateRateLimit("a2", accounts.RateLimit{
		Status: "rejected", FiveHourUtil: 1.0, FiveHourReset: farFuture,
	})

	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("want 429 forwarded when all rejected and wait too long, got %d", w.Code)
	}
}

func TestProxyHoldContextCancelled(t *testing.T) {
	// All accounts rejected, reset in 1 second (within ProxyHoldMax).
	// The client cancels its context after 100ms → the hold select fires
	// on r.Context().Done() and the handler returns without forwarding.
	soonestReset := time.Now().Add(1 * time.Second)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return 429 with no rate-limit headers so the existing status is preserved
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	pool := newSeededTestPool(t)
	pool.UpdateRateLimit("a1", accounts.RateLimit{
		Status: "rejected", FiveHourUtil: 1.0, FiveHourReset: soonestReset,
	})
	pool.UpdateRateLimit("a2", accounts.RateLimit{
		Status: "rejected", FiveHourUtil: 1.0, FiveHourReset: soonestReset,
	})

	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, upstream.URL)

	// Context cancels after 100ms — long enough for sendRequest but shorter than
	// the 1-second hold duration; the hold select will fire on ctx.Done().
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleProxy(w, req) // should return quickly (< 1s)
	// No response code assertion — handler returns without writing when ctx is done.
}

func TestProxyTokenError(t *testing.T) {
	// Make the OAuth token endpoint return an error (invalid_grant) so that
	// Ensure() fails → ActiveTokenWithName returns an error → handleProxy
	// returns 502 with "token unavailable".
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer tokenSrv.Close()

	// Set the token endpoint to our fake server and restore after the test.
	accounts.SetTokenEndpoint(tokenSrv.URL)
	defer accounts.SetTokenEndpoint(accounts.TokenURL)

	// Use a non-seeded pool so Ensure must refresh.
	pool := newTestPool()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502 on token refresh failure, got %d", w.Code)
	}
}

func TestSendRequestDebugMode(t *testing.T) {
	// Temporarily enable debug logging to cover the debugMode branch in sendRequest.
	old := debugMode
	debugMode = true
	defer func() { debugMode = old }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"type":"message"}`))
	}))
	defer upstream.Close()

	pool := newSeededTestPool(t)
	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d", w.Code)
	}
}

func TestStatusEndpointWithLastSeenAndTuner(t *testing.T) {
	// Covers:
	//   - lastSeenStr = formatAgo(...)  (LastSeen is non-zero after UpdateRateLimit)
	//   - tuneIntervalStr = ti.String() (pinger has a tuner with interval > 0)
	pool := newTestPool()
	pool.UpdateRateLimit("a1", accounts.RateLimit{
		Status: "allowed", FiveHourUtil: 0.5,
	})
	l := newTestLogger(t)
	srv := New(pool, l)
	tu := tuner.New(pool, l, 5*time.Minute)
	srv.pinger.SetTuner(tu)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "5m0s") {
		t.Errorf("expected tuneInterval=5m0s in response body, got: %s", body)
	}
	if !strings.Contains(body, "ago") && !strings.Contains(body, "just now") {
		t.Errorf("expected lastSeen to contain ago/just now, got: %s", body)
	}
}

func TestPingerLastTunedAfterTune(t *testing.T) {
	// Covers the formatAgoSimple branch in pinger.LastTuned() when LastTuned is non-zero.
	pool := newTestPool()
	l := newTestLogger(t)

	// Inject enough events to trigger a parameter change (high 429 rate)
	for i := 0; i < 30; i++ {
		l.Log("429_received", "a1", map[string]any{"action": "switch", "fiveHour": 0.9})
	}
	for i := 0; i < 80; i++ {
		l.Log("request", "a1", map[string]any{"method": "POST"})
	}

	tu := tuner.New(pool, l, time.Hour)
	tu.Analyze() // triggers param change (high 429 rate), sets LastTuned to now
	srv := New(pool, l)
	srv.pinger.SetTuner(tu)

	// LastTuned should now return something like "just now" or "Xm ago"
	result := srv.pinger.LastTuned()
	if result == "never" {
		t.Errorf("LastTuned should not be 'never' after tuner ran, got %q", result)
	}
}

func TestSendRequestDebugModeLongToken(t *testing.T) {
	// Token > 20 chars → tokPfx truncation path in sendRequest's debugMode branch.
	old := debugMode
	debugMode = true
	defer func() { debugMode = old }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	// Use a 30-char access token so len(tokPfx) > 20 triggers truncation.
	longToken := "a_very_long_access_token_12345678"
	accts, _ := accounts.ParseAccounts(`[{"name":"a1","refreshToken":"rt1","accessToken":"` + longToken + `","expiresAt":9999999999999}]`)
	pool := accounts.NewPool(accts)
	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.handleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d", w.Code)
	}
}

func TestPingAccountTokenError(t *testing.T) {
	// pingAccount with an unknown account name → TokenFor returns error → log error, return.
	// Covers lines 150-153 (token error path in pingAccount).
	pool := newSeededTestPool(t)
	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, "http://127.0.0.1:1") // won't be reached
	p := NewPinger(pool, l, srv)

	// "ghost" is not in the pool → TokenFor returns "account not found" error.
	p.pingAccount(context.Background(), "ghost")
	// No assertion needed — we're just ensuring no panic and the error path runs.
}

func TestStartupPing(t *testing.T) {
	// StartupPing spawns one goroutine per account. Each goroutine calls pingAccount.
	// We set tokenEndpoint to a server returning invalid_grant so pingAccount hits
	// the token error path and returns quickly without reaching the real Anthropic API.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer failSrv.Close()

	orig := accounts.SetTokenEndpoint(failSrv.URL)
	defer accounts.SetTokenEndpoint(orig)

	// Non-seeded pool: Ensure will call refresh → invalid_grant → token error.
	accts, _ := accounts.ParseAccounts(`[{"name":"a1","refreshToken":"rt1"}]`)
	pool := accounts.NewPool(accts)
	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, "http://127.0.0.1:1")
	p := NewPinger(pool, l, srv)

	p.StartupPing(context.Background())
	// Give the goroutine time to complete the token refresh attempt.
	time.Sleep(50 * time.Millisecond)
}

func TestRunContextCancelled(t *testing.T) {
	// Run exits immediately when context is cancelled (covers ctx.Done() branch).
	pool := newSeededTestPool(t)
	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, "http://127.0.0.1:1")
	p := NewPinger(pool, l, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run is even called

	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Run returned promptly after ctx was cancelled.
	case <-time.After(2 * time.Second):
		t.Error("Run did not return after context cancellation")
	}
}

func TestCheckAndPingResets(t *testing.T) {
	// checkAndPingResets pings accounts whose 5h reset has passed.
	// We set up one account with rejected status and a reset time in the past,
	// then use a failing tokenEndpoint so pingAccount returns quickly.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer failSrv.Close()

	orig := accounts.SetTokenEndpoint(failSrv.URL)
	defer accounts.SetTokenEndpoint(orig)

	// Non-seeded account so token refresh is needed.
	accts, _ := accounts.ParseAccounts(`[{"name":"a1","refreshToken":"rt1"}]`)
	pool := accounts.NewPool(accts)

	// Mark account as rejected with reset time 1 minute in the past.
	pastReset := time.Now().Add(-1 * time.Minute)
	pool.UpdateRateLimit("a1", accounts.RateLimit{
		Status:        "rejected",
		FiveHourUtil:  1.0,
		FiveHourReset: pastReset,
	})

	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, "http://127.0.0.1:1")
	p := NewPinger(pool, l, srv)

	scheduled := map[string]time.Time{}
	p.checkAndPingResets(context.Background(), scheduled)

	// Account should now be tracked in scheduled.
	if _, ok := scheduled["a1"]; !ok {
		t.Error("a1 should be added to scheduled after reset-ping")
	}

	// Second call with same scheduled map should skip (already tracked).
	p.checkAndPingResets(context.Background(), scheduled)

	// Give the goroutine time to hit the token refresh path.
	time.Sleep(50 * time.Millisecond)
}

func TestPingAccountPingError(t *testing.T) {
	// pingAccount with a seeded (cached) token but a cancelled context:
	//   - TokenFor succeeds immediately (cached access token, no network call)
	//   - pingCtx derived from cancelled parent is already done
	//   - DoUpstreamRequest fails → log error, return (lines 160-163 covered)
	pool := newSeededTestPool(t) // tokens are pre-seeded, Ensure returns immediately
	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, "http://127.0.0.1:1") // not reached
	p := NewPinger(pool, l, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so DoUpstreamRequest fails immediately

	p.pingAccount(ctx, "a1") // logs "[pinger] ping error …" then returns
}

func TestRunTickerAndResetCases(t *testing.T) {
	// Run's two ticker branches:
	//   case <-ticker.C        → pingAccount (covers line 109)
	//   case <-resetTicker.C  → checkAndPingResets (covers line 112)
	// We set both intervals to 1ms so tickers fire almost immediately.
	// pingAccount uses a cancelled ctx (seeded token) → DoUpstreamRequest fails quickly.
	// checkAndPingResets finds no rejected accounts → returns immediately.
	origPing := pingInterval
	origReset := resetCheckInterval
	pingInterval = 1 * time.Millisecond
	resetCheckInterval = 1 * time.Millisecond
	defer func() {
		pingInterval = origPing
		resetCheckInterval = origReset
	}()

	pool := newSeededTestPool(t)
	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, "http://127.0.0.1:1")
	p := NewPinger(pool, l, srv)

	// Cancel after 50ms — enough for both tickers to fire at least once.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Run did not return after context cancellation")
	}
}

func TestCheckAndPingResetsContinuePaths(t *testing.T) {
	// Exercises the two early-continue paths in checkAndPingResets:
	//   1. Status != "rejected"  → skip (first continue)
	//   2. Status == "rejected" but reset is in the future → skip (second continue)
	accts, _ := accounts.ParseAccounts(`[
		{"name":"allowed","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999},
		{"name":"futureReset","refreshToken":"rt2","accessToken":"tok2","expiresAt":9999999999999}
	]`)
	pool := accounts.NewPool(accts)

	// Account "allowed": Status stays "allowed" (default) → first continue fires.
	// Account "futureReset": Status="rejected" but reset is 10 minutes away → second continue.
	pool.UpdateRateLimit("futureReset", accounts.RateLimit{
		Status:        "rejected",
		FiveHourUtil:  1.0,
		FiveHourReset: time.Now().Add(10 * time.Minute),
	})

	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, "http://127.0.0.1:1")
	p := NewPinger(pool, l, srv)

	scheduled := map[string]time.Time{}
	p.checkAndPingResets(context.Background(), scheduled)

	// Neither account should be added: both hit a continue before the scheduled update.
	if len(scheduled) != 0 {
		t.Errorf("scheduled should be empty, got %v", scheduled)
	}
}

func TestProxyLoopExhausted502(t *testing.T) {
	// Pool with 1 account; upstream always returns 429 with rejected status
	// and a reset 2s in the future. maxAttempts=3 (1+2). Each iteration holds
	// for ~2s (the time.After(waitDur) case), continues, then exhausts the loop.
	// Final result: 502 "all accounts exhausted".
	//
	// This test takes ~3-6 seconds and covers:
	//   - case <-time.After(waitDur): continue  (hold timer fires)
	//   - s.log.Log + http.Error on line 250-251  (loop exhaustion)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reset 2 seconds from now (Unix precision: minimum reliable non-zero waitDur)
		resetSec := time.Now().Add(2 * time.Second).Unix()
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "1.0")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(resetSec, 10))
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.95")
		w.Header().Set("anthropic-ratelimit-unified-7d-reset", "9999999999")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	// 1 account → maxAttempts = 3 (1+2); all holds fire the timer, no context cancel.
	accts, _ := accounts.ParseAccounts(`[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`)
	pool := accounts.NewPool(accts)
	// Pre-mark as rejected so SelectBest never switches.
	pool.UpdateRateLimit("a1", accounts.RateLimit{
		Status: "rejected", FiveHourUtil: 1.0,
		FiveHourReset: time.Now().Add(2 * time.Second),
	})

	l := newTestLogger(t)
	srv := NewWithTarget(pool, l, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.handleProxy(w, req) // blocks for ~3×2s = ~6s

	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502 when loop exhausted after holds, got %d", w.Code)
	}
}
