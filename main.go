package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/in-jun/cc-relay-proxy/internal/accounts"
	"github.com/in-jun/cc-relay-proxy/internal/logger"
	"github.com/in-jun/cc-relay-proxy/internal/proxy"
	"github.com/in-jun/cc-relay-proxy/internal/tuner"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run initialises and starts the proxy. It blocks until a signal or server error.
func run() error {
	// Parse configuration from environment
	ccAccounts := os.Getenv("CC_ACCOUNTS")
	if ccAccounts == "" {
		return fmt.Errorf("CC_ACCOUNTS is required (JSON array of {name, refreshToken[, accessToken, expiresAt]})")
	}

	port := envOrDefault("CC_PROXY_PORT", "9999")
	logPath := envOrDefault("CC_LOG_PATH", "logs/proxy.log")

	tuneIntervalSec := 3600
	if v := os.Getenv("CC_TUNE_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tuneIntervalSec = n
		}
	}

	// Initialize logger
	l, err := logger.New(logPath)
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	defer l.Close()

	// Parse accounts
	accts, err := accounts.ParseAccounts(ccAccounts)
	if err != nil {
		return fmt.Errorf("accounts: %w", err)
	}

	pool := accounts.NewPool(accts)
	params := pool.Params()

	l.Log("startup", "", map[string]any{
		"numAccounts": len(accts),
		"port":        port,
		"params": map[string]any{
			"switchThreshold5h": params.SwitchThreshold5h,
			"hardBlock7d":       params.HardBlock7d,
			"weight5h":          params.Weight5h,
			"weight7d":          params.Weight7d,
		},
	})

	// Wire refresh callbacks so token refreshes appear in the log
	pool.SetRefreshCallback(func(name string, expiresInMins int) {
		l.Log("token_refreshed", name, map[string]any{"expiresInMins": expiresInMins})
	})

	log.Printf("[cc-relay-proxy] starting on :%s with %d account(s)", port, len(accts))

	// Build components
	srv := proxy.New(pool, l)
	tuneInterval := time.Duration(tuneIntervalSec) * time.Second
	t := tuner.New(pool, l, tuneInterval)
	srv.Pinger().SetTuner(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Startup pings (background)
	go srv.Pinger().StartupPing(ctx)

	// Periodic pings
	go srv.Pinger().Run(ctx)

	// Auto-tuner
	go t.Run(ctx)

	// HTTP server
	httpSrv := &http.Server{
		Addr:         ":" + port,
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

	addr := fmt.Sprintf("http://localhost:%s", port)
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
