package proxy

import (
	"errors"
	"fmt"
	"net/http"
)

var ErrInvalidInternalAuthConfig = errors.New("proxy: invalid internal authentication configuration")

// InternalAuth is a fail-closed authentication boundary for the internal
// listener. Place it in front of Handler so only one authenticated forwarding
// hop can reach shard routing on that listener.
type InternalAuth struct {
	next          http.Handler
	internalToken []byte
}

// NewInternalAuth constructs internal-listener authentication middleware.
func NewInternalAuth(next http.Handler, internalToken string) (*InternalAuth, error) {
	if next == nil {
		return nil, fmt.Errorf("%w: next handler is required", ErrInvalidInternalAuthConfig)
	}
	if !validInternalToken(internalToken) {
		return nil, fmt.Errorf(
			"%w: internal token must contain at least %d visible ASCII bytes",
			ErrInvalidInternalAuthConfig,
			MinimumInternalTokenSize,
		)
	}
	return &InternalAuth{
		next:          next,
		internalToken: []byte(internalToken),
	}, nil
}

func validInternalToken(token string) bool {
	if len(token) < MinimumInternalTokenSize {
		return false
	}
	for index := 0; index < len(token); index++ {
		if token[index] < 0x21 || token[index] > 0x7e {
			return false
		}
	}
	return true
}

// ServeHTTP rejects unauthenticated requests before they reach shard routing.
func (a *InternalAuth) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request == nil || !hasAuthenticatedForward(request.Header, a.internalToken) {
		http.Error(response, "forbidden", http.StatusForbidden)
		return
	}
	a.next.ServeHTTP(response, request)
}
