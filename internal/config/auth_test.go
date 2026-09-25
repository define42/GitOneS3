package config

import (
	"encoding/base64"
	"maps"
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

func TestLoadOIDC(t *testing.T) {
	t.Parallel()
	env := authEnvironment()
	delete(env, "GITONE_GOOGLE_CLIENT_ID")
	delete(env, "GITONE_GOOGLE_CLIENT_SECRET")
	env["GITONE_OIDC_ISSUER"] = "https://keycloak.example/realms/gitone"
	env["GITONE_OIDC_CLIENT_ID"] = "gitone"
	env["GITONE_OIDC_CLIENT_SECRET"] = "local-secret"
	cfg, err := Load(testLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	id, secret := cfg.Auth.ClientCredentials()
	if cfg.Auth.IssuerURL() != env["GITONE_OIDC_ISSUER"] || id != "gitone" || secret != "local-secret" ||
		cfg.Auth.CallbackPath() != "/auth/oidc/callback" {
		t.Fatal("OIDC configuration was not preserved")
	}
	for _, test := range []struct{ name, key, value string }{
		{name: "missing issuer", key: "GITONE_OIDC_ISSUER", value: ""},
		{name: "insecure issuer", key: "GITONE_OIDC_ISSUER", value: "http://keycloak/realms/gitone"},
		// #nosec G101 -- Deliberately invalid test credentials verify that issuer URLs reject userinfo.
		{name: "issuer credentials", key: "GITONE_OIDC_ISSUER", value: "https://user:pass@example.com"},
		{name: "issuer query", key: "GITONE_OIDC_ISSUER", value: "https://example.com?realm=gitone"},
		{name: "issuer fragment", key: "GITONE_OIDC_ISSUER", value: "https://example.com/#realm"},
		{name: "missing client", key: "GITONE_OIDC_CLIENT_ID", value: ""},
		{name: "missing secret", key: "GITONE_OIDC_CLIENT_SECRET", value: ""},
		{name: "mixed Google config", key: "GITONE_GOOGLE_CLIENT_ID", value: "google-client"},
		{name: "disabled OIDC", key: "GITONE_AUTH_ENABLED", value: "false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			modified := maps.Clone(env)
			modified[test.key] = test.value
			if _, err := Load(testLookup(modified)); err == nil {
				t.Fatal("invalid OIDC configuration accepted")
			}
		})
	}
}
