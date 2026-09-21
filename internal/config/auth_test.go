package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func authEnvironment() map[string]string {
	env := baseEnvironment()
	env["GITONE_AUTH_ENABLED"] = "true"
	env["GITONE_PUBLIC_URL"] = "https://git.example"
	env["GITONE_GOOGLE_CLIENT_ID"] = "client"
	env["GITONE_GOOGLE_CLIENT_SECRET"] = "secret"
	env["GITONE_COOKIE_HASH_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("h", 64)))
	env["GITONE_COOKIE_BLOCK_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))
	return env
}

func TestLoadAuth(t *testing.T) {
	t.Parallel()
	cfg, err := Load(testLookup(authEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.Enabled || cfg.Auth.PublicURL != "https://git.example" || cfg.Auth.GoogleClientID != "client" {
		t.Fatal("auth configuration lost")
	}
	hash, block, err := cfg.Auth.CookieKeys()
	if err != nil || len(hash) != 64 || len(block) != 32 {
		t.Fatal("invalid decoded keys")
	}
}

func TestLoadRejectsInvalidAuth(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, key, value string }{
		{"disabled with settings", "GITONE_AUTH_ENABLED", "false"},
		{"invalid enabled flag", "GITONE_AUTH_ENABLED", "maybe"},
		{"http origin", "GITONE_PUBLIC_URL", "http://git.example"},
		{"origin path", "GITONE_PUBLIC_URL", "https://git.example/"},
		{"origin query", "GITONE_PUBLIC_URL", "https://git.example?next=evil"},
		{"origin credentials", "GITONE_PUBLIC_URL", "https://user:password@git.example"},
		{"missing ID", "GITONE_GOOGLE_CLIENT_ID", ""},
		{"missing secret", "GITONE_GOOGLE_CLIENT_SECRET", ""},
		{"short hash key", "GITONE_COOKIE_HASH_KEY", "YQ=="},
		{"bad encryption key", "GITONE_COOKIE_BLOCK_KEY", "not-base64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env := authEnvironment()
			env[test.key] = test.value
			if _, err := Load(testLookup(env)); err == nil {
				t.Fatal("invalid authentication configuration accepted")
			}
		})
	}
}
