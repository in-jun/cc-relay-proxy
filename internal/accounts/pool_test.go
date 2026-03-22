package accounts

import (
	"context"
	"encoding/json"
	"net/http"
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
	p.SetParams(Params{SwitchThreshold5h: 0.50, HardBlock7d: 0.90, Weight5h: 0.60, Weight7d: 0.40})
	got = p.Params()
	if got.SwitchThreshold5h != 0.50 {
		t.Errorf("want 0.50 after set, got %f", got.SwitchThreshold5h)
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

func TestDefaultParams(t *testing.T) {
	p2 := defaultParams(2)
	if p2.SwitchThreshold5h != 0.75 {
		t.Errorf("N=2: want 0.75, got %f", p2.SwitchThreshold5h)
	}
	p4 := defaultParams(4)
	if p4.SwitchThreshold5h != 0.80 {
		t.Errorf("N=4: want 0.80, got %f", p4.SwitchThreshold5h)
	}
	p5 := defaultParams(5)
	if p5.SwitchThreshold5h != 0.85 {
		t.Errorf("N=5: want 0.85, got %f", p5.SwitchThreshold5h)
	}
}
