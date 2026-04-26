package accounts

import (
	"context"
	"encoding/json"
	"math"
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
			Name:      "acct" + string(rune('0'+i+1)),
			token:     newToken("rt_" + string(rune('a'+i))),
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

func TestSelectBestReactiveSwitchOnRejected(t *testing.T) {
	p := makePool(2)
	// Active account is rejected by the API.
	p.accounts[0].rateLimit = RateLimit{
		Status:       "rejected",
		FiveHourUtil: 1.0,
	}

	name, prevName, switched, reason := p.SelectBest()
	if !switched {
		t.Error("should switch when active is rejected")
	}
	if name != "acct2" {
		t.Errorf("want acct2, got %s", name)
	}
	if reason == "" {
		t.Error("switch reason should not be empty")
	}
	if prevName != "acct1" {
		t.Errorf("prevName should be acct1, got %s", prevName)
	}
	if !strings.Contains(reason, "reactive") {
		t.Errorf("reason should contain 'reactive', got %q", reason)
	}
}

func TestSelectBestHighUtilDoesNotBlock(t *testing.T) {
	// High utilization alone no longer triggers a reactive switch — only
	// "rejected" status does. Current account should stay when it already has
	// the lowest water score.
	p := makePool(2)
	farFuture := time.Now().Add(200 * time.Hour)
	// acct1 at 94%, acct2 at 95% — current (acct1) is already lowest water, no switch.
	p.accounts[0].rateLimit = RateLimit{Status: "allowed_warning", FiveHourUtil: 0.94, FiveHourReset: farFuture}
	p.accounts[1].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.95, FiveHourReset: farFuture}

	_, _, switched, _ := p.SelectBest()
	if switched {
		t.Error("high utilization alone should not trigger reactive switch")
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

func TestSelectBestProactiveSwitches(t *testing.T) {
	// acct1 water=0.70, acct2 water=0.40 → bestEffective(0.40) < curEffective(0.70) → proactive switch.
	p := makePool(3)
	farFuture := time.Now().Add(200 * time.Hour)
	p.accounts[0].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.70, FiveHourReset: farFuture}
	p.accounts[1].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.40, FiveHourReset: farFuture}
	p.accounts[2].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.60, FiveHourReset: farFuture}

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

func TestSelectBestProactiveNoSwitchWhenCurrentIsBest(t *testing.T) {
	// Current account already has the lowest water — no switch.
	p := makePool(3)
	farFuture := time.Now().Add(200 * time.Hour)
	p.accounts[0].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.40, FiveHourReset: farFuture}
	p.accounts[1].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.65, FiveHourReset: farFuture}
	p.accounts[2].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.68, FiveHourReset: farFuture}

	_, _, switched, _ := p.SelectBest()
	if switched {
		t.Error("should not switch when current account already has lowest water")
	}
}

func TestSelectBestProactiveSkippedWhenCurrentWaterZero(t *testing.T) {
	// Both accounts have no reset-time data → WaterScore returns unknownWater (1.0).
	// curEffective(1.0) == bestEffective(1.0) → bestEffective < curEffective is false → no switch.
	p := makePool(2)
	p.accounts[0].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0, SevenDayUtil: 0}
	p.accounts[1].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0, SevenDayUtil: 0}
	_, _, switched, _ := p.SelectBest()
	if switched {
		t.Error("should not switch when current water score is zero")
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
	p.accounts[0].rateLimit = RateLimit{Status: "rejected"}
	p.accounts[1].rateLimit = RateLimit{Status: "allowed"}
	p.accounts[2].rateLimit = RateLimit{Status: "rejected"}

	if p.AllRejected() {
		t.Error("AllRejected should be false when some accounts are not rejected")
	}
}

func TestSoonestResetConsidersBothWindows(t *testing.T) {
	p := makePool(2)
	reset5h := time.Now().Add(5 * time.Minute)
	reset7d := time.Now().Add(3 * time.Minute) // 7d resets sooner
	p.accounts[0].rateLimit = RateLimit{Status: "rejected", FiveHourReset: reset5h, SevenDayReset: reset7d}
	p.accounts[1].rateLimit = RateLimit{Status: "rejected"} // zero times

	soonest := p.SoonestReset()
	if soonest != reset7d {
		t.Errorf("SoonestReset should return earliest of 5h/7d across all accounts; got %v, want %v", soonest, reset7d)
	}
}

// TestWaterScore verifies the over-pace formula:
//
//	adj_d = max(0, u_d − pace_d)   where pace_d = (W_d − t_d) / W_d
//	score = max(adj_5h, adj_7d)
//
// pace_d is the time-proportional baseline: an account exactly on pace has
// adj=0 and will exhaust its quota precisely at the reset time. adj>0 means
// the account is ahead of pace and risks exhaustion before reset.
func TestWaterScore(t *testing.T) {
	const eps = 1e-6
	// farFuture: reset beyond each window → pace=0 → score = raw utilization.
	far5h := time.Now().Add(10 * time.Hour)  // > 5h window
	far7d := time.Now().Add(200 * time.Hour) // > 168h window

	// No imminent reset: score = max(util_5h, util_7d).
	// 5h dominates: max(0.64, 0.27) = 0.64
	rl := RateLimit{FiveHourUtil: 0.64, SevenDayUtil: 0.27, FiveHourReset: far5h, SevenDayReset: far7d}
	if ws := WaterScore(rl); math.Abs(ws-0.64) > eps {
		t.Errorf("5h dominates: want 0.64, got %f", ws)
	}
	// 7d dominates: max(0.10, 0.81) = 0.81
	rl2 := RateLimit{FiveHourUtil: 0.10, SevenDayUtil: 0.81, FiveHourReset: far5h, SevenDayReset: far7d}
	if ws := WaterScore(rl2); math.Abs(ws-0.81) > eps {
		t.Errorf("7d dominates: want 0.81, got %f", ws)
	}

	// Exactly at pace → score = 0.
	// 5h at midpoint: util=0.50, t=2.5h → pace=(5-2.5)/5=0.50 → adj5h=0
	// 7d util=0 so adj7d=0; score=0
	reset5hMid := time.Now().Add(150 * time.Minute) // 2.5h from now
	rlOnPace := RateLimit{FiveHourUtil: 0.50, SevenDayUtil: 0.0, FiveHourReset: reset5hMid, SevenDayReset: far7d}
	if ws := WaterScore(rlOnPace); ws > eps {
		t.Errorf("exactly on pace: want 0, got %f", ws)
	}

	// Over pace → adj > 0.
	// 5h: util=0.90, t=2.5h → pace=0.50 → adj=0.40
	rl3 := RateLimit{FiveHourUtil: 0.90, SevenDayUtil: 0.10, FiveHourReset: reset5hMid, SevenDayReset: far7d}
	if ws := WaterScore(rl3); math.Abs(ws-0.40) > eps {
		t.Errorf("over pace (2.5h): want 0.40, got %f", ws)
	}

	// Near reset → pace ≈ 1 → adj ≈ 0 regardless of utilization.
	// 5h: util=0.90, t=5min → pace≈0.983 → adj5h≈0 → score=adj7d=0.05
	reset5min := time.Now().Add(5 * time.Minute)
	rl4 := RateLimit{FiveHourUtil: 0.90, SevenDayUtil: 0.05, FiveHourReset: reset5min, SevenDayReset: far7d}
	if ws := WaterScore(rl4); math.Abs(ws-0.05) > eps {
		t.Errorf("near reset: want 0.05, got %f", ws)
	}

	// 7d symmetric: util=0.90, t=20h → pace=(168-20)/168=148/168
	// adj7d=max(0, 0.90-148/168); adj5h=0.10 → score=max(0.10, adj7d)
	reset7d20h := time.Now().Add(20 * time.Hour)
	rl5 := RateLimit{FiveHourUtil: 0.10, SevenDayUtil: 0.90, FiveHourReset: far5h, SevenDayReset: reset7d20h}
	wantAdj7d := math.Max(0, 0.90-148.0/168.0)
	want5 := math.Max(0.10, wantAdj7d)
	if ws := WaterScore(rl5); math.Abs(ws-want5) > eps {
		t.Errorf("7d reset in 20h: want %.6f, got %f", want5, ws)
	}

	// Unknown (zero resets) → unknownWater = 1.0 (uninitialized accounts go last).
	if ws := WaterScore(RateLimit{}); ws != unknownWater {
		t.Errorf("unknown: want 1.0, got %f", ws)
	}
}

func TestIsBlockedOnlyOnRejected(t *testing.T) {
	// Verify blocking behavior via SelectBest: only "rejected" triggers reactive switch.
	p := makePool(2)

	// allowed_warning at 99% — no reactive switch (high util alone does not block).
	p.accounts[0].rateLimit = RateLimit{Status: "allowed_warning", FiveHourUtil: 0.99, SevenDayUtil: 0.99}
	p.accounts[1].rateLimit = RateLimit{Status: "allowed", FiveHourUtil: 0.98}
	_, _, switched, _ := p.SelectBest()
	if switched {
		t.Error("allowed_warning at 99% should NOT trigger reactive switch")
	}

	// Reset and try with rejected — should trigger reactive switch.
	p2 := makePool(2)
	p2.accounts[0].rateLimit = RateLimit{Status: "rejected", FiveHourUtil: 1.0}
	p2.accounts[1].rateLimit = RateLimit{Status: "allowed"}
	_, _, switched2, _ := p2.SelectBest()
	if !switched2 {
		t.Error("rejected account should trigger reactive switch")
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
	// Force a switch by rejecting acct1
	p.accounts[0].rateLimit = RateLimit{Status: "rejected", FiveHourUtil: 1.0}
	p.accounts[1].rateLimit = RateLimit{Status: "allowed"}
	p.SelectBest() // switches to acct2

	snaps := p.Accounts()
	if len(snaps) != 3 {
		t.Fatalf("want 3 snapshots, got %d", len(snaps))
	}
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
	// Verify SetRefreshCallback attaches the callback on every account token
	// so the internal onRefresh field is non-nil after the call.
	p := makePool(2)
	p.SetRefreshCallback(func(name string, expiresInMins int) {})
	for _, a := range p.accounts {
		a.mu.RLock()
		tok := a.token
		a.mu.RUnlock()
		tok.mu.RLock()
		cb := tok.onRefresh
		tok.mu.RUnlock()
		if cb == nil {
			t.Errorf("account %s: SetRefreshCallback did not set onRefresh", a.Name)
		}
	}
}

func TestParseRateLimitHeadersMissingStatus(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.50")
	_, ok := ParseRateLimitHeaders(h)
	if ok {
		t.Error("ParseRateLimitHeaders should return false when status header is absent")
	}
}

func TestParseRateLimitHeadersPartial(t *testing.T) {
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

func TestParseAccounts(t *testing.T) {
	raw, _ := json.Marshal([]AccountConfig{
		{Name: "main", RefreshToken: "rt_main"},
		{RefreshToken: "rt_anon"},
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

func TestInvalidateToken(t *testing.T) {
	accts, _ := ParseAccounts(`[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`)
	pool := NewPool(accts)

	got, err := pool.TokenFor(context.Background(), "a1")
	if err != nil || got != "tok1" {
		t.Fatalf("pre-invalidate: want tok1, got %q err=%v", got, err)
	}

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
		cb("rt2", "tok2", time.Now().Add(time.Hour))
	}

	reloaded, err := ParseAccountsFile(f.Name())
	if err != nil {
		t.Fatalf("reload after rotation: %v", err)
	}
	if len(reloaded) != 1 {
		t.Fatalf("unexpected account count: %d", len(reloaded))
	}
}

func TestWatchRotationsCallbackError(t *testing.T) {
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

	os.RemoveAll(dir)

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
}

func TestParseAccountsInvalidJSON(t *testing.T) {
	_, err := ParseAccounts("not-valid-json")
	if err == nil {
		t.Error("expected error for invalid JSON input")
	}
}

func TestSetRefreshCallbackInvoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at_fresh",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	orig := SetTokenEndpoint(srv.URL)
	defer SetTokenEndpoint(orig)

	p := makePool(2)

	var gotName string
	var gotMins int
	p.SetRefreshCallback(func(name string, mins int) {
		gotName = name
		gotMins = mins
	})

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

// TestPersistAccountTargeted verifies that PersistAccount only updates the
// named account on disk and leaves all other accounts' tokens untouched.
// This is the regression test for the bug where WatchRotations called
// PersistAccounts (write-all-from-memory), which overwrote other accounts'
// on-disk tokens with potentially dead/consumed in-memory tokens.
func TestPersistAccountTargeted(t *testing.T) {
	f, err := os.CreateTemp("", "cc-persist-targeted-*.json")
	if err != nil {
		t.Fatal(err)
	}
	// Two accounts on disk with known tokens.
	initial := `[
  {"name":"a1","refreshToken":"rt1-disk","accessToken":"at1-disk","expiresAt":9999999999999,"priority":1},
  {"name":"a2","refreshToken":"rt2-disk","accessToken":"at2-disk","expiresAt":9999999999999,"priority":1}
]`
	f.WriteString(initial)
	f.Close()
	defer os.Remove(f.Name())

	// Load pool from disk so in-memory state matches disk.
	accts, err := ParseAccountsFile(f.Name())
	if err != nil {
		t.Fatalf("ParseAccountsFile: %v", err)
	}
	pool := NewPool(accts)

	// Simulate a2's in-memory token going stale (invalid_grant scenario).
	// In production this happens when a2's refresh fails; the consumed RT is
	// kept in memory but should NOT be written to disk.
	pool.accounts[1].token.mu.Lock()
	pool.accounts[1].token.refreshToken = "rt2-CONSUMED-dead"
	pool.accounts[1].token.accessToken = ""
	pool.accounts[1].token.mu.Unlock()

	// Only a1 rotated — persist just a1 with new credentials.
	newRT, newAT := "rt1-new", "at1-new"
	newExp := time.Now().Add(time.Hour)
	if err := pool.PersistAccount(f.Name(), "a1", newRT, newAT, newExp); err != nil {
		t.Fatalf("PersistAccount: %v", err)
	}

	// Reload from disk and verify:
	// - a1 has the new tokens.
	// - a2 still has rt2-disk (the good on-disk token), NOT rt2-CONSUMED-dead.
	reloaded, err := ParseAccountsFile(f.Name())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(reloaded))
	}
	byName := make(map[string]*Account, len(reloaded))
	for _, a := range reloaded {
		byName[a.Name] = a
	}
	a1 := byName["a1"]
	if a1 == nil {
		t.Fatal("a1 missing from file")
	}
	a1.token.mu.RLock()
	gotRT1 := a1.token.refreshToken
	a1.token.mu.RUnlock()
	if gotRT1 != newRT {
		t.Errorf("a1 refreshToken: want %q, got %q", newRT, gotRT1)
	}

	a2 := byName["a2"]
	if a2 == nil {
		t.Fatal("a2 missing from file")
	}
	a2.token.mu.RLock()
	gotRT2 := a2.token.refreshToken
	a2.token.mu.RUnlock()
	// Must be the on-disk token, not the dead in-memory one.
	if gotRT2 == "rt2-CONSUMED-dead" {
		t.Error("PersistAccount wrote the dead in-memory token for a2 — regression!")
	}
	if gotRT2 != "rt2-disk" {
		t.Errorf("a2 refreshToken: want rt2-disk, got %q", gotRT2)
	}
}

// TestPersistAccountFallbackOnMissingFile verifies PersistAccount falls back to
// a full in-memory write when the file doesn't exist yet.
func TestPersistAccountFallbackOnMissingFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "cc-persist-fallback-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := dir + "/accounts.json"

	accts, _ := ParseAccounts(`[{"name":"a1","refreshToken":"rt1","accessToken":"at1","expiresAt":9999999999999}]`)
	pool := NewPool(accts)

	// File does not exist — should fall back to PersistAccounts without error.
	exp := time.Now().Add(time.Hour)
	if err := pool.PersistAccount(path, "a1", "rt1-new", "at1-new", exp); err != nil {
		t.Fatalf("PersistAccount fallback: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created by fallback: %v", err)
	}
}
