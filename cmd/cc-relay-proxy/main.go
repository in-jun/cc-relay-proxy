package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
	"github.com/in-jun/cc-relay-proxy/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run initialises and starts the proxy. It blocks until a signal or server error.
func run() error {
	accountsFile := envOrDefault("CC_ACCOUNTS_FILE", "config/accounts.json")
	port := envOrDefault("CC_PROXY_PORT", "9999")
	bindAddr := envOrDefault("CC_PROXY_BIND", "127.0.0.1")
	logPath := envOrDefault("CC_LOG_PATH", "logs/proxy.log")

	// Parse accounts from file first — fail fast before touching the log file
	accts, err := accounts.ParseAccountsFile(accountsFile)
	if err != nil {
		return fmt.Errorf("accounts: %w", err)
	}

	// Initialize logger
	l, err := logger.New(logPath)
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	defer l.Close()

	pool := accounts.NewPool(accts)

	l.Log("startup", "", map[string]any{
		"numAccounts": len(accts),
		"listenAddr":  bindAddr + ":" + port,
	})

	// Wire refresh callbacks so token refreshes appear in the log
	pool.SetRefreshCallback(func(name string, expiresInMins int) {
		l.Log("token_refreshed", name, map[string]any{"expiresInMins": expiresInMins})
	})

	// Persist rotated tokens back to accounts file so restarts don't lose them
	pool.WatchRotations(accountsFile)

	listenAddr := bindAddr + ":" + port
	log.Printf("[cc-relay-proxy] starting on %s with %d account(s)", listenAddr, len(accts))

	srv := proxy.New(pool, l)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// StartupPing must complete before Run begins so that the reset-watcher and
	// inactive-ping ticker see fresh account state rather than the neutral startup
	// values. Both run in one goroutine so the HTTP server is not blocked.
	go func() {
		srv.Pinger().StartupPing(ctx)
		srv.Pinger().Run(ctx)
	}()

	httpSrv := &http.Server{
		Addr:         listenAddr,
		Handler:      srv.Handler(),
		ReadTimeout:  0, // streaming; no read timeout
		WriteTimeout: 0, // streaming; no write timeout
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("[cc-relay-proxy] shutting down")
		l.Log("shutdown", "", nil)
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		httpSrv.Shutdown(shutCtx)
		cancel()
	}()

	addr := fmt.Sprintf("http://%s", listenAddr)
	log.Printf("[cc-relay-proxy] listening at %s — set ANTHROPIC_BASE_URL=%s", addr, addr)

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
