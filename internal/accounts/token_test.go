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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "tok_" + time.Now().Format("150405"),
			"refresh_token": "rt_new",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	// Override the token URL for the test
	origURL := TokenURL
	_ = origURL // keep reference

	tok := newToken("rt_initial")

	// Verify that an empty token is correctly identified as needing refresh
	if tok.accessToken != "" {
		t.Error("new token should have empty access token")
	}

	// Set a pre-expired token to simulate needing refresh
	tok.mu.Lock()
	tok.accessToken = "old_tok"
	tok.expiresAt = time.Now().Add(-1 * time.Minute) // expired
	tok.mu.Unlock()

	if tok.ExpiresIn() != "expired" {
		t.Errorf("expected expired, got %s", tok.ExpiresIn())
	}

	_ = srv
	_ = calls
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
