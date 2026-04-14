package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTokenEnsureRefreshes(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Verify the request body has the correct grant_type
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "refresh_token" {
			t.Errorf("expected grant_type=refresh_token, got %q", body["grant_type"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new_access_token",
			"refresh_token": "new_refresh_token",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	// Point the token refresher at our fake server
	orig := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(orig)

	tok := newToken("rt_initial")
	ctx := context.Background()
	got, err := tok.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}
	if got != "new_access_token" {
		t.Errorf("want new_access_token, got %q", got)
	}
	if calls != 1 {
		t.Errorf("want 1 refresh call, got %d", calls)
	}
	// Verify RTR: new refresh token stored
	tok.mu.RLock()
	rt := tok.refreshToken
	tok.mu.RUnlock()
	if rt != "new_refresh_token" {
		t.Errorf("RTR: want new_refresh_token stored, got %q", rt)
	}
	// Second call should not refresh (token still valid)
	_, err = tok.Ensure(ctx)
	if err != nil {
		t.Fatalf("second Ensure returned error: %v", err)
	}
	if calls != 1 {
		t.Errorf("second Ensure should use cached token, got %d calls", calls)
	}
}

func TestRefreshCallbackInvoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok",
			"expires_in":   7200, // 120 minutes
		})
	}))
	defer srv.Close()

	orig := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(orig)

	var cbMins int
	tok := newToken("rt")
	tok.SetRefreshCallback(func(mins int) { cbMins = mins })

	_, err := tok.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if cbMins != 120 {
		t.Errorf("callback should receive expiresIn/60 = 120, got %d", cbMins)
	}
}

func TestIsPermError(t *testing.T) {
	perm := []string{
		"token refresh: status 400: {\"error\":\"invalid_grant\"}",
		"token refresh: status 400: {\"error\":\"invalid_client\"}",
		"token refresh: status 401: unauthorized",
	}
	for _, s := range perm {
		if !isPermError(fmt.Errorf("%s", s)) {
			t.Errorf("expected perm error for %q", s)
		}
	}
	transient := []string{
		"token refresh: http: connection refused",
		"token refresh: status 429: rate limited",
		"token refresh: status 500: internal error",
	}
	for _, s := range transient {
		if isPermError(fmt.Errorf("%s", s)) {
			t.Errorf("expected transient (non-perm) error for %q", s)
		}
	}
}

func TestEnsurePermErrorNoRetry(t *testing.T) {
	// Server returns 400 invalid_grant — should fail immediately (no retry)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token expired"}`))
	}))
	defer srv.Close()

	orig := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(orig)

	tok := newToken("expired_rt")
	_, err := tok.Ensure(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid_grant")
	}
	if !isPermError(err) {
		t.Errorf("expected perm error, got: %v", err)
	}
	if calls != 1 {
		t.Errorf("perm error should not retry: want 1 call, got %d", calls)
	}
}

func TestEnsureTransientErrorRetries(t *testing.T) {
	// Server returns 503 twice, then 200 — should retry and succeed
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"service_unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "recovered_token",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	orig := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(orig)

	tok := newToken("rt")
	got, err := tok.Ensure(context.Background())
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if got != "recovered_token" {
		t.Errorf("want recovered_token, got %s", got)
	}
	if calls != 3 {
		t.Errorf("want 3 calls (2 failures + 1 success), got %d", calls)
	}
}

func TestTokenRTR(t *testing.T) {
	// RTR: if server doesn't return new refresh_token, keep existing one
	tok := &Token{
		refreshToken: "original_rt",
		accessToken:  "old",
		expiresAt:    time.Now().Add(-1 * time.Minute),
	}

	// Simulate refresh result with no new refresh token
	tok.mu.Lock()
	result := struct {
		AccessToken  string
		RefreshToken string
		ExpiresIn    int
	}{
		AccessToken:  "new_access",
		RefreshToken: "", // server didn't return one
		ExpiresIn:    3600,
	}
	tok.accessToken = result.AccessToken
	if result.RefreshToken != "" {
		tok.refreshToken = result.RefreshToken
	}
	tok.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	tok.mu.Unlock()

	if tok.refreshToken != "original_rt" {
		t.Errorf("RTR: should keep original refresh token, got %s", tok.refreshToken)
	}
	if tok.accessToken != "new_access" {
		t.Errorf("access token should be updated, got %s", tok.accessToken)
	}
}

func TestExpiresAt(t *testing.T) {
	tok := &Token{}
	if !tok.ExpiresAt().IsZero() {
		t.Error("ExpiresAt should be zero for unrefreshed token")
	}
	future := time.Now().Add(1 * time.Hour).Truncate(time.Second)
	tok.expiresAt = future
	if tok.ExpiresAt() != future {
		t.Errorf("ExpiresAt should return stored expiry, got %v", tok.ExpiresAt())
	}
}

func TestEnsureContextCancelledDuringRetryWait(t *testing.T) {
	// Server returns 503 (transient) → Ensure waits 1s before retry.
	// Context is cancelled at 200ms → ctx.Done() fires during the wait.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"service_unavailable"}`))
	}))
	defer srv.Close()

	orig := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(orig)

	tok := newToken("rt")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := tok.Ensure(ctx)
	if err == nil {
		t.Fatal("expected error when context cancelled during retry wait")
	}
}

func TestRefreshBadJSONResponse(t *testing.T) {
	// Server returns 200 OK but body is not valid JSON → Ensure must return error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not-valid-json`))
	}))
	defer srv.Close()

	orig := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(orig)

	tok := newToken("rt")
	_, err := tok.Ensure(context.Background())
	if err == nil {
		t.Fatal("expected error for bad JSON response")
	}
}

func TestRefreshEmptyAccessToken(t *testing.T) {
	// Server returns 200 with valid JSON but empty access_token → Ensure must return error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	orig := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(orig)

	tok := newToken("rt")
	_, err := tok.Ensure(context.Background())
	if err == nil {
		t.Fatal("expected error for empty access_token")
	}
}

func TestExpiresIn(t *testing.T) {
	tok := &Token{}
	if s := tok.ExpiresIn(); s != "not refreshed" {
		t.Errorf("want 'not refreshed', got %s", s)
	}

	tok.accessToken = "x"
	// Use a fixed future time so the test isn't sensitive to sub-second execution speed
	tok.expiresAt = time.Now().Add(2*time.Hour + 1*time.Second)
	s := tok.ExpiresIn()
	if s != "2h0m" {
		t.Errorf("want 2h0m, got %s", s)
	}

	tok.expiresAt = time.Now().Add(30*time.Minute + 1*time.Second)
	s = tok.ExpiresIn()
	if s != "30m" {
		t.Errorf("want 30m, got %s", s)
	}

	// Expired: accessToken set but expiresAt is in the past.
	tok.expiresAt = time.Now().Add(-1 * time.Minute)
	s = tok.ExpiresIn()
	if s != "expired" {
		t.Errorf("want 'expired', got %s", s)
	}
}

func TestTokenInvalidate(t *testing.T) {
	// Invalidate clears accessToken and expiresAt so Ensure triggers a fresh refresh.
	tok := &Token{
		accessToken:  "valid_tok",
		refreshToken: "rt",
		expiresAt:    time.Now().Add(2 * time.Hour),
	}
	// Fast path returns the existing token before invalidation.
	got, err := tok.Ensure(context.Background())
	if err != nil || got != "valid_tok" {
		t.Fatalf("pre-invalidate: want valid_tok, got %q err=%v", got, err)
	}

	tok.Invalidate()

	tok.mu.RLock()
	isEmpty := tok.accessToken == "" && tok.expiresAt.IsZero()
	tok.mu.RUnlock()
	if !isEmpty {
		t.Error("Invalidate should clear accessToken and expiresAt")
	}
}

func TestPoolInvalidateTokenUnknown(t *testing.T) {
	// InvalidateToken on an unknown account name is a no-op (no panic).
	accts, _ := ParseAccounts(`[{"name":"a1","refreshToken":"rt1"}]`)
	pool := NewPool(accts)
	pool.InvalidateToken("nonexistent") // must not panic
}

func TestSetTokenEndpoint(t *testing.T) {
	prev := SetTokenEndpoint("http://test.example.com")
	defer SetTokenEndpoint(prev)
	if tokenEndpoint != "http://test.example.com" {
		t.Errorf("SetTokenEndpoint should update tokenEndpoint, got %s", tokenEndpoint)
	}
}

func TestRefreshHTTPClientError(t *testing.T) {
	// Server closes the connection immediately after accepting, causing
	// http.DefaultClient.Do to return an error.
	// Covers refresh's "token refresh: http: ..." error path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("hijack not supported")
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close() // close without sending any response
	}))
	defer srv.Close()

	prev := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(prev)

	// Use a short context so we don't wait through the full retry delay.
	// The http error is recorded on attempt 0; ctx expires before attempt 1.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	tok := newToken("rt")
	_, err := tok.Ensure(ctx)
	if err == nil {
		t.Fatal("expected error when server closes connection immediately")
	}
}

func TestRefreshBodyReadError(t *testing.T) {
	// Server sends headers with Content-Length then closes the connection
	// before sending the full body, causing io.ReadAll to return an error.
	// Covers refresh's "token refresh: read body: ..." error path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("hijack not supported")
			return
		}
		conn, _, _ := hj.Hijack()
		// Send valid HTTP response headers with Content-Length but truncate body.
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{partial"))
		conn.Close() // close before body is fully sent
	}))
	defer srv.Close()

	prev := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(prev)

	// Use a short context so we don't wait through the full retry delay.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	tok := newToken("rt")
	_, err := tok.Ensure(ctx)
	if err == nil {
		t.Fatal("expected error when response body is truncated")
	}
}

func TestEnsureDoubleCheckOnWriteLock(t *testing.T) {
	// Two goroutines race to refresh the same expired token.
	// The write lock is exclusive, so only one goroutine actually calls the server.
	// The second goroutine enters the slow path, blocks on the write lock, then
	// hits the double-check (token already valid) and returns without another refresh.
	calls := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		// Hold the connection briefly so the second goroutine arrives at the write lock
		// before the first finishes and sets the token.
		time.Sleep(60 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "shared_token",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	orig := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(orig)

	tok := newToken("rt") // starts with no access token → both goroutines fail fast path

	start := make(chan struct{})
	type result struct {
		tok string
		err error
	}
	ch := make(chan result, 2)

	for i := 0; i < 2; i++ {
		go func() {
			<-start
			t, err := tok.Ensure(context.Background())
			ch <- result{t, err}
		}()
	}
	close(start) // release both goroutines simultaneously

	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			t.Errorf("Ensure error: %v", r.err)
		}
		if r.tok != "shared_token" {
			t.Errorf("want shared_token, got %q", r.tok)
		}
	}
	// Exactly one server call: the second goroutine hit the double-check.
	mu.Lock()
	c := calls
	mu.Unlock()
	if c != 1 {
		t.Errorf("want exactly 1 refresh call (double-check fires), got %d", c)
	}
}
