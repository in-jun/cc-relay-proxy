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
// Priority controls account preference (1=highest). Accounts with lower
// priority numbers are always preferred over higher ones via a large water
// score offset, forming absolute priority bands that never overlap.
type AccountConfig struct {
	Name         string `json:"name"`
	RefreshToken string `json:"refreshToken"`
	AccessToken  string `json:"accessToken,omitempty"` // optional seed
	ExpiresAt    int64  `json:"expiresAt,omitempty"`   // unix ms, optional
	Priority     int    `json:"priority,omitempty"`    // 1=highest (default 1)
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
	priority  int // 1=highest; 0 treated as 1
	token     *Token
	mu        sync.RWMutex
	rateLimit RateLimit
}

// Pool manages a set of Anthropic accounts and implements the selection algorithm.
type Pool struct {
	mu       sync.RWMutex
	accounts []*Account
	byName   map[string]*Account // index for O(1) name lookups; populated once in NewPool
	active   int                 // index into accounts
	fileMu   sync.Mutex          // serialises all accounts-file writes (PersistAccount/PersistAccounts)
}

// ParseAccountsFile reads and parses a JSON accounts file.
func ParseAccountsFile(path string) ([]*Account, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseAccounts(string(data))
}

// PersistAccounts writes the current token state of all accounts back to path
// so that restarting the proxy doesn't lose rotated refresh tokens.
// Only call this when you know every account's in-memory tokens are valid
// (e.g. initial startup write). For rotation callbacks use PersistAccount.
func (p *Pool) PersistAccounts(path string) error {
	p.mu.RLock()
	cfgs := make([]AccountConfig, len(p.accounts))
	for i, a := range p.accounts {
		// Token fields are protected by token.mu, not a.mu.
		// Account.Name and a.priority are write-once at parse time: safe to read without a lock.
		a.token.mu.RLock()
		rt := a.token.refreshToken
		at := a.token.accessToken
		exp := a.token.expiresAt
		a.token.mu.RUnlock()
		cfgs[i] = AccountConfig{
			Name:         a.Name,
			RefreshToken: rt,
			AccessToken:  at,
			ExpiresAt:    exp.UnixMilli(),
			Priority:     a.priority,
		}
	}
	p.mu.RUnlock()

	p.fileMu.Lock()
	defer p.fileMu.Unlock()
	return writeAccountsFile(path, cfgs)
}

// PersistAccount updates a single account's token in path, reading all other
// accounts from the existing file. This avoids overwriting other accounts with
// potentially stale or consumed in-memory tokens (e.g. after invalid_grant).
//
// fileMu serialises concurrent calls so that two simultaneous rotations don't
// race on the read-modify-write cycle and clobber each other's update.
func (p *Pool) PersistAccount(path, name, refreshToken, accessToken string, expiresAt time.Time) error {
	p.fileMu.Lock()
	defer p.fileMu.Unlock()

	// Read current file so we don't clobber other accounts' tokens.
	raw, err := os.ReadFile(path)
	if err != nil {
		// File might not exist yet; fall back to a full in-memory write.
		// fileMu is already held so call writeAccountsFile directly.
		return p.persistAccountsLocked(path)
	}
	var cfgs []AccountConfig
	if err := json.Unmarshal(raw, &cfgs); err != nil {
		// Corrupt file — overwrite with full in-memory state as last resort.
		return p.persistAccountsLocked(path)
	}

	// Update only the matching account entry; leave all others untouched.
	found := false
	for i, c := range cfgs {
		if c.Name == name {
			cfgs[i].RefreshToken = refreshToken
			cfgs[i].AccessToken = accessToken
			cfgs[i].ExpiresAt = expiresAt.UnixMilli()
			found = true
			break
		}
	}
	if !found {
		// Account not in file yet — append it.
		p.mu.RLock()
		priority := 1
		if a := p.byName[name]; a != nil {
			priority = a.priority
		}
		p.mu.RUnlock()
		cfgs = append(cfgs, AccountConfig{
			Name:         name,
			RefreshToken: refreshToken,
			AccessToken:  accessToken,
			ExpiresAt:    expiresAt.UnixMilli(),
			Priority:     priority,
		})
	}

	return writeAccountsFile(path, cfgs)
}

// persistAccountsLocked is PersistAccounts without acquiring fileMu.
// Must only be called while fileMu is already held.
func (p *Pool) persistAccountsLocked(path string) error {
	p.mu.RLock()
	cfgs := make([]AccountConfig, len(p.accounts))
	for i, a := range p.accounts {
		a.token.mu.RLock()
		rt := a.token.refreshToken
		at := a.token.accessToken
		exp := a.token.expiresAt
		a.token.mu.RUnlock()
		cfgs[i] = AccountConfig{
			Name:         a.Name,
			RefreshToken: rt,
			AccessToken:  at,
			ExpiresAt:    exp.UnixMilli(),
			Priority:     a.priority,
		}
	}
	p.mu.RUnlock()
	return writeAccountsFile(path, cfgs)
}

// writeAccountsFile atomically writes cfgs to path via a temp-file rename.
// The parent directory must be writable (use a directory bind-mount, not a
// file bind-mount, so that os.Rename across the same filesystem succeeds).
func writeAccountsFile(path string, cfgs []AccountConfig) error {
	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cc-accounts-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), path)
}

// WatchRotations registers a rotate callback on every account token so that
// whenever a refresh rotates the credentials, only that account's entry in
// path is updated. Reading the file for all other accounts prevents stale or
// consumed in-memory tokens from overwriting on-disk entries.
// The callback is invoked by Ensure() after it releases the token lock, so
// PersistAccount can safely acquire token read-locks without deadlocking.
func (p *Pool) WatchRotations(path string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		name := a.Name // capture for closure
		a.token.SetRotateCallback(func(rt, at string, exp time.Time) {
			if err := p.PersistAccount(path, name, rt, at, exp); err != nil {
				log.Printf("[accounts] failed to persist rotated token for %s: %v", name, err)
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
			priority:  effectivePriority(c.Priority),
			token:     tok,
			rateLimit: RateLimit{Status: "allowed"},
		}
	}
	return accts, nil
}

// NewPool creates a Pool from the given accounts.
func NewPool(accounts []*Account) *Pool {
	idx := make(map[string]*Account, len(accounts))
	for _, a := range accounts {
		idx[a.Name] = a
	}
	return &Pool{
		accounts: accounts,
		byName:   idx,
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
	acct := p.byName[name]
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
	acct := p.byName[name]
	p.mu.RUnlock()
	if acct != nil {
		acct.token.Invalidate()
	}
}

// UpdateRateLimit records parsed rate limit headers for the named account.
func (p *Pool) UpdateRateLimit(name string, rl RateLimit) {
	p.mu.RLock()
	acct := p.byName[name]
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
	acct := p.byName[name]
	p.mu.RUnlock()
	if acct == nil {
		return RateLimit{}
	}
	acct.mu.RLock()
	rl := acct.rateLimit
	acct.mu.RUnlock()
	return rl
}

// WaterScore computes the selection score for an account (lower = preferred).
//
// Score = 0.7×5h_util + 0.3×7d_util
//
// unknownWater is returned when no rate-limit data has arrived yet (e.g. startup
// ping failed). 1.0 (worst possible) keeps uninitialized accounts at the back of
// the queue until a real ping updates their state.
const unknownWater = 1.0

func WaterScore(rl RateLimit) float64 {
	if rl.FiveHourReset.IsZero() && rl.SevenDayReset.IsZero() {
		return unknownWater
	}
	return 0.7*rl.FiveHourUtil + 0.3*rl.SevenDayUtil
}

// priorityBandSize is the water score offset applied per priority level.
// Water scores are in [0,1], so a gap of 10 creates non-overlapping bands:
//
//	priority 1: effective water [0, 1]
//	priority 2: effective water [10, 11]
//	priority 3: effective water [20, 21]
//
// This makes lower-priority accounts always score worse than higher-priority
// ones regardless of their actual utilization.
const priorityBandSize = 10.0

// effectivePriority normalises a raw priority, treating 0 as 1.
func effectivePriority(p int) int {
	if p <= 0 {
		return 1
	}
	return p
}

// EffectiveWater combines a raw water score with an account's priority to
// produce a comparable score across priority bands. Lower = preferred.
func EffectiveWater(water float64, priority int) float64 {
	return water + float64(effectivePriority(priority)-1)*priorityBandSize
}

// SelectBest chooses the best account using priority-weighted water-filling.
// An account is only hard-blocked when the API explicitly rejects it (status
// "rejected"). Within the same priority band, lower water score wins. Across
// bands, priority 1 always wins over priority 2, etc. — the gap between
// bands (priorityBandSize=10) exceeds the entire water score range [0,1].
//
// Selection priority:
//  1. Reactive switch — current account is rejected → switch to lowest
//     effective-water non-rejected alternative. If no alternative exists
//     (all rejected), stay on current and return switched=false so the
//     caller can forward the 429.
//  2. Proactive switch — a non-rejected alternative has a lower effective
//     water score than current → switch immediately.
//  3. Keep current (current is non-rejected and already best or tied).
func (p *Pool) SelectBest() (name string, prevName string, switched bool, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cur := p.accounts[p.active]
	cur.mu.RLock()
	curRL := cur.rateLimit
	cur.mu.RUnlock()

	curRejected := curRL.Status == "rejected"
	curWater := WaterScore(curRL)
	curEffective := EffectiveWater(curWater, cur.priority)

	// Find best non-rejected alternative by effective water score.
	bestIdx := -1
	bestEffective := math.MaxFloat64
	bestRawWater := math.MaxFloat64
	for i, a := range p.accounts {
		if i == p.active {
			continue
		}
		a.mu.RLock()
		rl := a.rateLimit
		pri := a.priority
		a.mu.RUnlock()
		if rl.Status != "rejected" {
			ws := WaterScore(rl)
			ew := EffectiveWater(ws, pri)
			if ew < bestEffective {
				bestEffective = ew
				bestRawWater = ws
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
			return to, from, true, fmt.Sprintf(
				"reactive: %s rejected, switching to %s (water=%.3f, priority=%d)",
				from, to, bestRawWater, p.accounts[p.active].priority,
			)
		}
		// All rejected — stay, caller forwards 429.
		return cur.Name, cur.Name, false, ""
	}

	// Priority 2: proactive switch — use lowest effective-water account.
	// Skip if current account has no ping data yet (unknownWater); wait for a
	// real ping before making proactive decisions to avoid cascade-switching.
	curHasData := !curRL.FiveHourReset.IsZero() || !curRL.SevenDayReset.IsZero()
	if curHasData && bestIdx >= 0 && bestEffective < curEffective {
		from := p.accounts[p.active].Name
		p.active = bestIdx
		to := p.accounts[p.active].Name
		return to, from, true, fmt.Sprintf(
			"proactive: %s water=%.3f(eff=%.3f) → %s water=%.3f(eff=%.3f)",
			from, curWater, curEffective, to, bestRawWater, bestEffective,
		)
	}

	// Priority 3: keep current.
	return cur.Name, cur.Name, false, ""
}

// SoonestReset returns the soonest reset time that would actually unblock an account.
// The 5h reset is only meaningful if the 7d utilization is below 1.0; otherwise the
// account remains rejected after the 5h window clears and waiting for it wastes the hold.
func (p *Pool) SoonestReset() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var soonest time.Time
	for _, a := range p.accounts {
		a.mu.RLock()
		r5 := a.rateLimit.FiveHourReset
		r7 := a.rateLimit.SevenDayReset
		u7 := a.rateLimit.SevenDayUtil
		a.mu.RUnlock()
		// Only count the 5h reset if 7d is not fully exhausted.
		if u7 < 1.0 && !r5.IsZero() && (soonest.IsZero() || r5.Before(soonest)) {
			soonest = r5
		}
		if !r7.IsZero() && (soonest.IsZero() || r7.Before(soonest)) {
			soonest = r7
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
			Priority:    effectivePriority(a.priority),
			RateLimit:   rl,
			TokenExpiry: a.token.ExpiresIn(),
			Water:       WaterScore(rl),
		}
	}
	return snaps
}

// AccountSnapshot is a point-in-time view of an account.
type AccountSnapshot struct {
	Name        string
	IsActive    bool
	Priority    int
	RateLimit   RateLimit
	TokenExpiry string
	Water       float64 // raw water score; EffectiveWater = Water + (Priority-1)*priorityBandSize
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
