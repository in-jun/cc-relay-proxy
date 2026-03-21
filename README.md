# cc-relay-proxy

A local reverse proxy for Claude Code that automatically rotates between multiple accounts to avoid rate limits.

## Quick Start

```bash
go build -o cc-relay-proxy .

CC_ACCOUNTS='[{"name":"acct1","refreshToken":"rt_..."},{"name":"acct2","refreshToken":"rt_..."}]' \
./cc-relay-proxy
```

Get your refresh token:
```bash
cat ~/.claude/.credentials.json | python3 -c "
import json,sys; d=json.load(sys.stdin)
print(d['claudeAiOauth']['refreshToken'])
"
```

Connect Claude Code (`~/.claude/settings.json`):
```json
{ "env": { "ANTHROPIC_BASE_URL": "http://localhost:9999" } }
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CC_ACCOUNTS` | required | JSON array of accounts |
| `CC_PROXY_PORT` | `9999` | Proxy port |
| `CC_LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `CC_TUNE_INTERVAL` | `3600` | Auto-tuning interval (seconds) |

## Status

```bash
curl http://localhost:9999/status
```
