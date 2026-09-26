package gittransport

import (
	"context"
	"time"
)

type lfsCredentialExpiryKey struct{}

// WithLFSCredentialExpiry records the admission expiry of a validated SSH LFS
// credential so batch actions can advertise the same lifetime to native clients.
func WithLFSCredentialExpiry(ctx context.Context, expires time.Time) context.Context {
	return context.WithValue(ctx, lfsCredentialExpiryKey{}, expires)
}

// LFSCredentialExpiry returns the validated SSH credential's admission expiry.
// An already admitted transfer remains subject to its own deadline and live ACL.
func LFSCredentialExpiry(ctx context.Context) (time.Time, bool) {
	expires, ok := ctx.Value(lfsCredentialExpiryKey{}).(time.Time)
	return expires, ok && !expires.IsZero()
}
