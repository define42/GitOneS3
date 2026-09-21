package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewInternalAuthValidatesOptions(t *testing.T) {
	t.Parallel()

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if _, err := NewInternalAuth(nil, testInternalToken); !errors.Is(err, ErrInvalidInternalAuthConfig) {
		t.Fatalf("NewInternalAuth(nil) error = %v, expected config error", err)
	}
	if _, err := NewInternalAuth(next, "short"); !errors.Is(err, ErrInvalidInternalAuthConfig) {
		t.Fatalf("NewInternalAuth(short token) error = %v, expected config error", err)
	}
	if _, err := NewInternalAuth(next, testInternalToken+"\x00"); !errors.Is(err, ErrInvalidInternalAuthConfig) {
		t.Fatalf("NewInternalAuth(control token) error = %v, expected config error", err)
	}
}

func TestInternalAuthRequiresExactlyOneValidMarkerAndToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers http.Header
	}{
		{name: "missing headers", headers: make(http.Header)},
		{
			name: "marker only",
			headers: http.Header{
				ForwardedHeader: []string{ForwardedHeaderValue},
			},
		},
		{
			name: "token only",
			headers: http.Header{
				InternalTokenHeader: []string{testInternalToken},
			},
		},
		{
			name: "wrong marker",
			headers: http.Header{
				ForwardedHeader:     []string{"2"},
				InternalTokenHeader: []string{testInternalToken},
			},
		},
		{
			name: "wrong token",
			headers: http.Header{
				ForwardedHeader:     []string{ForwardedHeaderValue},
				InternalTokenHeader: []string{"0123456789abcdef0123456789abcdee"},
			},
		},
		{
			name: "duplicate marker",
			headers: http.Header{
				ForwardedHeader:     []string{ForwardedHeaderValue, ForwardedHeaderValue},
				InternalTokenHeader: []string{testInternalToken},
			},
		},
		{
			name: "duplicate token",
			headers: http.Header{
				ForwardedHeader:     []string{ForwardedHeaderValue},
				InternalTokenHeader: []string{testInternalToken, testInternalToken},
			},
		},
		{
			name: "duplicate marker with noncanonical case",
			headers: http.Header{
				ForwardedHeader:      []string{ForwardedHeaderValue},
				"x-gitone-forwarded": []string{ForwardedHeaderValue},
				InternalTokenHeader:  []string{testInternalToken},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var nextCalled bool
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				nextCalled = true
			})
			auth, err := NewInternalAuth(next, testInternalToken)
			if err != nil {
				t.Fatalf("NewInternalAuth() error = %v", err)
			}
			request := httptest.NewRequest(http.MethodGet, "/alice/repo.git", nil)
			request.Header = test.headers.Clone()
			response := httptest.NewRecorder()

			auth.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, expected forbidden", response.Code)
			}
			if nextCalled {
				t.Fatal("unauthenticated request reached next handler")
			}
		})
	}
}

func TestInternalAuthPreservesAuthenticatedHeadersForRouter(t *testing.T) {
	t.Parallel()

	var nextCalled bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		nextCalled = true
		if request.Header.Get(ForwardedHeader) != ForwardedHeaderValue {
			t.Error("forward marker was removed before routing")
		}
		if request.Header.Get(InternalTokenHeader) != testInternalToken {
			t.Error("internal token was removed before routing")
		}
	})
	auth, err := NewInternalAuth(next, testInternalToken)
	if err != nil {
		t.Fatalf("NewInternalAuth() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/alice/repo.git", nil)
	setAuthenticatedForward(request)
	response := httptest.NewRecorder()

	auth.ServeHTTP(response, request)
	if !nextCalled {
		t.Fatal("authenticated request did not reach next handler")
	}
}
