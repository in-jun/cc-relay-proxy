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
	SwitchThreshold5h   float64
	HardBlock7d         float64
	Weight5h            float64 // retained for logging/tuning history; not used in selection
	Weight7d            float64 // retained for logging/tuning history; not used in selection
	ProactiveHysteresis float64 // fraction improvement required to switch proactively (0=disabled)
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
		// 2 accounts: disable proactive switching to avoid flip-flopping.
		return Params{SwitchThreshold5h: 0.75, HardBlock7d: 0.90, Weight5h: 0.80, Weight7d: 0.20, ProactiveHysteresis: 0.0}
	case n <= 4:
		// 3-4 accounts: switch proactively when best alternative is ≥20% better.
		return Params{SwitchThreshold5h: 0.80, HardBlock7d: 0.90, Weight5h: 0.70, Weight7d: 0.30, ProactiveHysteresis: 0.20}
	default:
		// 5+ accounts: finer-grained routing, switch at 15% improvement.
		return Params{SwitchThreshold5h: 0.85, HardBlock7d: 0.90, Weight5h: 0.50, Weight7d: 0.50, ProactiveHysteresis: 0.15}
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

const (
	window5hMins  = 5 * 60   // 300 minutes in a 5h quota window
	window7dHours = 7 * 24   // 168 hours in a 7d quota window
)

// timeDecay returns a [0,1] multiplier representing how much of a reset window
// remains. At reset time the factor is 0 (treat quota as fully recovered); with
// the full window remaining the factor is 1 (no discount). This lets WaterScore
// distinguish an account at 90% utilization that resets in 10 minutes from one
// that won't reset for 4 hours — the former is nearly free and should be preferred.
func timeDecay5h(reset time.Time) float64 {
	if reset.IsZero() {
		return 1.0 // no data → assume worst case (no discount)
	}
	minsLeft := time.Until(reset).Minutes()
	if minsLeft <= 0 {
		return 0.0 // already past reset → fully recovered
	}
	if minsLeft >= window5hMins {
		return 1.0 // full window remaining → no discount
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
// Score = max(effective_5h / threshold, effective_7d / hardBlock) where each
// effective utilization is the raw utilization discounted by the fraction of the
// reset window remaining. An account at 90% 5h utilization that resets in 10
// minutes scores ~3× lower than one with the same utilization that resets in 4
// hours, because the former is nearly free.
//
// Exported so callers outside the package (e.g. the proxy logger) can include
// the score in structured log events without reimplementing the formula.
func WaterScore(rl RateLimit, params Params) float64 {
	s5h := (rl.FiveHourUtil * timeDecay5h(rl.FiveHourReset)) / params.SwitchThreshold5h
	s7d := (rl.SevenDayUtil * timeDecay7d(rl.SevenDayReset)) / params.HardBlock7d
	if s5h > s7d {
		return s5h
	}
	return s7d
}

// WaterScoreFor returns the current water-filling score for the named account.
func (p *Pool) WaterScoreFor(name string) float64 {
	return WaterScore(p.RateLimitFor(name), p.Params())
}

// isBlocked reports whether an account is ineligible for selection.
// An account that is over its static utilization threshold is NOT considered
// blocked if its reset window arrives within the next 2 minutes — SelectBest
// will include it as a candidate so a request can be served the moment it
// resets, rather than waiting for the next ping cycle to notice the change.
const nearResetGrace = 2 * time.Minute

func isBlocked(rl RateLimit, params Params) bool {
	if rl.Status == "rejected" {
		return true
	}
	// 5h over threshold: blocked unless reset is imminent
	if rl.FiveHourUtil >= params.SwitchThreshold5h {
		if rl.FiveHourReset.IsZero() || time.Until(rl.FiveHourReset) > nearResetGrace {
			return true
		}
	}
	// 7d hard-blocked: almost always stuck for hours/days, so no grace period
	if rl.SevenDayUtil >= params.HardBlock7d {
		return true
	}
	return false
}

// SelectBest chooses the best account using a water-filling algorithm with
// proactive switching and hysteresis. Returns the previous account name,
// whether a switch occurred, and the switch reason. Both name and prevName
// are captured atomically under the write lock, eliminating the TOCTOU that
// would arise from a separate ActiveName() call before SelectBest().
//
// Selection priority:
//  1. If current is blocked (rejected / over threshold) → reactive switch to
//     the non-blocked account with the lowest water-fill score.
//  2. If ProactiveHysteresis > 0 and a non-blocked alternative has a water
//     score at least ProactiveHysteresis fraction lower than current → proactive
//     switch to equalize utilization before hitting the threshold.
//  3. Keep current.
//  4. Fallback (all blocked, none rejected) → caution: pick lowest 5h.
//  5. All rejected → stay, caller forwards 429.
func (p *Pool) SelectBest() (name string, prevName string, switched bool, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	params := p.params
	cur := p.accounts[p.active]
	cur.mu.RLock()
	curRL := cur.rateLimit
	cur.mu.RUnlock()

	curBlocked := isBlocked(curRL, params)
	curWater := WaterScore(curRL, params)

	// Find best non-blocked alternative by water-fill score.
	bestIdx := -1
	bestWater := math.MaxFloat64
	for i, a := range p.accounts {
		if i == p.active {
			continue
		}
		a.mu.RLock()
		rl := a.rateLimit
		a.mu.RUnlock()
		if !isBlocked(rl, params) {
			ws := WaterScore(rl, params)
			if ws < bestWater {
				bestWater = ws
				bestIdx = i
			}
		}
	}

	// Priority 1: reactive switch — current is blocked.
	if curBlocked {
		if bestIdx >= 0 {
			from := p.accounts[p.active].Name
			p.active = bestIdx
			to := p.accounts[p.active].Name
			return to, from, true, fmt.Sprintf("threshold: %s exhausted, switching to %s", from, to)
		}

		// Priority 4: all alternatives are also blocked — caution fallback.
		// Pick lowest effective 5h (utilization × time-decay) among non-rejected accounts
		// so that an account near its reset is preferred over one stuck for hours.
		cautionIdx := -1
		bestUtil := math.MaxFloat64
		for i, a := range p.accounts {
			a.mu.RLock()
			rl := a.rateLimit
			a.mu.RUnlock()
			if rl.Status != "rejected" {
				effective := rl.FiveHourUtil * timeDecay5h(rl.FiveHourReset)
				if effective < bestUtil {
					bestUtil = effective
					cautionIdx = i
				}
			}
		}
		if cautionIdx >= 0 && cautionIdx != p.active {
			from := p.accounts[p.active].Name
			p.active = cautionIdx
			to := p.accounts[p.active].Name
			return to, from, true, fmt.Sprintf("caution-fallback: all near-threshold, %s has lowest 5h", to)
		}

		// Priority 5: all rejected — stay, caller forwards 429.
		return cur.Name, cur.Name, false, ""
	}

	// Priority 2: proactive switch — current is fine but a significantly
	// less-loaded account is available.
	if params.ProactiveHysteresis > 0 && bestIdx >= 0 {
		threshold := curWater * (1 - params.ProactiveHysteresis)
		if bestWater < threshold {
			from := p.accounts[p.active].Name
			p.active = bestIdx
			to := p.accounts[p.active].Name
			return to, from, true, fmt.Sprintf(
				"proactive: %s water=%.3f → %s water=%.3f (hysteresis=%.0f%%)",
				from, curWater, to, bestWater, params.ProactiveHysteresis*100,
			)
		}
	}

	// Priority 3: keep current.
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
