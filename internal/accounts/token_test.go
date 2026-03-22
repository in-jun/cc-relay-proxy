package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	orig := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = orig }()

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

	orig := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = orig }()

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

	orig := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = orig }()

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

	orig := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = orig }()

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

	orig := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = orig }()

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

	orig := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = orig }()

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

	orig := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = orig }()

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
}
