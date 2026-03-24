# cc-relay-proxy

A local proxy that sits between Claude Code and the Anthropic API, transparently rotating between multiple accounts to avoid rate limits. Claude Code never sees a 429 — the proxy absorbs it and retries with a different account.

```
Claude Code  →  cc-relay-proxy :9999  →  api.anthropic.com
                 ├─ Route to best available account
                 ├─ On 429 → switch account and retry
                 └─ GET /status
```

## Installation

### Docker (recommended)

```bash
mkdir -p config logs
# put your accounts.json inside config/
docker run -d \
  -p 9999:9999 \
  -v $(pwd)/config:/app/config \
  -v $(pwd)/logs:/app/logs \
  --name cc-relay-proxy \
  injundev/cc-relay-proxy
```

### Build from source

```bash
git clone https://github.com/in-jun/cc-relay-proxy
cd cc-relay-proxy
go build -o cc-relay-proxy ./cmd/cc-relay-proxy
```

Requires Go 1.21+. No external dependencies.

## Setup

### 1. Create config/accounts.json

```bash
mkdir -p config
```

```json
[
  {"name": "acct1", "refreshToken": "rt_..."},
  {"name": "acct2", "refreshToken": "rt_..."}
]
```

Get your refresh token from the local Claude Code credentials:

```bash
cat ~/.claude/.credentials.json | python3 -c "
import json, sys
d = json.load(sys.stdin)
print(d['claudeAiOauth']['refreshToken'])
"
```

### 2. Connect Claude Code

Add to `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:9999"
  }
}
```

Or use an alias to keep the original `claude` command available:

```bash
alias claudep='ANTHROPIC_BASE_URL=http://localhost:9999 claude'
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `CC_ACCOUNTS_FILE` | `config/accounts.json` | Path to accounts file |
| `CC_PROXY_PORT` | `9999` | Port to listen on |
| `CC_PROXY_BIND` | `127.0.0.1` | Bind address (`0.0.0.0` to expose on LAN) |
| `CC_LOG_PATH` | `logs/proxy.log` | JSONL log file |
| `CC_LOG_LEVEL` | — | Set to `debug` to log auth headers |

## How account selection works

Each account gets a water score (lower = more available):

```
water = max(5h_util × time_left / 300min,
            7d_util × time_left / 168hr)
```

Time-decay is the key insight: an account at 90% 5h utilization that resets in 10 minutes scores near 0 and floats to the top of the pool — the proxy will start using it the moment quota resets. An account is only hard-blocked when the API explicitly returns `rejected`; high utilization alone just raises the score.

With multiple accounts, the proxy switches proactively if a better option scores at least 10% lower than the current one.

## Status endpoint

```bash
curl -s http://localhost:9999/status | python3 -m json.tool
```

```json
{
  "active": "acct1",
  "uptime": "2h34m",
  "accounts": [
    {
      "name": "acct1",
      "isActive": true,
      "status": "allowed",
      "water": 0.133,
      "fiveHour": { "utilization": 0.0, "percent": "0%", "resetsInMins": 254 },
      "sevenDay":  { "utilization": 0.67, "percent": "67%", "resetsInHours": 33 },
      "tokenExpiresIn": "7h40m",
      "lastSeen": "just now"
    }
  ]
}
```

## Logs

Every significant event is written as a JSONL line to `logs/proxy.log`:

```
request           — account, latency, water score per request
account_switched  — switch reason and water scores before/after
429_received      — action taken: switch / hold / forward
rate_limit_update — utilization parsed from API response headers
token_refreshed   — OAuth token renewal
```
