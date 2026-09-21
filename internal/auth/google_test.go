package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

func TestGoogleExchangeVerifiesIDToken(t *testing.T) {
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
			google := &Google{
				oauth:    oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: provider.Endpoint(), RedirectURL: "https://git.example" + CallbackPath},
				verifier: provider.Verifier(&oidc.Config{ClientID: "client", SupportedSigningAlgs: []string{oidc.RS256}}),
				client:   server.Client(),
			}
			identity, err := google.Exchange(context.Background(), "code", "pkce-verifier", "expected-nonce")
			if name == "valid" {
				if err != nil || identity.Subject != "google-user" {
					t.Fatalf("valid identity rejected: %+v, %v", identity, err)
				}
			} else if err == nil {
				t.Fatalf("%s token accepted", name)
			}
		})
	}
}
