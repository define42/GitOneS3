package proxy

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/define42/GitOneS3/internal/shard"
)

const (
	ForwardedHeader          = "X-GitOne-Forwarded"
	InternalTokenHeader      = "X-GitOne-Internal-Token"
	ForwardedHeaderValue     = "1"
	MinimumInternalTokenSize = 32
)

var (
	ErrInvalidHandlerConfig = errors.New("proxy: invalid handler configuration")
)

// Resolver maps a shard to its direct internal destination.
type Resolver interface {
	Resolve(id shard.ShardID) (*url.URL, error)
}

// HandlerOptions contains the immutable dependencies for routing middleware.
type HandlerOptions struct {
	LocalShard    shard.ShardID
	Router        *shard.Router
	Resolver      Resolver
	Next          http.Handler
	InternalToken string
	Transport     http.RoundTripper
}

// Handler serves local requests and directly streams remote requests to their
// owning shard.
type Handler struct {
	localShard    shard.ShardID
	router        *shard.Router
	resolver      Resolver
	next          http.Handler
	internalToken []byte
	reverseProxy  *httputil.ReverseProxy
}

// NewHandler validates dependencies and constructs shard-routing middleware.
func NewHandler(options HandlerOptions) (*Handler, error) {
	if options.Router == nil {
		return nil, fmt.Errorf("%w: router is required", ErrInvalidHandlerConfig)
	}
	if options.Resolver == nil {
		return nil, fmt.Errorf("%w: resolver is required", ErrInvalidHandlerConfig)
	}
	if options.Next == nil {
		return nil, fmt.Errorf("%w: next handler is required", ErrInvalidHandlerConfig)
	}
	if uint32(options.LocalShard) >= options.Router.ShardCount() {
		return nil, fmt.Errorf("%w: local shard is out of range", ErrInvalidHandlerConfig)
	}
	if !validInternalToken(options.InternalToken) {
		return nil, fmt.Errorf(
			"%w: internal token must contain at least %d visible ASCII bytes",
			ErrInvalidHandlerConfig,
			MinimumInternalTokenSize,
		)
	}

	transport := options.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	reverseProxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(response, "shard forwarding failed", http.StatusBadGateway)
		},
		ModifyResponse: func(response *http.Response) error {
			stripInternalHeaders(response.Header)
			return nil
		},
	}

	return &Handler{
		localShard:    options.LocalShard,
		router:        options.Router,
		resolver:      options.Resolver,
		next:          options.Next,
		internalToken: []byte(options.InternalToken),
		reverseProxy:  reverseProxy,
	}, nil
}

// ServeHTTP routes request locally or forwards it directly to its owner.
func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	route, err := h.router.Resolve(request)
	if err != nil {
		http.Error(response, "bad repository path", http.StatusBadRequest)
		return
	}

	isForwarded := h.isAuthenticatedForward(request)
	if isForwarded && route.Owner != h.localShard {
		http.Error(response, "internal routing mismatch", http.StatusBadGateway)
		return
	}
	if route.Owner == h.localShard {
		h.serveLocal(response, request)
		return
	}

	destination, err := h.resolver.Resolve(route.Owner)
	if err != nil || !isValidDestination(destination) {
		http.Error(response, "shard destination unavailable", http.StatusBadGateway)
		return
	}
	h.serveRemote(response, request, destination)
}

func (h *Handler) serveLocal(response http.ResponseWriter, request *http.Request) {
	localRequest := request.Clone(request.Context())
	localRequest.Header = request.Header.Clone()
	stripInternalHeaders(localRequest.Header)
	h.next.ServeHTTP(response, localRequest)
}

func (h *Handler) serveRemote(
	response http.ResponseWriter,
	request *http.Request,
	destination *url.URL,
) {
	reverseProxy := *h.reverseProxy
	reverseProxy.Rewrite = func(proxyRequest *httputil.ProxyRequest) {
		proxyRequest.SetURL(destination)
		proxyRequest.Out.Host = destination.Host
		proxyRequest.SetXForwarded()
		stripInternalHeaders(proxyRequest.Out.Header)
		proxyRequest.Out.Header.Set(ForwardedHeader, ForwardedHeaderValue)
		proxyRequest.Out.Header.Set(InternalTokenHeader, string(h.internalToken))
	}
	reverseProxy.ServeHTTP(response, request)
}

func (h *Handler) isAuthenticatedForward(request *http.Request) bool {
	return hasAuthenticatedForward(request.Header, h.internalToken)
}

func hasAuthenticatedForward(header http.Header, internalToken []byte) bool {
	forwardedValues := exactHeaderValues(header, ForwardedHeader)
	tokenValues := exactHeaderValues(header, InternalTokenHeader)
	if len(forwardedValues) != 1 || len(tokenValues) != 1 {
		return false
	}
	if forwardedValues[0] != ForwardedHeaderValue {
		return false
	}
	return subtle.ConstantTimeCompare(
		[]byte(tokenValues[0]),
		internalToken,
	) == 1
}

func isValidDestination(destination *url.URL) bool {
	if destination == nil || destination.Host == "" {
		return false
	}
	if destination.Scheme != "http" && destination.Scheme != "https" {
		return false
	}
	if destination.User != nil || destination.Opaque != "" {
		return false
	}
	if destination.Path != "" || destination.RawPath != "" {
		return false
	}
	if destination.ForceQuery || destination.RawQuery != "" {
		return false
	}
	return destination.Fragment == "" && destination.RawFragment == ""
}

func stripInternalHeaders(header http.Header) {
	for name := range header {
		lowerName := strings.ToLower(name)
		isGitOneHeader := lowerName == "x-gitone" || strings.HasPrefix(lowerName, "x-gitone-")
		if isGitOneHeader || lowerName == "x-user" {
			delete(header, name)
		}
	}
}

func exactHeaderValues(header http.Header, expectedName string) []string {
	values := make([]string, 0, 1)
	for name, currentValues := range header {
		if strings.EqualFold(name, expectedName) {
			values = append(values, currentValues...)
		}
	}
	return values
}
