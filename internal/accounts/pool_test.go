package accounts

import (
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
	name, switched, _ := p.SelectBest()
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

	name, switched, reason := p.SelectBest()
	if !switched {
		t.Error("should switch when active is over threshold")
	}
	if name != "acct2" {
		t.Errorf("want acct2, got %s", name)
	}
	if reason == "" {
		t.Error("switch reason should not be empty")
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
	_, switched, _ := p.SelectBest()
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
