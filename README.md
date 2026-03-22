# cc-relay-proxy

Claude Code의 API 요청을 중간에서 가로채 여러 Anthropic 계정을 자동으로 돌려가며 사용하는 로컬 프록시. 429(Rate Limit)가 오면 다른 계정으로 즉시 전환해 Claude Code에는 보이지 않게 처리한다.

```
Claude Code  →  cc-relay-proxy :9999  →  api.anthropic.com
                 ├─ 최적 계정 선택 (water-fill 알고리즘)
                 ├─ 429 오면 다른 계정으로 자동 전환
                 └─ GET /status  (계정 상태 확인)
```

## 설치

### Docker (권장)

```bash
docker run -d \
  -p 9999:9999 \
  -v $(pwd)/accounts.json:/app/accounts.json:ro \
  -v $(pwd)/logs:/app/logs \
  --name cc-relay-proxy \
  injundev/cc-relay-proxy
```

### 직접 빌드

```bash
git clone https://github.com/in-jun/cc-relay-proxy
cd cc-relay-proxy
go build -o cc-relay-proxy ./cmd/cc-relay-proxy
```

Go 1.21 이상, 외부 의존성 없음.

## 설정

### 1. accounts.json 만들기

```json
[
  {"name": "acct1", "refreshToken": "rt_..."},
  {"name": "acct2", "refreshToken": "rt_..."}
]
```

refresh token은 로컬 Claude Code 로그인 정보에서 가져온다:

```bash
cat ~/.claude/.credentials.json | python3 -c "
import json, sys
d = json.load(sys.stdin)
print(d['claudeAiOauth']['refreshToken'])
"
```

### 2. Claude Code 연결

`~/.claude/settings.json`에 추가:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:9999"
  }
}
```

또는 별칭으로 쓰기:

```bash
alias claudep='ANTHROPIC_BASE_URL=http://localhost:9999 claude'
```

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `CC_ACCOUNTS_FILE` | `accounts.json` | 계정 파일 경로 |
| `CC_PROXY_PORT` | `9999` | 포트 |
| `CC_PROXY_BIND` | `127.0.0.1` | 바인드 주소 |
| `CC_LOG_PATH` | `logs/proxy.log` | 로그 파일 |
| `CC_LOG_LEVEL` | — | `debug`로 설정하면 인증 헤더 로깅 |

## 계정 선택 알고리즘

각 계정에 water score(낮을수록 좋음)를 계산해 가장 여유 있는 계정을 선택한다:

```
water = max(5h_사용률 × 남은시간/300분,
            7d_사용률 × 남은시간/168시간)
```

time-decay가 핵심 — 5h 사용률이 90%여도 10분 후 리셋되면 score가 거의 0에 가까워져 자동으로 우선순위가 올라간다. 계정이 여럿일 때 현재 계정보다 10% 이상 score가 낮은 계정이 있으면 미리 전환한다.

API가 명시적으로 `rejected`를 반환할 때만 해당 계정을 제외하고, 나머지는 모두 score로만 판단한다.

## 상태 확인

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

## 로그

`logs/proxy.log`에 JSONL 형식으로 기록된다. 주요 이벤트:

```
request          — 각 요청의 계정, 레이턴시, water score
account_switched — 계정 전환 (이유 포함)
429_received     — 429 처리 결과 (switch / hold / forward)
rate_limit_update — API 응답에서 파싱한 사용률
token_refreshed  — OAuth 토큰 갱신
```
