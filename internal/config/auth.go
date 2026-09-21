package config

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
)

// GoogleIssuer also identifies legacy records that predate explicit issuers.
const GoogleIssuer = "https://accounts.google.com"

// Auth contains OIDC credentials and shared cookie keys.
// Keys are base64-encoded; the hash key is 64 bytes and the AES key is 32 bytes.
type Auth struct {
	Enabled            bool
	PublicURL          string
	GoogleClientID     string
	GoogleClientSecret string
	OIDCIssuer         string
	OIDCClientID       string
	OIDCClientSecret   string
	CookieHashKey      string
	CookieBlockKey     string
}

func loadAuth(lookup LookupEnv) (Auth, error) {
	enabled, err := boolValue(lookup, "GITONE_AUTH_ENABLED", false)
	if err != nil {
		return Auth{}, err
	}
	return Auth{
		Enabled:            enabled,
		PublicURL:          value(lookup, "GITONE_PUBLIC_URL", ""),
		GoogleClientID:     value(lookup, "GITONE_GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: value(lookup, "GITONE_GOOGLE_CLIENT_SECRET", ""),
		OIDCIssuer:         value(lookup, "GITONE_OIDC_ISSUER", ""),
		OIDCClientID:       value(lookup, "GITONE_OIDC_CLIENT_ID", ""),
		OIDCClientSecret:   value(lookup, "GITONE_OIDC_CLIENT_SECRET", ""),
		CookieHashKey:      value(lookup, "GITONE_COOKIE_HASH_KEY", ""),
		CookieBlockKey:     value(lookup, "GITONE_COOKIE_BLOCK_KEY", ""),
	}, nil
}

// Validate fails closed on incomplete or insecure enabled authentication.
func (a Auth) Validate() error {
	googleConfigured := a.GoogleClientID != "" || a.GoogleClientSecret != ""
	oidcConfigured := a.OIDCIssuer != "" || a.OIDCClientID != "" || a.OIDCClientSecret != ""
	if !a.Enabled {
		if a.PublicURL != "" || googleConfigured || oidcConfigured || a.CookieHashKey != "" || a.CookieBlockKey != "" {
			return errors.New("config: authentication settings require GITONE_AUTH_ENABLED=true")
		}
		return nil
	}
	u, err := url.Parse(a.PublicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("config: public URL must be an https origin without a path, credentials, query, or fragment")
	}
	if oidcConfigured {
		if googleConfigured {
			return errors.New("config: configure either Google or generic OIDC credentials, not both")
		}
		if err := ValidateIssuer(a.OIDCIssuer); err != nil {
			return err
		}
		if strings.TrimSpace(a.OIDCClientID) == "" || strings.TrimSpace(a.OIDCClientSecret) == "" {
			return errors.New("config: OIDC client ID and secret are required")
		}
	} else if strings.TrimSpace(a.GoogleClientID) == "" || strings.TrimSpace(a.GoogleClientSecret) == "" {
		return errors.New("config: Google or OIDC client credentials are required")
	}
	_, _, err = a.CookieKeys()
	return err
}

// ValidateIssuer accepts a pinned HTTPS issuer, including a Keycloak realm path.
func ValidateIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || len(issuer) > 2048 {
		return errors.New("config: OIDC issuer must be an https URL without credentials, query, or fragment")
	}
	return nil
}

// IssuerURL keeps the pre-OIDC Google configuration backward compatible.
func (a Auth) IssuerURL() string {
	if a.OIDCIssuer != "" {
		return a.OIDCIssuer
	}
	return GoogleIssuer
}

// ClientCredentials selects the configured provider's client credentials.
func (a Auth) ClientCredentials() (string, string) {
	if a.OIDCIssuer != "" {
		return a.OIDCClientID, a.OIDCClientSecret
	}
	return a.GoogleClientID, a.GoogleClientSecret
}

// CallbackPath preserves the registered redirect URI for existing Google clients.
func (a Auth) CallbackPath() string {
	if a.OIDCIssuer != "" {
		return "/auth/oidc/callback"
	}
	return "/auth/google/callback"
}

// CookieKeys decodes the shared signing and encryption keys without exposing them in errors.
func (a Auth) CookieKeys() ([]byte, []byte, error) {
	hash, err := base64.StdEncoding.DecodeString(a.CookieHashKey)
	if err != nil || len(hash) != 64 {
		return nil, nil, errors.New("config: cookie hash key must be base64 encoding of 64 random bytes")
	}
	block, err := base64.StdEncoding.DecodeString(a.CookieBlockKey)
	if err != nil || len(block) != 32 {
		return nil, nil, errors.New("config: cookie block key must be base64 encoding of 32 random bytes")
	}
	return hash, block, nil
}
