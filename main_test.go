package main

import (
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
