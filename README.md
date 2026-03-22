# cc-relay-proxy

A local reverse proxy for Claude Code that transparently rotates between multiple
Anthropic accounts to avoid rate limits. Claude Code never sees a 429 — the proxy
absorbs it and retries with a fresh account.

```
Claude Code  (ANTHROPIC_BASE_URL=http://localhost:9999)
     │
     ▼
cc-relay-proxy  :9999
  ├─ Swap Authorization header → best available OAuth token
  ├─ Parse rate-limit headers from every response
  ├─ On 429 → switch account + retry transparently
  ├─ Auto-tune selection parameters from log data hourly
  └─ GET /status  (JSON dashboard)
     │
     ▼
api.anthropic.com
```

## Quick start

### 1. Build

```bash
go build -o cc-relay-proxy ./cmd/cc-relay-proxy
```

Requires Go 1.21+. No external dependencies.

### 2. Create accounts file

```bash
cat > accounts.json << 'EOF'
[
  {"name": "acct1", "refreshToken": "rt_..."},
  {"name": "acct2", "refreshToken": "rt_..."},
  {"name": "acct3", "refreshToken": "rt_..."}
]
EOF
```

Get your refresh token from the local Claude Code credentials:

```bash
cat ~/.claude/.credentials.json | python3 -c "
import json, sys
d = json.load(sys.stdin)
print(d['claudeAiOauth']['refreshToken'])
"
```

> **API-key mode**: if you run Claude Code with `ANTHROPIC_API_KEY=anything` instead
> of a local login, the proxy handles it automatically — it strips the `x-api-key`
> header and injects the OAuth Bearer token.

### 3. Run

```bash
./cc-relay-proxy
```

### 4. Connect Claude Code

Add to `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:9999"
  }
}
```

Or set the environment variable before launching Claude Code:

```bash
ANTHROPIC_BASE_URL=http://localhost:9999 claude
```

## Configuration

All configuration is through environment variables.

| Variable | Default | Description |
|---|---|---|
| `CC_ACCOUNTS_FILE` | `accounts.json` | Path to JSON accounts file |
| `CC_PROXY_PORT` | `9999` | Port to listen on |
| `CC_PROXY_BIND` | `127.0.0.1` | Bind address (`0.0.0.0` to expose on LAN) |
| `CC_LOG_PATH` | `logs/proxy.log` | JSONL log file (rotates at 10 MB) |
| `CC_TUNE_INTERVAL` | `3600` | Auto-tuning interval in seconds |
| `CC_LOG_LEVEL` | — | Set to `debug` to log auth headers |

## Account selection algorithm

The proxy uses a **water-filling** algorithm to pick the account with the most
remaining capacity across both rate-limit dimensions simultaneously.

### Water score (lower = preferred)

```
effective_5h = 5h_utilization × (minutes_to_5h_reset / 300)
effective_7d = 7d_utilization × (hours_to_7d_reset   / 168)

water = max(effective_5h / switchThreshold5h,
            effective_7d / hardBlock7d)
```

The time-decay factors are key: an account at 90% 5h utilization that resets in
10 minutes scores ~0.04, while the same account with 4 hours to reset scores 1.125.
Accounts near their reset windows naturally float to the top of the candidate pool.

### Selection priority

1. **Reactive switch** — current account is blocked (over threshold or rejected) →
   switch to the non-blocked account with the lowest water score.
2. **Proactive switch** — current account is fine, but an alternative has a water
   score at least `proactiveHysteresis` fraction lower → switch to equalize load.
3. **Keep current** — no better option.
4. **Caution fallback** — all accounts near threshold → pick lowest effective 5h.
5. **All rejected** — hold the connection until the soonest reset (max ~10 min),
   then retry. If the wait exceeds 10 minutes, forward the 429 to Claude Code.

### Default parameters (by account count)

| N accounts | switchThreshold5h | hardBlock7d | proactiveHysteresis |
|---|---|---|---|
| ≤ 2 | 0.75 | 0.90 | 0 (disabled) |
| 3–4 | 0.80 | 0.90 | 0.20 |
| 5+ | 0.85 | 0.90 | 0.15 |

Parameters are auto-tuned hourly by analyzing the last 24 hours of log data.

## Status endpoint

```bash
curl -s http://localhost:9999/status | python3 -m json.tool
```

```json
{
  "active": "acct1",
  "uptime": "2h34m",
  "totalRequests": 1234,
  "totalSwitches": 5,
  "total429": 0,
  "params": {
    "switchThreshold5h": 0.80,
    "hardBlock7d": 0.90,
    "proactiveHysteresis": 0.20,
    "tuneInterval": "1h0m0s",
    "lastTuned": "34m ago"
  },
  "accounts": [
    {
      "name": "acct1",
      "isActive": true,
      "status": "allowed",
      "fiveHour": { "utilization": 0.34, "percent": "34%", "resetsInMins": 87 },
      "sevenDay":  { "utilization": 0.12, "percent": "12%", "resetsInHours": 143 },
      "tokenExpiresIn": "42m",
      "lastSeen": "2m ago"
    }
  ],
  "tuneHistory": []
}
```

## Project structure

```
cc-relay-proxy/
├── cmd/
│   └── cc-relay-proxy/    # main package — startup, wiring, graceful shutdown
│       ├── main.go
│       └── main_test.go
├── internal/
│   ├── accounts/          # OAuth token management and account pool
│   │   ├── pool.go        # selection algorithm, water-fill score, params
│   │   ├── pool_test.go
│   │   ├── token.go       # token refresh with RTR, exponential backoff
│   │   └── token_test.go
│   ├── logger/            # structured JSONL logger with rotation
│   │   ├── logger.go
│   │   └── logger_test.go
│   ├── proxy/             # HTTP reverse proxy, 429 handling, ping scheduler
│   │   ├── auth_adapter.go   # Claude Code auth protocol (single change point)
│   │   ├── pinger.go         # keepalive pings, pool snapshots
│   │   ├── server.go         # request handler, status endpoint, SSE streaming
│   │   └── server_test.go
│   └── tuner/             # log-driven parameter auto-tuning
│       ├── tuner.go
│       └── tuner_test.go
├── accounts.json          # account credentials (not committed)
├── logs/                  # JSONL logs (not committed)
└── go.mod
```

## Log format

Every significant event is written as a JSONL line to `logs/proxy.log`.

```jsonc
{"ts":1234567890123,"event":"startup",          "data":{"numAccounts":3,"params":{...}}}
{"ts":...,"event":"request",      "account":"acct1","data":{"method":"POST","path":"/v1/messages","status":200,"latencyMs":312,"fiveHour":0.45,"sevenDay":0.12,"water":0.562}}
{"ts":...,"event":"account_switched","account":"acct2","data":{"from":"acct1","to":"acct2","reason":"proactive: ...","fiveHour_before":0.82}}
{"ts":...,"event":"rate_limit_update","account":"acct1","data":{"fiveHour":0.45,"sevenDay":0.12,"status":"allowed","water":0.562,"5hResetInMins":87,"7dResetInHrs":141}}
{"ts":...,"event":"429_received", "account":"acct1","data":{"action":"switch|hold|forward","fiveHour":0.95}}
{"ts":...,"event":"pool_snapshot",              "data":{"trigger":"periodic","accounts":[{"name":"acct1","water":0.562,...}]}}
{"ts":...,"event":"token_refreshed","account":"acct1","data":{"expiresInMins":58}}
{"ts":...,"event":"params_updated",             "data":{"old":{...},"new":{...},"reason":"premature switch ratio 24% → threshold +0.03"}}
```

## Building and testing

```bash
go build ./...        # build all packages
go vet ./...          # static analysis
go test ./...         # full test suite
go test -race ./...   # race detector
```

Build the binary:

```bash
go build -o cc-relay-proxy ./cmd/cc-relay-proxy
```
