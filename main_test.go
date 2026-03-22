package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

func writeAccountsFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "cc-accounts-*.json")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func TestEnvOrDefault(t *testing.T) {
	const key = "CC_RELAY_PROXY_TEST_VAR"

	os.Unsetenv(key)
	if got := envOrDefault(key, "fallback"); got != "fallback" {
		t.Errorf("want fallback, got %q", got)
	}

	os.Setenv(key, "custom")
	defer os.Unsetenv(key)
	if got := envOrDefault(key, "fallback"); got != "custom" {
		t.Errorf("want custom, got %q", got)
	}
}

func TestRunMissingAccountsFile(t *testing.T) {
	// run() returns an error when CC_ACCOUNTS_FILE points to a non-existent file.
	os.Setenv("CC_ACCOUNTS_FILE", "/tmp/does-not-exist-cc-accounts.json")
	defer os.Unsetenv("CC_ACCOUNTS_FILE")
	if err := run(); err == nil {
		t.Fatal("expected error when accounts file is missing")
	}
}

func TestRunInvalidAccountsFile(t *testing.T) {
	// run() returns an error when the accounts file contains invalid JSON.
	path := writeAccountsFile(t, "not-valid-json")
	os.Setenv("CC_ACCOUNTS_FILE", path)
	defer os.Unsetenv("CC_ACCOUNTS_FILE")
	if err := run(); err == nil {
		t.Fatal("expected error for invalid accounts JSON")
	}
}

func TestRunLoggerFails(t *testing.T) {
	// run() returns an error when the logger cannot be created.
	path := writeAccountsFile(t, `[{"name":"a1","refreshToken":"rt1"}]`)

	f, err := os.CreateTemp("", "main-test-not-a-dir-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	os.Setenv("CC_ACCOUNTS_FILE", path)
	os.Setenv("CC_LOG_PATH", f.Name()+"/sub/log.jsonl")
	defer func() {
		os.Unsetenv("CC_ACCOUNTS_FILE")
		os.Unsetenv("CC_LOG_PATH")
	}()

	if err := run(); err == nil {
		t.Fatal("expected error when logger init fails")
	}
}

func TestRunTuneIntervalParsed(t *testing.T) {
	path := writeAccountsFile(t, `[{"name":"a1","refreshToken":"rt1"}]`)

	f, err := os.CreateTemp("", "main-test-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	os.Setenv("CC_ACCOUNTS_FILE", path)
	os.Setenv("CC_TUNE_INTERVAL", "7200")
	os.Setenv("CC_LOG_PATH", f.Name()+"/sub/log.jsonl")
	defer func() {
		os.Unsetenv("CC_ACCOUNTS_FILE")
		os.Unsetenv("CC_TUNE_INTERVAL")
		os.Unsetenv("CC_LOG_PATH")
	}()

	if err := run(); err == nil {
		t.Fatal("expected error when logger init fails")
	}
}

func TestRunTuneIntervalInvalid(t *testing.T) {
	path := writeAccountsFile(t, `[{"name":"a1","refreshToken":"rt1"}]`)

	f, err := os.CreateTemp("", "main-test-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	os.Setenv("CC_ACCOUNTS_FILE", path)
	os.Setenv("CC_TUNE_INTERVAL", "not-a-number")
	os.Setenv("CC_LOG_PATH", f.Name()+"/sub/log.jsonl")
	defer func() {
		os.Unsetenv("CC_ACCOUNTS_FILE")
		os.Unsetenv("CC_TUNE_INTERVAL")
		os.Unsetenv("CC_LOG_PATH")
	}()

	if err := run(); err == nil {
		t.Fatal("expected error when logger init fails")
	}
}

func TestRunPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	path := writeAccountsFile(t, `[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`)

	f, err := os.CreateTemp("", "main-test-log-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	os.Setenv("CC_ACCOUNTS_FILE", path)
	os.Setenv("CC_PROXY_PORT", fmt.Sprintf("%d", port))
	os.Setenv("CC_LOG_PATH", f.Name())
	defer func() {
		os.Unsetenv("CC_ACCOUNTS_FILE")
		os.Unsetenv("CC_PROXY_PORT")
		os.Unsetenv("CC_LOG_PATH")
	}()

	if err := run(); err == nil {
		t.Fatal("expected error when port is already in use")
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	path := writeAccountsFile(t, `[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`)

	f, err := os.CreateTemp("", "main-test-log-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	os.Setenv("CC_ACCOUNTS_FILE", path)
	os.Setenv("CC_PROXY_PORT", fmt.Sprintf("%d", port))
	os.Setenv("CC_LOG_PATH", f.Name())
	defer func() {
		os.Unsetenv("CC_ACCOUNTS_FILE")
		os.Unsetenv("CC_PROXY_PORT")
		os.Unsetenv("CC_LOG_PATH")
	}()

	done := make(chan error, 1)
	go func() { done <- run() }()

	addr := fmt.Sprintf("http://127.0.0.1:%d/status", port)
	for i := 0; i < 50; i++ {
		resp, err := http.Get(addr)
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	syscall.Kill(os.Getpid(), syscall.SIGTERM)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run() returned error after graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("run() did not return within 5s after SIGTERM")
	}
}
