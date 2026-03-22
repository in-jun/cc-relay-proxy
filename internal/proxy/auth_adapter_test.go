package proxy

import (
	"net/http"
	"testing"
)

func TestAdaptClientAuthApiKeyMode(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Header.Set("x-api-key", "sk-ant-test-key")
	req.Header.Set("Content-Type", "application/json")

	adaptClientAuth(req, "test-token")

	if req.Header.Get("x-api-key") != "" {
		t.Error("x-api-key should be stripped")
	}
	if req.Header.Get("Authorization") != "Bearer test-token" {
		t.Errorf("Authorization wrong: %s", req.Header.Get("Authorization"))
	}
	if req.Header.Get("anthropic-beta") != oauthBetaFlag {
		t.Errorf("anthropic-beta wrong: %s", req.Header.Get("anthropic-beta"))
	}
}

func TestAdaptClientAuthOAuthMode(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer old-token")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	adaptClientAuth(req, "new-token")

	if req.Header.Get("Authorization") != "Bearer new-token" {
		t.Errorf("Authorization wrong: %s", req.Header.Get("Authorization"))
	}
	// Flag should not be duplicated
	beta := req.Header.Get("anthropic-beta")
	if beta != "oauth-2025-04-20" {
		t.Errorf("beta flag duplicated or wrong: %q", beta)
	}
}

func TestAdaptClientAuthPreservesExistingBeta(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Header.Set("anthropic-beta", "computer-use-2025-01-15")

	adaptClientAuth(req, "tok")

	beta := req.Header.Get("anthropic-beta")
	if beta != "computer-use-2025-01-15,oauth-2025-04-20" {
		t.Errorf("unexpected beta value: %q", beta)
	}
}

func TestEnsureOAuthBetaIdempotent(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-beta", "oauth-2025-04-20,other-flag")
	ensureOAuthBeta(h)
	if h.Get("anthropic-beta") != "oauth-2025-04-20,other-flag" {
		t.Errorf("should not modify header when flag already present: %q", h.Get("anthropic-beta"))
	}
}
