FROM golang:1.21-alpine AS builder

WORKDIR /src
COPY go.mod .
COPY cmd/ cmd/
COPY internal/ internal/

RUN go build -o /cc-relay-proxy ./cmd/cc-relay-proxy

FROM alpine:3.19

RUN apk add --no-cache ca-certificates && \
    adduser -D -H -s /sbin/nologin proxy

WORKDIR /app

COPY --from=builder /cc-relay-proxy /usr/local/bin/cc-relay-proxy

RUN mkdir -p /app/logs && chown proxy:proxy /app/logs

USER proxy

EXPOSE 9999

ENV CC_PROXY_PORT=9999 \
    CC_PROXY_BIND=0.0.0.0 \
    CC_ACCOUNTS_FILE=/app/accounts.json \
    CC_LOG_PATH=/app/logs/proxy.log

ENTRYPOINT ["cc-relay-proxy"]
