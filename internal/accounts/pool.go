package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// AccountConfig is the per-account config read from the accounts JSON file.
// accessToken and expiresAt are optional: if provided the proxy uses the
// existing token immediately instead of forcing a refresh on first request.
type AccountConfig struct {
	Name         string `json:"name"`
	RefreshToken string `json:"refreshToken"`
	AccessToken  string `json:"accessToken,omitempty"` // optional seed
	ExpiresAt    int64  `json:"expiresAt,omitempty"`   // unix ms, optional
}

// RateLimit holds the last-known rate limit state for an account.
type RateLimit struct {
	Status        string  // "allowed" | "allowed_warning" | "rejected"
	FiveHourUtil  float64 // 0.0–1.0
	FiveHourReset time.Time
	SevenDayUtil  float64
	SevenDayReset time.Time
	LastSeen      time.Time
}

// Account is a single Anthropic account with its token and rate limit state.
type Account struct {
	Name      string
	token     *Token
	mu        sync.RWMutex
	rateLimit RateLimit
}

// ProactiveHysteresis is the minimum fractional improvement in water score
// required to switch away from the current account proactively. At 0.10, a
// candidate must be at least 10% better to trigger a switch, preventing
// flip-flopping between accounts with nearly equal scores.
const ProactiveHysteresis = 0.10

// Pool manages a set of Anthropic accounts and implements the selection algorithm.
type Pool struct {
	mu       sync.RWMutex
	accounts []*Account
	active   int // index into accounts
}

// ParseAccountsFile reads and parses a JSON accounts file.
func ParseAccountsFile(path string) ([]*Account, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseAccounts(string(data))
}

// PersistAccounts atomically writes the current token state of all accounts
// back to path so that restarting the proxy doesn't lose rotated refresh tokens.
func (p *Pool) PersistAccounts(path string) error {
	p.mu.RLock()
	cfgs := make([]AccountConfig, len(p.accounts))
	for i, a := range p.accounts {
		a.mu.RLock()
		rt := a.token.refreshToken
		at := a.token.accessToken
		exp := a.token.expiresAt
		a.mu.RUnlock()
		cfgs[i] = AccountConfig{
			Name:         a.Name,
			RefreshToken: rt,
			AccessToken:  at,
			ExpiresAt:    exp.UnixMilli(),
		}
	}
	p.mu.RUnlock()

	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: temp file → rename
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cc-accounts-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), path)
}

// WatchRotations registers a rotate callback on every account token so that
// whenever a refresh rotates the credentials, they are persisted to path.
func (p *Pool) WatchRotations(path string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		a := a // capture
		a.token.SetRotateCallback(func(_, _ string, _ time.Time) {
			if err := p.PersistAccounts(path); err != nil {
				log.Printf("[accounts] failed to persist rotated tokens: %v", err)
			}
		})
	}
}

// ParseAccounts parses a JSON accounts array from a string.
func ParseAccounts(data string) ([]*Account, error) {
	var cfgs []AccountConfig
	if err := json.Unmarshal([]byte(data), &cfgs); err != nil {
		return nil, fmt.Errorf("parse accounts: %w", err)
	}
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("accounts: at least one account required")
	}
	accts := make([]*Account, len(cfgs))
	for i, c := range cfgs {
		if c.Name == "" {
			c.Name = fmt.Sprintf("acct%d", i+1)
		}
		if c.RefreshToken == "" {
			return nil, fmt.Errorf("accounts[%d] (%s): refreshToken is required", i, c.Name)
		}
		var tok *Token
		if c.AccessToken != "" && c.ExpiresAt > 0 {
			tok = newTokenSeeded(c.RefreshToken, c.AccessToken, time.UnixMilli(c.ExpiresAt))
		} else {
			tok = newToken(c.RefreshToken)
		}
		accts[i] = &Account{
			Name:      c.Name,
			token:     tok,
			rateLimit: RateLimit{Status: "allowed"},
		}
	}
	return accts, nil
}

// NewPool creates a Pool from the given accounts.
func NewPool(accounts []*Account) *Pool {
	return &Pool{
		accounts: accounts,
		active:   0,
	}
}

// Len returns the number of accounts in the pool.
func (p *Pool) Len() int { return len(p.accounts) }

// ActiveName returns the name of the currently active account.
func (p *Pool) ActiveName() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accounts[p.active].Name
}

// ActiveToken returns a valid access token for the current active account.
func (p *Pool) ActiveToken(ctx context.Context) (string, error) {
	tok, _, err := p.ActiveTokenWithName(ctx)
	return tok, err
}

// ActiveTokenWithName returns both a valid access token and the account name,
// capturing them together under the same read lock to prevent mismatches if
// another goroutine switches the active account between separate calls.
func (p *Pool) ActiveTokenWithName(ctx context.Context) (token, name string, err error) {
	p.mu.RLock()
	acct := p.accounts[p.active]
	name = acct.Name
	p.mu.RUnlock()
	// Ensure is called outside the lock (it may block during token refresh).
	// The account pointer is stable — accounts are never removed or replaced.
	token, err = acct.token.Ensure(ctx)
	return
}

// TokenFor returns a valid access token for a named account.
func (p *Pool) TokenFor(ctx context.Context, name string) (string, error) {
	p.mu.RLock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.RUnlock()
	if acct == nil {
		return "", fmt.Errorf("account %q not found", name)
	}
	return acct.token.Ensure(ctx)
}

// InvalidateToken clears the cached access token for the named account,
// forcing the next call to TokenFor/ActiveToken to perform a fresh refresh.
// Used when the API returns 401 to recover without dropping the request.
func (p *Pool) InvalidateToken(name string) {
	p.mu.RLock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.RUnlock()
	if acct != nil {
		acct.token.Invalidate()
	}
}

// UpdateRateLimit records parsed rate limit headers for the named account.
func (p *Pool) UpdateRateLimit(name string, rl RateLimit) {
	p.mu.RLock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.RUnlock()
	if acct == nil {
		return
	}
	acct.mu.Lock()
	acct.rateLimit = rl
	acct.rateLimit.LastSeen = time.Now()
	acct.mu.Unlock()
}

// RateLimitFor returns the current rate limit state for the named account.
func (p *Pool) RateLimitFor(name string) RateLimit {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.RLock()
			rl := a.rateLimit
			a.mu.RUnlock()
			return rl
		}
	}
	return RateLimit{}
}

const (
	window5hMins  = 5 * 60 // 300 minutes in a 5h quota window
	window7dHours = 7 * 24 // 168 hours in a 7d quota window
)

// timeDecay5h returns a [0,1] multiplier: 0 at reset (fully recovered), 1 at
// full window remaining. Accounts near their reset score lower and float to
// the top of the candidate pool.
func timeDecay5h(reset time.Time) float64 {
	if reset.IsZero() {
		return 1.0
	}
	minsLeft := time.Until(reset).Minutes()
	if minsLeft <= 0 {
		return 0.0
	}
	if minsLeft >= window5hMins {
		return 1.0
	}
	return minsLeft / window5hMins
}

func timeDecay7d(reset time.Time) float64 {
	if reset.IsZero() {
		return 1.0
	}
	hoursLeft := time.Until(reset).Hours()
	if hoursLeft <= 0 {
		return 0.0
	}
	if hoursLeft >= window7dHours {
		return 1.0
	}
	return hoursLeft / window7dHours
}

// WaterScore computes the water-filling score for an account (lower = preferred).
//
// Score = max(5h_util × decay5h, 7d_util × decay7d)
//
// Both dimensions are in [0,1]. The max ensures the worse dimension dominates.
// Time-decay naturally de-prioritizes accounts far from their reset window and
// promotes accounts that are near reset (effectively free capacity soon).
// unknownWater is the water score returned when no rate-limit data has arrived
// yet (e.g. startup ping failed). Returning 0 would make the account look like
// the best choice and trigger a proactive switch onto a potentially broken
// account. 0.5 keeps it neutral until a real ping updates the state.
const unknownWater = 0.5

func WaterScore(rl RateLimit) float64 {
	// No rate-limit data yet — treat as neutral, not "best available".
	if rl.FiveHourReset.IsZero() && rl.SevenDayReset.IsZero() {
		return unknownWater
	}
	s5h := rl.FiveHourUtil * timeDecay5h(rl.FiveHourReset)
	s7d := rl.SevenDayUtil * timeDecay7d(rl.SevenDayReset)
	if s5h > s7d {
		return s5h
	}
	return s7d
}

// WaterScoreFor returns the current water-filling score for the named account.
func (p *Pool) WaterScoreFor(name string) float64 {
	return WaterScore(p.RateLimitFor(name))
}

// SelectBest chooses the best account using a pure water-filling algorithm.
// An account is only hard-blocked when the API explicitly rejects it (status
// "rejected"). Utilization levels are handled by the water score — high
// utilization raises the score, naturally routing requests to less-loaded
// accounts without needing static thresholds.
//
// Selection priority:
//  1. Reactive switch — current account is rejected → switch to lowest water non-rejected.
//  2. Proactive switch — a non-rejected alternative scores at least proactiveHysteresis
//     fraction lower than current → switch to equalize load before hitting limits.
//  3. Keep current.
//  4. All rejected → stay, caller forwards 429.
func (p *Pool) SelectBest() (name string, prevName string, switched bool, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cur := p.accounts[p.active]
	cur.mu.RLock()
	curRL := cur.rateLimit
	cur.mu.RUnlock()

	curRejected := curRL.Status == "rejected"
	curWater := WaterScore(curRL)

	// Find best non-rejected alternative by water-fill score.
	bestIdx := -1
	bestWater := math.MaxFloat64
	for i, a := range p.accounts {
		if i == p.active {
			continue
		}
		a.mu.RLock()
		rl := a.rateLimit
		a.mu.RUnlock()
		if rl.Status != "rejected" {
			ws := WaterScore(rl)
			if ws < bestWater {
				bestWater = ws
				bestIdx = i
			}
		}
	}

	// Priority 1: reactive switch — current account is rejected by the API.
	if curRejected {
		if bestIdx >= 0 {
			from := p.accounts[p.active].Name
			p.active = bestIdx
			to := p.accounts[p.active].Name
			return to, from, true, fmt.Sprintf("reactive: %s rejected, switching to %s (water=%.3f)", from, to, bestWater)
		}
		// All rejected — stay, caller forwards 429.
		return cur.Name, cur.Name, false, ""
	}

	// Priority 2: proactive switch — a significantly less-loaded account is available.
	if bestIdx >= 0 && curWater > 0 {
		threshold := curWater * (1 - ProactiveHysteresis)
		if bestWater < threshold {
			from := p.accounts[p.active].Name
			p.active = bestIdx
			to := p.accounts[p.active].Name
			return to, from, true, fmt.Sprintf(
				"proactive: %s water=%.3f → %s water=%.3f",
				from, curWater, to, bestWater,
			)
		}
	}

	// Priority 3: keep current.
	return cur.Name, cur.Name, false, ""
}

// SoonestReset returns the soonest reset time (5h or 7d) among all accounts.
func (p *Pool) SoonestReset() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var soonest time.Time
	for _, a := range p.accounts {
		a.mu.RLock()
		r5 := a.rateLimit.FiveHourReset
		r7 := a.rateLimit.SevenDayReset
		a.mu.RUnlock()
		for _, t := range []time.Time{r5, r7} {
			if !t.IsZero() && (soonest.IsZero() || t.Before(soonest)) {
				soonest = t
			}
		}
	}
	return soonest
}

// AllRejected reports whether every account has status == "rejected".
func (p *Pool) AllRejected() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		a.mu.RLock()
		s := a.rateLimit.Status
		a.mu.RUnlock()
		if s != "rejected" {
			return false
		}
	}
	return true
}

// Accounts returns a snapshot of all accounts for status reporting.
func (p *Pool) Accounts() []AccountSnapshot {
	p.mu.RLock()
	activeIdx := p.active
	p.mu.RUnlock()

	snaps := make([]AccountSnapshot, len(p.accounts))
	for i, a := range p.accounts {
		a.mu.RLock()
		rl := a.rateLimit
		a.mu.RUnlock()
		snaps[i] = AccountSnapshot{
			Name:        a.Name,
			IsActive:    i == activeIdx,
			RateLimit:   rl,
			TokenExpiry: a.token.ExpiresIn(),
		}
	}
	return snaps
}

// AccountSnapshot is a point-in-time view of an account.
type AccountSnapshot struct {
	Name        string
	IsActive    bool
	RateLimit   RateLimit
	TokenExpiry string
}

// SetRefreshCallback attaches cb to every account token so callers can log
// token_refreshed events. The callback receives (accountName, expiresInMins).
func (p *Pool) SetRefreshCallback(cb func(name string, expiresInMins int)) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		name := a.Name // capture for closure
		a.token.SetRefreshCallback(func(mins int) { cb(name, mins) })
	}
}

// ParseRateLimitHeaders extracts rate limit info from an HTTP response header.
func ParseRateLimitHeaders(h http.Header) (RateLimit, bool) {
	status := h.Get("anthropic-ratelimit-unified-status")
	if status == "" {
		return RateLimit{}, false
	}

	rl := RateLimit{Status: status}

	if v := h.Get("anthropic-ratelimit-unified-5h-utilization"); v != "" {
		rl.FiveHourUtil, _ = strconv.ParseFloat(v, 64)
	}
	if v := h.Get("anthropic-ratelimit-unified-5h-reset"); v != "" {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.FiveHourReset = time.Unix(ts, 0)
		}
	}
	if v := h.Get("anthropic-ratelimit-unified-7d-utilization"); v != "" {
		rl.SevenDayUtil, _ = strconv.ParseFloat(v, 64)
	}
	if v := h.Get("anthropic-ratelimit-unified-7d-reset"); v != "" {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.SevenDayReset = time.Unix(ts, 0)
		}
	}

	return rl, true
}
