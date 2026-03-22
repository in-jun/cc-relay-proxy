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
	AccessToken  string `json:"accessToken,omitempty"`  // optional seed
	ExpiresAt    int64  `json:"expiresAt,omitempty"`    // unix ms, optional
}

// RateLimit holds the last-known rate limit state for an account.
type RateLimit struct {
	Status          string  // "allowed" | "allowed_warning" | "rejected"
	FiveHourUtil    float64 // 0.0–1.0
	FiveHourReset   time.Time
	SevenDayUtil    float64
	SevenDayReset   time.Time
	LastSeen        time.Time
}

// Account is a single Anthropic account with its token and rate limit state.
type Account struct {
	Name      string
	token     *Token
	mu        sync.RWMutex
	rateLimit RateLimit
}

// Params holds tunable algorithm parameters.
type Params struct {
	SwitchThreshold5h float64
	HardBlock7d       float64
	Weight5h          float64
	Weight7d          float64
}

// Pool manages a set of Anthropic accounts and implements the selection algorithm.
type Pool struct {
	mu       sync.RWMutex
	accounts []*Account
	active   int // index into accounts
	params   Params
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

// defaultParams returns initial params scaled by N accounts.
func defaultParams(n int) Params {
	switch {
	case n <= 2:
		return Params{SwitchThreshold5h: 0.75, HardBlock7d: 0.90, Weight5h: 0.80, Weight7d: 0.20}
	case n <= 4:
		return Params{SwitchThreshold5h: 0.80, HardBlock7d: 0.90, Weight5h: 0.70, Weight7d: 0.30}
	default:
		return Params{SwitchThreshold5h: 0.85, HardBlock7d: 0.90, Weight5h: 0.50, Weight7d: 0.50}
	}
}

// NewPool creates a Pool from the given accounts.
func NewPool(accounts []*Account) *Pool {
	return &Pool{
		accounts: accounts,
		active:   0,
		params:   defaultParams(len(accounts)),
	}
}

// Len returns the number of accounts in the pool.
// Unlike Accounts(), this does not allocate a snapshot slice.
func (p *Pool) Len() int { return len(p.accounts) }

// Params returns a copy of current params.
func (p *Pool) Params() Params {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.params
}

// SetParams replaces the tuning params.
func (p *Pool) SetParams(params Params) {
	p.mu.Lock()
	p.params = params
	p.mu.Unlock()
}

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

// score computes the weighted utilization score for an account (lower = preferred).
func score(rl RateLimit, params Params) float64 {
	return params.Weight5h*rl.FiveHourUtil + params.Weight7d*rl.SevenDayUtil
}

func isBlocked(rl RateLimit, params Params) bool {
	return rl.Status == "rejected" ||
		rl.FiveHourUtil >= params.SwitchThreshold5h ||
		rl.SevenDayUtil >= params.HardBlock7d
}

// SelectBest chooses the best account and returns the previous account name,
// whether a switch occurred, and the switch reason. Both name and prevName
// are captured atomically under the write lock, eliminating the TOCTOU that
// would arise from a separate ActiveName() call before SelectBest().
func (p *Pool) SelectBest() (name string, prevName string, switched bool, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	params := p.params
	cur := p.accounts[p.active]
	cur.mu.RLock()
	curRL := cur.rateLimit
	cur.mu.RUnlock()

	// Priority 1: keep current if it's fine
	if !isBlocked(curRL, params) {
		return cur.Name, cur.Name, false, ""
	}

	// Priority 2: switch to best non-blocked account
	bestIdx := -1
	bestScore := math.MaxFloat64
	for i, a := range p.accounts {
		if i == p.active {
			continue
		}
		a.mu.RLock()
		rl := a.rateLimit
		a.mu.RUnlock()
		if !isBlocked(rl, params) {
			s := score(rl, params)
			if s < bestScore {
				bestScore = s
				bestIdx = i
			}
		}
	}
	if bestIdx >= 0 {
		from := p.accounts[p.active].Name
		p.active = bestIdx
		to := p.accounts[p.active].Name
		return to, from, true, fmt.Sprintf("threshold: %s exhausted, switching to %s", from, to)
	}

	// Priority 3: all blocked — pick lowest 5h among non-rejected
	bestIdx = -1
	bestUtil := math.MaxFloat64
	for i, a := range p.accounts {
		a.mu.RLock()
		rl := a.rateLimit
		a.mu.RUnlock()
		if rl.Status != "rejected" && rl.FiveHourUtil < bestUtil {
			bestUtil = rl.FiveHourUtil
			bestIdx = i
		}
	}
	if bestIdx >= 0 && bestIdx != p.active {
		from := p.accounts[p.active].Name
		p.active = bestIdx
		to := p.accounts[p.active].Name
		return to, from, true, fmt.Sprintf("caution-fallback: all near-threshold, %s has lowest 5h", to)
	}

	// Priority 4: all rejected — stay on current (caller will forward 429)
	return cur.Name, cur.Name, false, ""
}

// SoonestReset returns the soonest 5h reset time among all accounts.
func (p *Pool) SoonestReset() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var soonest time.Time
	for _, a := range p.accounts {
		a.mu.RLock()
		t := a.rateLimit.FiveHourReset
		a.mu.RUnlock()
		if !t.IsZero() && (soonest.IsZero() || t.Before(soonest)) {
			soonest = t
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
