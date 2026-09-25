package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/define42/GitOneS3/internal/config"
)

func TestOIDCAuthorizationRequestsAccountInteraction(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		issuer   string
		callback string
		prompt   string
	}{
		{
			name: "Google account chooser", issuer: config.GoogleIssuer,
			callback: CallbackPath, prompt: "select_account",
		},
		{
			name: "Keycloak login", issuer: "https://keycloak.example/realms/gitone",
			callback: "/auth/oidc/callback", prompt: "login",
		},
		{
			name: "generic OIDC login", issuer: "https://identity.example",
			callback: "/auth/oidc/callback", prompt: "login",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provider := &OIDC{
				issuer: test.issuer,
				oauth: oauth2.Config{
					ClientID: "gitone", RedirectURL: "https://git.example" + test.callback,
					Endpoint: oauth2.Endpoint{AuthURL: "https://identity.example/authorize"},
					Scopes:   []string{oidc.ScopeOpenID, "email"},
				},
			}
			location, err := url.Parse(provider.AuthorizationURL("signed-state+&", "nonce", "verifier"))
			if err != nil {
				t.Fatal(err)
			}
			query := location.Query()
			for name, want := range map[string]string{
				"prompt": test.prompt, "state": "signed-state+&", "nonce": "nonce",
				"client_id": "gitone", "redirect_uri": provider.oauth.RedirectURL,
				"response_type": "code", "scope": "openid email", "code_challenge_method": "S256",
			} {
				if values := query[name]; len(values) != 1 || values[0] != want {
					t.Errorf("authorization parameter %s = %q, want %q", name, values, want)
				}
			}
			if query.Get("code_challenge") == "" || query.Has("code_verifier") {
				t.Fatal("authorization must include the PKCE challenge without exposing the verifier")
			}
		})
	}
}

func TestOIDCExchangeVerifiesIDToken(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid", "wrong signature", "wrong issuer", "wrong audience", "expired", "wrong nonce", "unverified email", "missing subject", "wrong authorized party", "missing ID token", "exchange rejected"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var issuer string
			mux := http.NewServeMux()
			mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"issuer": issuer, "authorization_endpoint": issuer + "/auth",
					"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys",
					"id_token_signing_alg_values_supported": []string{"RS256"},
				})
			})
			mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
					Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig",
				}}})
			})
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.PostForm.Get("code") != "code" || r.PostForm.Get("code_verifier") != "pkce-verifier" ||
					r.PostForm.Get("redirect_uri") != "https://git.example"+CallbackPath {
					t.Error("code exchange lost PKCE or fixed callback")
					http.Error(w, "bad request", 400)
					return
				}
				if name == "exchange rejected" {
					http.Error(w, "denied", 400)
					return
				}
				claims := map[string]any{"iss": issuer, "aud": "client", "sub": "google-user",
					"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
					"nonce": "expected-nonce", "email": "user@example.com", "email_verified": true}
				switch name {
				case "wrong issuer":
					claims["iss"] = "https://evil.example"
				case "wrong audience":
					claims["aud"] = "other-client"
				case "expired":
					claims["exp"] = time.Now().Add(-time.Hour).Unix()
				case "wrong nonce":
					claims["nonce"] = "wrong"
				case "unverified email":
					claims["email_verified"] = false
				case "missing subject":
					delete(claims, "sub")
				case "wrong authorized party":
					claims["azp"] = "other-client"
				}
				signingKey := key
				if name == "wrong signature" {
					signingKey = otherKey
				}
				signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: signingKey}, (&jose.SignerOptions{}).WithHeader("kid", "test"))
				if err != nil {
					t.Error(err)
					http.Error(w, "signing failure", 500)
					return
				}
				data, _ := json.Marshal(claims)
				signed, err := signer.Sign(data)
				if err != nil {
					t.Error(err)
					http.Error(w, "signing failure", 500)
					return
				}
				raw, err := signed.CompactSerialize()
				if err != nil {
					t.Error(err)
					http.Error(w, "signing failure", 500)
					return
				}
				response := map[string]any{"access_token": "access", "token_type": "Bearer", "id_token": raw}
				if name == "missing ID token" {
					delete(response, "id_token")
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			issuer = server.URL
			provider, err := oidc.NewProvider(context.Background(), issuer)
			if err != nil {
				t.Fatal(err)
			}
			google := &OIDC{
				oauth:    oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: provider.Endpoint(), RedirectURL: "https://git.example" + CallbackPath},
				verifier: provider.Verifier(&oidc.Config{ClientID: "client", SupportedSigningAlgs: []string{oidc.RS256}}),
				client:   server.Client(),
				issuer:   issuer,
			}
			identity, err := google.Exchange(context.Background(), "code", "pkce-verifier", "expected-nonce")
			if name == "valid" {
				if err != nil || identity.Subject != "google-user" || identity.Issuer != issuer {
					t.Fatalf("valid identity rejected: %+v, %v", identity, err)
				}
			} else if err == nil {
				t.Fatalf("%s token accepted", name)
			}
		})
	}
}

func TestOIDCDiscoveryAndAuthorization(t *testing.T) {
	t.Parallel()
	for _, mismatch := range []bool{false, true} {
		name := "configured issuer"
		if mismatch {
			name = "issuer mismatch rejected"
		}
		t.Run(name, func(t *testing.T) {
			var issuer string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/realms/gitone/.well-known/openid-configuration" {
					http.NotFound(w, r)
					return
				}
				discoveredIssuer := issuer
				if mismatch {
					discoveredIssuer = "https://other.example/realms/gitone"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"issuer": discoveredIssuer, "authorization_endpoint": issuer + "/auth",
					"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys",
					"id_token_signing_alg_values_supported": []string{"RS256"},
				})
			}))
			defer server.Close()
			issuer = server.URL + "/realms/gitone"
			cfg := testConfig()
			cfg.GoogleClientID, cfg.GoogleClientSecret = "", ""
			cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret = issuer, "gitone", "secret"
			provider, err := newOIDC(context.Background(), cfg, server.Client())
			if mismatch {
				if err == nil {
					t.Fatal("mismatched discovery issuer accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			location, err := url.Parse(provider.AuthorizationURL("state", "nonce", "verifier"))
			if err != nil {
				t.Fatal(err)
			}
			query := location.Query()
			if query.Get("prompt") != "login" {
				t.Fatal("discovered OIDC provider must request interactive login")
			}
			if query.Get("redirect_uri") != cfg.PublicURL+"/auth/oidc/callback" ||
				query.Get("client_id") != "gitone" || query.Get("code_challenge_method") != "S256" ||
				query.Get("nonce") != "nonce" || query.Get("state") != "state" || query.Get("response_type") != "code" {
				t.Fatal("OIDC authorization lost callback, client, or CSRF/PKCE parameters")
			}
		})
	}
	legacy := testConfig()
	if legacy.IssuerURL() != config.GoogleIssuer || legacy.CallbackPath() != CallbackPath {
		t.Fatal("legacy Google configuration changed")
	}
}
