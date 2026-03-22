// Package accounts manages OAuth tokens and rate-limit state for N Anthropic accounts.
package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	ClientID           = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	TokenURL           = "https://platform.claude.com/v1/oauth/token"
	Scopes             = "user:inference user:profile user:sessions:claude_code user:mcp_servers user:file_upload"
	TokenRefreshMargin = 5 * time.Minute
	refreshTimeout     = 15 * time.Second
	refreshMaxRetries  = 3
)

// tokenEndpoint is the URL used by refresh(). Overridable in tests.
var tokenEndpoint = TokenURL

// RefreshCallback is called after a successful token refresh.
// Used by the pool to emit token_refreshed log events.
type RefreshCallback func(expiresInMins int)

// Token holds an account's current OAuth credentials.
type Token struct {
	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	onRefresh    RefreshCallback // optional; called after each successful refresh
}

// newToken creates a Token seeded with a refresh token only.
// AccessToken is empty until the first Ensure call.
func newToken(refreshToken string) *Token {
	return &Token{refreshToken: refreshToken}
}

// SetRefreshCallback attaches a callback invoked after each successful token refresh.
func (t *Token) SetRefreshCallback(cb RefreshCallback) {
	t.mu.Lock()
	t.onRefresh = cb
	t.mu.Unlock()
}

// newTokenSeeded creates a Token pre-loaded with a known-valid access token.
// The proxy will use this token until it is within TokenRefreshMargin of expiry,
// avoiding an immediate refresh call on startup.
func newTokenSeeded(refreshToken, accessToken string, expiresAt time.Time) *Token {
	return &Token{
		refreshToken: refreshToken,
		accessToken:  accessToken,
		expiresAt:    expiresAt,
	}
}

// Ensure returns a valid access token, refreshing if needed.
// It is safe to call concurrently; at most one refresh runs at a time.
func (t *Token) Ensure(ctx context.Context) (string, error) {
	// Fast path: still valid
	t.mu.RLock()
	if t.accessToken != "" && time.Now().Add(TokenRefreshMargin).Before(t.expiresAt) {
		tok := t.accessToken
		t.mu.RUnlock()
		return tok, nil
	}
	t.mu.RUnlock()

	// Slow path: refresh with exponential backoff
	t.mu.Lock()
	defer t.mu.Unlock()
	// Double-check after acquiring write lock
	if t.accessToken != "" && time.Now().Add(TokenRefreshMargin).Before(t.expiresAt) {
		return t.accessToken, nil
	}

	var lastErr error
	for attempt := 0; attempt < refreshMaxRetries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		tok, err := t.refresh(ctx)
		if err == nil {
			return tok, nil
		}
		lastErr = err
		// Don't retry on permanent errors (invalid_grant etc.)
		if isPermError(err) {
			break
		}
	}
	return "", lastErr
}

// refresh performs the OAuth token exchange. Must be called with write lock held.
func (t *Token) refresh(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": t.refreshToken,
		"client_id":     ClientID,
		"scope":         Scopes,
	})

	rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodPost, tokenEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("token refresh: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token refresh: http: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("token refresh: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token refresh: status %d: %s", resp.StatusCode, raw)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"` // seconds
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("token refresh: parse response: %w", err)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("token refresh: empty access_token in response")
	}

	t.accessToken = result.AccessToken
	// RTR: keep existing refresh token if server didn't return a new one
	if result.RefreshToken != "" {
		t.refreshToken = result.RefreshToken
	}
	t.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)

	if t.onRefresh != nil {
		t.onRefresh(result.ExpiresIn / 60)
	}

	return t.accessToken, nil
}

// isPermError reports whether an error from the token endpoint should not be retried.
func isPermError(err error) bool {
	s := err.Error()
	// invalid_grant = refresh token invalid/consumed; don't hammer the endpoint
	return strings.Contains(s, "invalid_grant") ||
		strings.Contains(s, "invalid_client") ||
		strings.Contains(s, "status 401")
}

// ExpiresAt returns when the current access token expires (zero if none).
func (t *Token) ExpiresAt() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.expiresAt
}

// ExpiresIn returns a human-readable time until expiry.
func (t *Token) ExpiresIn() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.accessToken == "" {
		return "not refreshed"
	}
	d := time.Until(t.expiresAt)
	if d <= 0 {
		return "expired"
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
