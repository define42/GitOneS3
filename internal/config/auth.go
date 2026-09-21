package config

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
)

// Auth contains Google OIDC credentials and shared cookie keys.
// Keys are base64-encoded; the hash key is 64 bytes and the AES key is 32 bytes.
type Auth struct {
	Enabled            bool
	PublicURL          string
	GoogleClientID     string
	GoogleClientSecret string
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
		CookieHashKey:      value(lookup, "GITONE_COOKIE_HASH_KEY", ""),
		CookieBlockKey:     value(lookup, "GITONE_COOKIE_BLOCK_KEY", ""),
	}, nil
}

// Validate fails closed on incomplete or insecure enabled authentication.
func (a Auth) Validate() error {
	if !a.Enabled {
		if a.PublicURL != "" || a.GoogleClientID != "" || a.GoogleClientSecret != "" || a.CookieHashKey != "" || a.CookieBlockKey != "" {
			return errors.New("config: authentication settings require GITONE_AUTH_ENABLED=true")
		}
		return nil
	}
	u, err := url.Parse(a.PublicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("config: public URL must be an https origin without a path, credentials, query, or fragment")
	}
	if strings.TrimSpace(a.GoogleClientID) == "" || strings.TrimSpace(a.GoogleClientSecret) == "" {
		return errors.New("config: Google client ID and secret are required")
	}
	_, _, err = a.CookieKeys()
	return err
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
