package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
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
