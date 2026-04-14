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
	// curWater=0 and bestEffective=0 → bestEffective < curEffective is false → no switch.
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

func TestWaterScore(t *testing.T) {
	const eps = 1e-6
	farFuture := time.Now().Add(200 * time.Hour) // beyond both windows → decay=1

	// 5h dominates: max(0.64*1, 0.27*1) = 0.64
	rl := RateLimit{FiveHourUtil: 0.64, SevenDayUtil: 0.27, FiveHourReset: farFuture, SevenDayReset: farFuture}
	ws := WaterScore(rl)
	if ws < 0.64-eps || ws > 0.64+eps {
		t.Errorf("5h-dominant: want 0.640000, got %.6f", ws)
	}

	// 7d dominates: max(0.10*1, 0.81*1) = 0.81
	rl2 := RateLimit{FiveHourUtil: 0.10, SevenDayUtil: 0.81, FiveHourReset: farFuture, SevenDayReset: farFuture}
	ws2 := WaterScore(rl2)
	if ws2 < 0.81-eps || ws2 > 0.81+eps {
		t.Errorf("7d-dominant: want 0.810000, got %.6f", ws2)
	}
}

func TestWaterScoreTimeDecay(t *testing.T) {
	// 5h=90%, resets in 30 of 300 minutes → decay=0.10 → effective_5h=0.09
	// 7d=50%, resets far future → effective_7d=0.50
	// water = max(0.09, 0.50) = 0.50
	const eps = 0.01
	resetSoon := time.Now().Add(30 * time.Minute)
	farFuture := time.Now().Add(200 * time.Hour)
	rl := RateLimit{FiveHourUtil: 0.90, SevenDayUtil: 0.50, FiveHourReset: resetSoon, SevenDayReset: farFuture}
	ws := WaterScore(rl)
	if ws < 0.50-eps || ws > 0.50+eps {
		t.Errorf("time-decayed 5h: want ~0.50, got %.6f", ws)
	}

	// Must score lower than same account with far 5h reset
	rlFar := RateLimit{FiveHourUtil: 0.90, SevenDayUtil: 0.50, FiveHourReset: farFuture, SevenDayReset: farFuture}
	wsFar := WaterScore(rlFar)
	if ws >= wsFar {
		t.Errorf("near-reset account should score lower: near=%.4f far=%.4f", ws, wsFar)
	}
}

func TestWaterScoreZeroReset(t *testing.T) {
	// Reset already past → decay=0 → effective util=0 → score=0
	pastReset := time.Now().Add(-1 * time.Minute)
	rl := RateLimit{FiveHourUtil: 0.95, SevenDayUtil: 0.95, FiveHourReset: pastReset, SevenDayReset: pastReset}
	ws := WaterScore(rl)
	if ws > 1e-9 {
		t.Errorf("past-reset account should score 0, got %.6f", ws)
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
