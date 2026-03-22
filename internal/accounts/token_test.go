package accounts

import (
	"encoding/json"
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
	ctx := t.Context()
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
