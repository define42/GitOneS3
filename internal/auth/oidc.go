// Package auth authenticates users at their namespace's owning shard.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/define42/GitOneS3/internal/config"
)

// CallbackPath is retained for existing Google clients. Generic providers use
// the callback selected by config.Auth.CallbackPath.
const CallbackPath = "/auth/google/callback"

// Identity binds a subject to its issuing provider. An absent issuer in legacy
// records means Google; email is display information, never an account key.
type Identity struct {
	Issuer  string `json:"issuer,omitempty"`
	Subject string `json:"subject"`
	Email   string `json:"email"`
}

// Provider is the authentication service's consumer-facing OIDC boundary.
type Provider interface {
	AuthorizationURL(state, nonce, verifier string) string
	Exchange(context.Context, string, string, string) (Identity, error)
}

// OIDC implements authorization code flow with ID token verification and PKCE.
type OIDC struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
	issuer   string
}

func NewOIDC(ctx context.Context, cfg config.Auth) (*OIDC, error) {
	return newOIDC(ctx, cfg, &http.Client{Timeout: 15 * time.Second})
}

func newOIDC(ctx context.Context, cfg config.Auth, client *http.Client) (*OIDC, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errors.New("OIDC authentication is disabled")
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), cfg.IssuerURL())
	if err != nil {
		return nil, errors.New("OIDC discovery failed")
	}
	clientID, clientSecret := cfg.ClientCredentials()
	return &OIDC{
		oauth: oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret,
			Endpoint: provider.Endpoint(), RedirectURL: cfg.PublicURL + cfg.CallbackPath(),
			Scopes: []string{oidc.ScopeOpenID, "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID, SupportedSigningAlgs: []string{oidc.RS256}}),
		client:   client,
		issuer:   cfg.IssuerURL(),
	}, nil
}

func (g *OIDC) AuthorizationURL(state, nonce, verifier string) string {
	return g.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

func (g *OIDC) Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error) {
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, g.client), 20*time.Second)
	defer cancel()
	token, err := g.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, errors.New("OIDC code exchange failed")
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return Identity{}, errors.New("OIDC response has no ID token")
	}
	idToken, err := g.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, errors.New("OIDC ID token verification failed")
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 || idToken.Subject == "" {
		return Identity{}, errors.New("invalid OIDC identity or nonce")
	}
	var claims struct {
		Email           string `json:"email"`
		EmailVerified   bool   `json:"email_verified"`
		AuthorizedParty string `json:"azp"`
	}
	if err := idToken.Claims(&claims); err != nil || !claims.EmailVerified || claims.Email == "" {
		return Identity{}, errors.New("verified email is required")
	}
	if claims.AuthorizedParty != "" && claims.AuthorizedParty != g.oauth.ClientID {
		return Identity{}, errors.New("invalid OIDC authorized party")
	}
	identity := Identity{Issuer: g.issuer, Subject: idToken.Subject, Email: claims.Email}
	// Preserve the legacy Google record and cookie format.
	if identity.Issuer == config.GoogleIssuer {
		identity.Issuer = ""
	}
	return identity, nil
}
