package proxy

import (
	"net/http"
	"strings"
)

// oauthBetaFlag is the beta header value required for OAuth token authentication.
// Without it, api.anthropic.com returns "OAuth authentication is currently not supported".
//
// Claude Code behaviour by mode:
//   - OAuth mode:  sends "Authorization: Bearer <token>" + "anthropic-beta: oauth-2025-04-20" (already present)
//   - API-key mode: sends "x-api-key: <key>", no anthropic-beta header
//
// If a future Claude Code release changes this flag value, update this constant only.
const oauthBetaFlag = "oauth-2025-04-20"

// adaptClientAuth rewrites outgoing request headers to use OAuth Bearer authentication,
// regardless of the mode Claude Code is running in.
//
// This is the single location for Claude Code auth protocol details. When the
// upstream auth scheme changes (new header name, different beta flag, etc.),
// edit this function and oauthBetaFlag above — nothing else needs to change.
func adaptClientAuth(req *http.Request, tok string) {
	// API-key mode: Claude Code sends x-api-key instead of Authorization.
	// Strip it — we always authenticate via OAuth Bearer.
	req.Header.Del("x-api-key")
	req.Header.Set("Authorization", "Bearer "+tok)
	ensureOAuthBeta(req.Header)
}

// ensureOAuthBeta adds oauthBetaFlag to the anthropic-beta header if not already present.
func ensureOAuthBeta(h http.Header) {
	existing := h.Get("anthropic-beta")
	if strings.Contains(existing, oauthBetaFlag) {
		return
	}
	if existing == "" {
		h.Set("anthropic-beta", oauthBetaFlag)
	} else {
		h.Set("anthropic-beta", existing+","+oauthBetaFlag)
	}
}
