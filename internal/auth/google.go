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

const CallbackPath = "/auth/google/callback"

// Identity uses Google's immutable subject; email is display information only.
type Identity struct {
	Subject string `json:"subject"`
	Email   string `json:"email"`
}

// Provider is the authentication service's consumer-facing OIDC boundary.
type Provider interface {
	AuthorizationURL(state, nonce, verifier string) string
	Exchange(context.Context, string, string, string) (Identity, error)
}

// Google implements authorization code flow with OIDC verification and PKCE.
type Google struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

func NewGoogle(ctx context.Context, cfg config.Auth) (*Google, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errors.New("Google authentication is disabled")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), "https://accounts.google.com")
	if err != nil {
		return nil, errors.New("Google OIDC discovery failed")
	}
	return &Google{
		oauth: oauth2.Config{
			ClientID: cfg.GoogleClientID, ClientSecret: cfg.GoogleClientSecret,
			Endpoint: provider.Endpoint(), RedirectURL: cfg.PublicURL + CallbackPath,
			Scopes: []string{oidc.ScopeOpenID, "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.GoogleClientID, SupportedSigningAlgs: []string{oidc.RS256}}),
		client:   client,
	}, nil
}

func (g *Google) AuthorizationURL(state, nonce, verifier string) string {
	return g.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

func (g *Google) Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error) {
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, g.client), 20*time.Second)
	defer cancel()
	token, err := g.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, errors.New("Google code exchange failed")
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return Identity{}, errors.New("Google response has no ID token")
	}
	idToken, err := g.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, errors.New("Google ID token verification failed")
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 || idToken.Subject == "" {
		return Identity{}, errors.New("invalid Google identity or nonce")
	}
	var claims struct {
		Email           string `json:"email"`
		EmailVerified   bool   `json:"email_verified"`
		AuthorizedParty string `json:"azp"`
	}
	if err := idToken.Claims(&claims); err != nil || !claims.EmailVerified || claims.Email == "" {
		return Identity{}, errors.New("verified Google email is required")
	}
	if claims.AuthorizedParty != "" && claims.AuthorizedParty != g.oauth.ClientID {
		return Identity{}, errors.New("invalid Google authorized party")
	}
	return Identity{Subject: idToken.Subject, Email: claims.Email}, nil
}
