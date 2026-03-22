package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func makePool(n int) *Pool {
	accts := make([]*Account, n)
	for i := range accts {
		accts[i] = &Account{
			Name:  "acct" + string(rune('0'+i+1)),
			token: newToken("rt_" + string(rune('a'+i))),
			rateLimit: RateLimit{Status: "allowed"},
		}
	}
	return NewPool(accts)
}

func TestSelectBestKeepsCurrent(t *testing.T) {
	p := makePool(2)
	name, _, switched, _ := p.SelectBest()
	if switched {
		t.Error("should not switch when current is fine")
	}
	if name != "acct1" {
		t.Errorf("want acct1, got %s", name)
	}
}

func TestSelectBestSwitchesOnThreshold(t *testing.T) {
	p := makePool(2)
	// Push active account over threshold
	p.accounts[0].rateLimit = RateLimit{
		Status:       "allowed_warning",
		FiveHourUtil: 0.90, // over 0.75 threshold for 2 accounts
	}

	name, prevName, switched, reason := p.SelectBest()
	if !switched {
		t.Error("should switch when active is over threshold")
	}
	if name != "acct2" {
		t.Errorf("want acct2, got %s", name)
	}
	if reason == "" {
		t.Error("switch reason should not be empty")
	}
	if prevName != "acct1" {
		t.Errorf("prevName should be acct1 (account before switch), got %s", prevName)
	}
}

func TestSelectBestAllRejected(t *testing.T) {
	p := makePool(2)
	p.accounts[0].rateLimit = RateLimit{Status: "rejected", FiveHourUtil: 1.0}
	p.accounts[1].rateLimit = RateLimit{Status: "rejected", FiveHourUtil: 1.0}

	if !p.AllRejected() {
		t.Error("AllRejected should be true")
	}

	// Should stay on current account, no switch
	_, _, switched, _ := p.SelectBest()
	if switched {
		t.Error("should not switch when all are rejected")
	}
}

func TestParseRateLimitHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "allowed_warning")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.73")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1700000000")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.21")
	h.Set("anthropic-ratelimit-unified-7d-reset", "1700604800")

	rl, ok := ParseRateLimitHeaders(h)
	if !ok {
		t.Fatal("should parse ok")
	}
	if rl.Status != "allowed_warning" {
		t.Errorf("want allowed_warning, got %s", rl.Status)
	}
	if rl.FiveHourUtil != 0.73 {
		t.Errorf("want 0.73, got %f", rl.FiveHourUtil)
	}
	if rl.FiveHourReset != time.Unix(1700000000, 0) {
		t.Error("wrong 5h reset time")
	}
	if rl.SevenDayUtil != 0.21 {
		t.Errorf("want 0.21, got %f", rl.SevenDayUtil)
	}
}

func TestAllRejectedPartial(t *testing.T) {
	p := makePool(3)
	// Only one account rejected — AllRejected should return false
	p.accounts[0].rateLimit = RateLimit{Status: "rejected"}
	p.accounts[1].rateLimit = RateLimit{Status: "allowed"}
	p.accounts[2].rateLimit = RateLimit{Status: "rejected"}

	if p.AllRejected() {
		t.Error("AllRejected should be false when some accounts are not rejected")
	}
}

func TestSelectBestCautionFallback(t *testing.T) {
	// All accounts over threshold (caution) but none rejected — Priority 3.
	// Should switch to the account with the lowest 5h utilization.
	p := makePool(3)
	threshold := p.Params().SwitchThreshold5h // 0.85 for N=3

	// All accounts over threshold, none rejected
	p.accounts[0].rateLimit = RateLimit{Status: "allowed_warning", FiveHourUtil: threshold + 0.05} // active, highest
	p.accounts[1].rateLimit = RateLimit{Status: "allowed_warning", FiveHourUtil: threshold + 0.02} // lowest
	p.accounts[2].rateLimit = RateLimit{Status: "allowed_warning", FiveHourUtil: threshold + 0.04}

	name, prevName, switched, reason := p.SelectBest()
	if !switched {
		t.Error("should switch to caution-fallback account")
	}
	if name != "acct2" {
		t.Errorf("caution fallback: want acct2 (lowest 5h), got %s", name)
	}
	if prevName != "acct1" {
		t.Errorf("prevName should be acct1, got %s", prevName)
	}
	if !strings.Contains(reason, "caution-fallback") {
		t.Errorf("reason should contain 'caution-fallback', got %q", reason)
	}
}

func TestSelectBestCautionFallbackStaysWhenCurrentIsBest(t *testing.T) {
	// All accounts over threshold, current has lowest 5h — should stay.
	p := makePool(3)
	threshold := p.Params().SwitchThreshold5h

	p.accounts[0].rateLimit = RateLimit{Status: "allowed_warning", FiveHourUtil: threshold + 0.01} // active, lowest
	p.accounts[1].rateLimit = RateLimit{Status: "allowed_warning", FiveHourUtil: threshold + 0.05}
	p.accounts[2].rateLimit = RateLimit{Status: "allowed_warning", FiveHourUtil: threshold + 0.08}

	_, _, switched, _ := p.SelectBest()
	if switched {
		t.Error("should not switch when current has lowest 5h in caution-fallback")
	}
}

func TestSoonestResetSkipsZeroTimes(t *testing.T) {
	p := makePool(2)
	// a1 has no reset time (zero), a2 has a real reset time
	reset := time.Now().Add(5 * time.Minute)
	p.accounts[0].rateLimit = RateLimit{Status: "rejected", FiveHourReset: time.Time{}} // zero
	p.accounts[1].rateLimit = RateLimit{Status: "rejected", FiveHourReset: reset}

	soonest := p.SoonestReset()
	if soonest != reset {
		t.Errorf("SoonestReset should skip zero times and return a2's reset; got %v, want %v", soonest, reset)
	}
}

func TestParseAccounts(t *testing.T) {
	raw, _ := json.Marshal([]AccountConfig{
		{Name: "main", RefreshToken: "rt_main"},
		{RefreshToken: "rt_anon"}, // no name → should default to "acct2"
	})
	accts, err := ParseAccounts(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(accts))
	}
	if accts[0].Name != "main" {
		t.Errorf("want main, got %s", accts[0].Name)
	}
	if accts[1].Name != "acct2" {
		t.Errorf("want acct2, got %s", accts[1].Name)
	}
}

func TestParseAccountsSeededToken(t *testing.T) {
	expiresAt := time.Now().Add(1 * time.Hour).UnixMilli()
	raw, _ := json.Marshal([]AccountConfig{
		{Name: "seeded", RefreshToken: "rt1", AccessToken: "at1", ExpiresAt: expiresAt},
	})
	accts, err := ParseAccounts(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	// Token should be immediately usable without a refresh
	tok, err := accts[0].token.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if tok != "at1" {
		t.Errorf("want at1, got %s", tok)
	}
}

func TestParseAccountsEmpty(t *testing.T) {
	_, err := ParseAccounts("[]")
	if err == nil {
		t.Error("expected error for empty accounts list")
	}
}

func TestParseAccountsMissingRefreshToken(t *testing.T) {
	raw, _ := json.Marshal([]AccountConfig{
		{Name: "bad", RefreshToken: ""},
	})
	_, err := ParseAccounts(string(raw))
	if err == nil {
		t.Error("expected error for missing refreshToken")
	}
}

func TestPoolLen(t *testing.T) {
	p := makePool(3)
	if p.Len() != 3 {
		t.Errorf("want 3, got %d", p.Len())
	}
}

func TestPoolParamsAndSetParams(t *testing.T) {
	p := makePool(2)
	got := p.Params()
	if got.SwitchThreshold5h != 0.75 {
		t.Errorf("want 0.75 initial, got %f", got.SwitchThreshold5h)
	}
	if got.ProactiveHysteresis != 0.0 {
		t.Errorf("N=2: want ProactiveHysteresis=0.0, got %f", got.ProactiveHysteresis)
	}
	p.SetParams(Params{SwitchThreshold5h: 0.50, HardBlock7d: 0.90, Weight5h: 0.60, Weight7d: 0.40, ProactiveHysteresis: 0.10})
	got = p.Params()
	if got.SwitchThreshold5h != 0.50 {
		t.Errorf("want 0.50 after set, got %f", got.SwitchThreshold5h)
	}
	if got.ProactiveHysteresis != 0.10 {
		t.Errorf("want ProactiveHysteresis=0.10 after set, got %f", got.ProactiveHysteresis)
	}
}

func TestSelectBestProactiveSwitches(t *testing.T) {
	// N=3 → ProactiveHysteresis=0.20 by default.
	// acct1 active: water=0.70; acct2: water=0.40 → 0.40 < 0.70*(1-0.20)=0.56 → proactive switch.
	p := makePool(3)
	params := p.Params()
	// acct1: 5h=0.70*threshold, 7d=0 → waterScore = max(0.70,0) = 0.70
	p.accounts[0].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.70 * params.SwitchThreshold5h}
	// acct2: 5h=0.40*threshold, 7d=0 → waterScore = 0.40
	p.accounts[1].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.40 * params.SwitchThreshold5h}
	// acct3: water=0.60 (worse than acct2, should not be chosen)
	p.accounts[2].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.60 * params.SwitchThreshold5h}

	name, prevName, switched, reason := p.SelectBest()
	if !switched {
		t.Error("should proactively switch to less-loaded account")
	}
	if name != "acct2" {
		t.Errorf("want acct2 (lowest water), got %s", name)
	}
	if prevName != "acct1" {
		t.Errorf("prevName should be acct1, got %s", prevName)
	}
	if !strings.Contains(reason, "proactive") {
		t.Errorf("reason should contain 'proactive', got %q", reason)
	}
}

func TestSelectBestProactiveNoSwitchInsideHysteresis(t *testing.T) {
	// acct2 water is only slightly better — within hysteresis band → no switch.
	p := makePool(3)
	params := p.Params()
	// acct1 water=0.70, acct2 water=0.65 → 0.65 > 0.70*(1-0.20)=0.56 → no switch
	p.accounts[0].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.70 * params.SwitchThreshold5h}
	p.accounts[1].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.65 * params.SwitchThreshold5h}
	p.accounts[2].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.68 * params.SwitchThreshold5h}

	_, _, switched, _ := p.SelectBest()
	if switched {
		t.Error("should not switch when alternative is within hysteresis band")
	}
}

func TestSelectBestProactiveDisabledForTwoAccounts(t *testing.T) {
	// N=2 → ProactiveHysteresis=0.0 → proactive switching disabled.
	p := makePool(2)
	params := p.Params()
	if params.ProactiveHysteresis != 0.0 {
		t.Fatalf("N=2 should have ProactiveHysteresis=0.0, got %f", params.ProactiveHysteresis)
	}
	// acct2 is much less loaded — but proactive is disabled
	p.accounts[0].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.70 * params.SwitchThreshold5h}
	p.accounts[1].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.01 * params.SwitchThreshold5h}

	_, _, switched, _ := p.SelectBest()
	if switched {
		t.Error("N=2 should not proactively switch (ProactiveHysteresis=0)")
	}
}

func TestWaterScore(t *testing.T) {
	const eps = 1e-9
	params := Params{SwitchThreshold5h: 0.80, HardBlock7d: 0.90}
	// 5h dominates: max(0.64/0.80, 0.27/0.90) = max(0.80, 0.30) = 0.80
	rl := RateLimit{FiveHourUtil: 0.64, SevenDayUtil: 0.27}
	ws := waterScore(rl, params)
	want5h := 0.64 / 0.80
	if ws < want5h-eps || ws > want5h+eps {
		t.Errorf("5h-dominant: want %.6f, got %.6f", want5h, ws)
	}
	// 7d dominates: max(0.10/0.80, 0.81/0.90) = max(0.125, 0.90) = 0.90
	rl2 := RateLimit{FiveHourUtil: 0.10, SevenDayUtil: 0.81}
	ws2 := waterScore(rl2, params)
	want7d := 0.81 / 0.90
	if ws2 < want7d-eps || ws2 > want7d+eps {
		t.Errorf("7d-dominant: want %.6f, got %.6f", want7d, ws2)
	}
}

func TestPoolActiveName(t *testing.T) {
	p := makePool(2)
	if p.ActiveName() != "acct1" {
		t.Errorf("want acct1, got %s", p.ActiveName())
	}
}

func TestUpdateAndGetRateLimit(t *testing.T) {
	p := makePool(2)
	rl := RateLimit{
		Status:       "allowed_warning",
		FiveHourUtil: 0.55,
		SevenDayUtil: 0.10,
	}
	p.UpdateRateLimit("acct1", rl)
	got := p.RateLimitFor("acct1")
	if got.Status != "allowed_warning" {
		t.Errorf("want allowed_warning, got %s", got.Status)
	}
	if got.FiveHourUtil != 0.55 {
		t.Errorf("want 0.55, got %f", got.FiveHourUtil)
	}
	if got.LastSeen.IsZero() {
		t.Error("LastSeen should be set after UpdateRateLimit")
	}
}

func TestRateLimitForUnknownAccount(t *testing.T) {
	p := makePool(2)
	rl := p.RateLimitFor("nonexistent")
	if rl.Status != "" {
		t.Errorf("want empty RateLimit for unknown account, got status=%s", rl.Status)
	}
}

func TestUpdateRateLimitUnknownAccount(t *testing.T) {
	p := makePool(2)
	// Should not panic
	p.UpdateRateLimit("nonexistent", RateLimit{Status: "allowed"})
}

func TestPoolAccounts(t *testing.T) {
	p := makePool(3)
	// Switch active to acct2
	p.accounts[1].rateLimit = RateLimit{Status: "allowed"} // already allowed
	// Force a switch by blocking acct1
	p.accounts[0].rateLimit = RateLimit{Status: "rejected", FiveHourUtil: 1.0}
	p.SelectBest() // switches to acct2

	snaps := p.Accounts()
	if len(snaps) != 3 {
		t.Fatalf("want 3 snapshots, got %d", len(snaps))
	}
	// Find the active one
	var active *AccountSnapshot
	for i := range snaps {
		if snaps[i].IsActive {
			active = &snaps[i]
		}
	}
	if active == nil {
		t.Fatal("no active account in snapshot")
	}
	if active.Name != "acct2" {
		t.Errorf("want acct2 active, got %s", active.Name)
	}
}

func makeSeededPool(n int) *Pool {
	accts := make([]*Account, n)
	expiresAt := time.Now().Add(1 * time.Hour)
	for i := range accts {
		name := "acct" + string(rune('0'+i+1))
		tok := newTokenSeeded("rt_"+name, "at_"+name, expiresAt)
		accts[i] = &Account{
			Name:      name,
			token:     tok,
			rateLimit: RateLimit{Status: "allowed"},
		}
	}
	return NewPool(accts)
}

func TestActiveToken(t *testing.T) {
	p := makeSeededPool(2)
	tok, err := p.ActiveToken(context.Background())
	if err != nil {
		t.Fatalf("ActiveToken failed: %v", err)
	}
	if tok != "at_acct1" {
		t.Errorf("want at_acct1, got %s", tok)
	}
}

func TestActiveTokenWithName(t *testing.T) {
	p := makeSeededPool(2)
	tok, name, err := p.ActiveTokenWithName(context.Background())
	if err != nil {
		t.Fatalf("ActiveTokenWithName failed: %v", err)
	}
	if tok != "at_acct1" {
		t.Errorf("want at_acct1, got %s", tok)
	}
	if name != "acct1" {
		t.Errorf("want acct1, got %s", name)
	}
}

func TestTokenFor(t *testing.T) {
	p := makeSeededPool(2)
	tok, err := p.TokenFor(context.Background(), "acct2")
	if err != nil {
		t.Fatalf("TokenFor failed: %v", err)
	}
	if tok != "at_acct2" {
		t.Errorf("want at_acct2, got %s", tok)
	}
}

func TestTokenForUnknown(t *testing.T) {
	p := makeSeededPool(2)
	_, err := p.TokenFor(context.Background(), "unknown")
	if err == nil {
		t.Error("expected error for unknown account name")
	}
}

func TestSetRefreshCallback(t *testing.T) {
	p := makePool(2)
	called := false
	p.SetRefreshCallback(func(name string, expiresInMins int) {
		called = true
		_ = name
		_ = expiresInMins
	})
	// Callback is registered; actual invocation tested in token_test.go
	// Just verify no panic and the field is wired (black-box check)
	_ = called
}

func TestParseRateLimitHeadersMissingStatus(t *testing.T) {
	// No status header → should return false
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.50")
	_, ok := ParseRateLimitHeaders(h)
	if ok {
		t.Error("ParseRateLimitHeaders should return false when status header is absent")
	}
}

func TestParseRateLimitHeadersPartial(t *testing.T) {
	// Only status header — other fields default to zero
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "allowed")
	rl, ok := ParseRateLimitHeaders(h)
	if !ok {
		t.Fatal("should parse ok with only status header")
	}
	if rl.Status != "allowed" {
		t.Errorf("want allowed, got %s", rl.Status)
	}
	if rl.FiveHourUtil != 0.0 {
		t.Errorf("FiveHourUtil should default to 0, got %f", rl.FiveHourUtil)
	}
	if !rl.FiveHourReset.IsZero() {
		t.Error("FiveHourReset should be zero when header absent")
	}
}

func TestParseAccountsInvalidJSON(t *testing.T) {
	_, err := ParseAccounts("not-valid-json")
	if err == nil {
		t.Error("expected error for invalid JSON input")
	}
}

func TestSetRefreshCallbackInvoked(t *testing.T) {
	// Start a fake token endpoint that returns a valid token.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at_fresh",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	orig := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = orig }()

	// Non-seeded pool — tokens need a refresh on first Ensure.
	p := makePool(2)

	var gotName string
	var gotMins int
	p.SetRefreshCallback(func(name string, mins int) {
		gotName = name
		gotMins = mins
	})

	// ActiveToken triggers Ensure → refresh → callback.
	_, err := p.ActiveToken(context.Background())
	if err != nil {
		t.Fatalf("ActiveToken: %v", err)
	}
	if gotName != "acct1" {
		t.Errorf("callback name: want acct1, got %q", gotName)
	}
	if gotMins != 60 {
		t.Errorf("callback mins: want 60, got %d", gotMins)
	}
}

func TestDefaultParams(t *testing.T) {
	p2 := defaultParams(2)
	if p2.SwitchThreshold5h != 0.75 {
		t.Errorf("N=2: want 0.75, got %f", p2.SwitchThreshold5h)
	}
	if p2.ProactiveHysteresis != 0.0 {
		t.Errorf("N=2: want ProactiveHysteresis=0.0, got %f", p2.ProactiveHysteresis)
	}
	p4 := defaultParams(4)
	if p4.SwitchThreshold5h != 0.80 {
		t.Errorf("N=4: want 0.80, got %f", p4.SwitchThreshold5h)
	}
	if p4.ProactiveHysteresis != 0.20 {
		t.Errorf("N=4: want ProactiveHysteresis=0.20, got %f", p4.ProactiveHysteresis)
	}
	p5 := defaultParams(5)
	if p5.SwitchThreshold5h != 0.85 {
		t.Errorf("N=5: want 0.85, got %f", p5.SwitchThreshold5h)
	}
	if p5.ProactiveHysteresis != 0.15 {
		t.Errorf("N=5: want ProactiveHysteresis=0.15, got %f", p5.ProactiveHysteresis)
	}
}

func TestInvalidateToken(t *testing.T) {
	accts, _ := ParseAccounts(`[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`)
	pool := NewPool(accts)

	// Confirm token is initially available.
	got, err := pool.TokenFor(context.Background(), "a1")
	if err != nil || got != "tok1" {
		t.Fatalf("pre-invalidate: want tok1, got %q err=%v", got, err)
	}

	// After invalidation, the token struct should be cleared.
	pool.InvalidateToken("a1")
	for _, a := range pool.accounts {
		a.mu.RLock()
		tok := a.token
		a.mu.RUnlock()
		tok.mu.RLock()
		empty := tok.accessToken == "" && tok.expiresAt.IsZero()
		tok.mu.RUnlock()
		if !empty {
			t.Error("InvalidateToken should clear accessToken and expiresAt")
		}
	}

	// Unknown account name → no-op.
	pool.InvalidateToken("nonexistent")
}

func TestParseAccountsFile(t *testing.T) {
	f, err := os.CreateTemp("", "cc-accts-*.json")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`[{"name":"a1","refreshToken":"rt1"}]`)
	f.Close()
	defer os.Remove(f.Name())

	accts, err := ParseAccountsFile(f.Name())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(accts) != 1 || accts[0].Name != "a1" {
		t.Fatalf("unexpected accounts: %+v", accts)
	}
}

func TestParseAccountsFileMissing(t *testing.T) {
	_, err := ParseAccountsFile("/tmp/does-not-exist-cc-pool-test.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestPersistAccounts(t *testing.T) {
	f, err := os.CreateTemp("", "cc-persist-*.json")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	accts, _ := ParseAccounts(`[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`)
	pool := NewPool(accts)

	if err := pool.PersistAccounts(f.Name()); err != nil {
		t.Fatalf("PersistAccounts: %v", err)
	}

	// Read back and verify
	reloaded, err := ParseAccountsFile(f.Name())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded) != 1 || reloaded[0].Name != "a1" {
		t.Fatalf("unexpected reloaded accounts: %+v", reloaded)
	}
}

func TestPersistAccountsBadPath(t *testing.T) {
	accts, _ := ParseAccounts(`[{"name":"a1","refreshToken":"rt1"}]`)
	pool := NewPool(accts)
	// Directory that doesn't exist — CreateTemp should fail
	err := pool.PersistAccounts("/tmp/nonexistent-dir-cc/accounts.json")
	if err == nil {
		t.Fatal("expected error for bad path")
	}
}

func TestWatchRotations(t *testing.T) {
	f, err := os.CreateTemp("", "cc-watch-*.json")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`)
	f.Close()
	defer os.Remove(f.Name())

	accts, _ := ParseAccountsFile(f.Name())
	pool := NewPool(accts)
	pool.WatchRotations(f.Name())

	// Simulate a rotation by firing the callback on the token directly
	for _, a := range pool.accounts {
		a.mu.RLock()
		tok := a.token
		a.mu.RUnlock()
		tok.mu.RLock()
		cb := tok.onRotate
		tok.mu.RUnlock()
		if cb == nil {
			t.Fatal("WatchRotations did not set rotate callback")
		}
		// Fire it with updated credentials
		cb("rt2", "tok2", time.Now().Add(time.Hour))
	}

	// File should now contain the updated credentials
	reloaded, err := ParseAccountsFile(f.Name())
	if err != nil {
		t.Fatalf("reload after rotation: %v", err)
	}
	if len(reloaded) != 1 {
		t.Fatalf("unexpected account count: %d", len(reloaded))
	}
}

func TestWatchRotationsCallbackError(t *testing.T) {
	// Create a temp dir and place the accounts file inside it
	dir, err := os.MkdirTemp("", "cc-watch-err-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	path := dir + "/accounts.json"
	if err := os.WriteFile(path, []byte(`[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`), 0644); err != nil {
		t.Fatal(err)
	}

	accts, _ := ParseAccountsFile(path)
	pool := NewPool(accts)
	pool.WatchRotations(path)

	// Remove the directory so PersistAccounts fails when the callback fires
	os.RemoveAll(dir)

	// Fire the callback — PersistAccounts will fail, log.Printf branch executes
	for _, a := range pool.accounts {
		a.mu.RLock()
		tok := a.token
		a.mu.RUnlock()
		tok.mu.RLock()
		cb := tok.onRotate
		tok.mu.RUnlock()
		if cb != nil {
			cb("rt2", "tok2", time.Now().Add(time.Hour))
		}
	}
	// If we reach here without panic, the error path was handled correctly
}
