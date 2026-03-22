package main

import (
	"fmt"
	"net"
	"os"
	"testing"
)

func TestEnvOrDefault(t *testing.T) {
	const key = "CC_RELAY_PROXY_TEST_VAR"

	// Variable not set → returns default.
	os.Unsetenv(key)
	if got := envOrDefault(key, "fallback"); got != "fallback" {
		t.Errorf("want fallback, got %q", got)
	}

	// Variable set → returns its value.
	os.Setenv(key, "custom")
	defer os.Unsetenv(key)
	if got := envOrDefault(key, "fallback"); got != "custom" {
		t.Errorf("want custom, got %q", got)
	}
}

func TestRunMissingCCAccounts(t *testing.T) {
	// run() returns an error when CC_ACCOUNTS is not set.
	os.Unsetenv("CC_ACCOUNTS")
	err := run()
	if err == nil {
		t.Fatal("expected error when CC_ACCOUNTS is missing")
	}
}

func TestRunInvalidCCAccounts(t *testing.T) {
	// run() returns an error when CC_ACCOUNTS is invalid JSON.
	os.Setenv("CC_ACCOUNTS", "not-valid-json")
	defer os.Unsetenv("CC_ACCOUNTS")
	err := run()
	if err == nil {
		t.Fatal("expected error for invalid CC_ACCOUNTS JSON")
	}
}

func TestRunLoggerFails(t *testing.T) {
	// run() returns an error when the logger cannot be created.
	// Use an existing file as the directory component so MkdirAll fails.
	f, err := os.CreateTemp("", "main-test-not-a-dir-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	os.Setenv("CC_ACCOUNTS", `[{"name":"a1","refreshToken":"rt1"}]`)
	os.Setenv("CC_LOG_PATH", f.Name()+"/sub/log.jsonl") // f.Name() is a file, not a dir
	defer func() {
		os.Unsetenv("CC_ACCOUNTS")
		os.Unsetenv("CC_LOG_PATH")
	}()

	err = run()
	if err == nil {
		t.Fatal("expected error when logger init fails")
	}
}

func TestRunTuneIntervalParsed(t *testing.T) {
	// run() with CC_TUNE_INTERVAL set to a valid integer path (covers
	// the strconv.Atoi branch). We trigger an early return via invalid logger path.
	f, err := os.CreateTemp("", "main-test-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	os.Setenv("CC_ACCOUNTS", `[{"name":"a1","refreshToken":"rt1"}]`)
	os.Setenv("CC_TUNE_INTERVAL", "7200")
	os.Setenv("CC_LOG_PATH", f.Name()+"/sub/log.jsonl")
	defer func() {
		os.Unsetenv("CC_ACCOUNTS")
		os.Unsetenv("CC_TUNE_INTERVAL")
		os.Unsetenv("CC_LOG_PATH")
	}()

	err = run()
	if err == nil {
		t.Fatal("expected error when logger init fails")
	}
}

func TestRunPortInUse(t *testing.T) {
	// Occupy a port so ListenAndServe fails immediately, covering the "happy path"
	// startup code (pool setup, logger, pinger wiring) through to the server error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// Temp log file in a writable location.
	f, err := os.CreateTemp("", "main-test-log-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	os.Setenv("CC_ACCOUNTS", `[{"name":"a1","refreshToken":"rt1","accessToken":"tok1","expiresAt":9999999999999}]`)
	os.Setenv("CC_PROXY_PORT", fmt.Sprintf("%d", port))
	os.Setenv("CC_LOG_PATH", f.Name())
	defer func() {
		os.Unsetenv("CC_ACCOUNTS")
		os.Unsetenv("CC_PROXY_PORT")
		os.Unsetenv("CC_LOG_PATH")
	}()

	err = run()
	if err == nil {
		t.Fatal("expected error when port is already in use")
	}
}

func TestRunTuneIntervalInvalid(t *testing.T) {
	// run() with CC_TUNE_INTERVAL set to a non-integer: strconv.Atoi fails,
	// tuneIntervalSec stays at default. Early return via bad log path.
	f, err := os.CreateTemp("", "main-test-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	os.Setenv("CC_ACCOUNTS", `[{"name":"a1","refreshToken":"rt1"}]`)
	os.Setenv("CC_TUNE_INTERVAL", "not-a-number")
	os.Setenv("CC_LOG_PATH", f.Name()+"/sub/log.jsonl")
	defer func() {
		os.Unsetenv("CC_ACCOUNTS")
		os.Unsetenv("CC_TUNE_INTERVAL")
		os.Unsetenv("CC_LOG_PATH")
	}()

	err = run()
	if err == nil {
		t.Fatal("expected error when logger init fails")
	}
}
