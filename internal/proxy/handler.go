package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/shard"
)

const (
	ForwardedHeader      = "X-GitOne-Forwarded"
	ForwardedHeaderValue = "1"
	gitBodyReadTimeout   = 90 * time.Second
)

var (
	ErrInvalidHandlerConfig = errors.New("proxy: invalid handler configuration")
)

// Resolver maps a shard to its direct internal destination.
type Resolver interface {
	Resolve(id shard.ShardID) (*url.URL, error)
}

// Router resolves namespace requests and special routes such as OIDC callbacks.
type Router interface {
	Resolve(*http.Request) (shard.Route, error)
	ShardCount() uint32
}

// HandlerOptions contains the immutable dependencies for routing middleware.
type HandlerOptions struct {
	LocalShard         shard.ShardID
	Router             Router
	Resolver           Resolver
	Next               http.Handler
	Transport          http.RoundTripper
	LFSTransferTimeout time.Duration
}

// Handler serves local requests and directly streams remote requests to their
// owning shard.
type Handler struct {
	localShard         shard.ShardID
	router             Router
	resolver           Resolver
	next               http.Handler
	reverseProxy       *httputil.ReverseProxy
	lfsTransferTimeout time.Duration
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
	if options.LFSTransferTimeout == 0 {
		options.LFSTransferTimeout = 30 * time.Minute
	}
	if options.LFSTransferTimeout < 0 || options.LFSTransferTimeout > 12*time.Hour {
		return nil, fmt.Errorf("%w: LFS transfer timeout must be positive and at most 12h", ErrInvalidHandlerConfig)
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
		localShard:         options.LocalShard,
		router:             options.Router,
		resolver:           options.Resolver,
		next:               options.Next,
		reverseProxy:       reverseProxy,
		lfsTransferTimeout: options.LFSTransferTimeout,
	}, nil
}

// ServeHTTP routes request locally or forwards it directly to its owner.
func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	route, err := h.router.Resolve(request)
	if err != nil {
		http.Error(response, "bad repository path", http.StatusBadRequest)
		return
	}

	isForwarded := hasForwardedMarker(request.Header)
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
	if isLFSTransfer(request) {
		// The public connection must permit the same bounded transfer time as
		// its owner. Its default 30s body deadline is too short for large files.
		ctx, cancel := context.WithTimeout(request.Context(), h.lfsTransferTimeout)
		defer cancel()
		request = request.WithContext(ctx)
		deadline, _ := ctx.Deadline()
		controller := http.NewResponseController(response)
		for _, set := range []func(time.Time) error{controller.SetReadDeadline, controller.SetWriteDeadline} {
			if err := set(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
				http.Error(response, "cannot establish LFS forwarding deadline", http.StatusServiceUnavailable)
				return
			}
		}
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}
	if isGitRPC(request) {
		// The owner permits 90-second Git operations. Match that body budget
		// on the entry connection, where owner authentication has not run yet.
		// Keep it finite and active through any unread-body drain.
		deadline := time.Now().Add(gitBodyReadTimeout)
		if contextDeadline, ok := request.Context().Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		if err := http.NewResponseController(response).SetReadDeadline(deadline); err != nil &&
			!errors.Is(err, http.ErrNotSupported) {
			http.Error(response, "cannot establish git forwarding deadline", http.StatusServiceUnavailable)
			return
		}
	}

	reverseProxy := *h.reverseProxy
	reverseProxy.Rewrite = func(proxyRequest *httputil.ProxyRequest) {
		proxyRequest.SetURL(destination)
		proxyRequest.Out.Host = destination.Host
		proxyRequest.SetXForwarded()
		stripInternalHeaders(proxyRequest.Out.Header)
		proxyRequest.Out.Header.Set(ForwardedHeader, ForwardedHeaderValue)
	}
	reverseProxy.ServeHTTP(response, request)
}

func isGitRPC(request *http.Request) bool {
	if request.Method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
	if len(parts) != 3 || !strings.HasSuffix(parts[1], ".git") {
		return false
	}
	return parts[2] == "git-upload-pack" || parts[2] == "git-receive-pack"
}

func isLFSTransfer(request *http.Request) bool {
	if request.Method != http.MethodPut && request.Method != http.MethodGet && request.Method != http.MethodHead {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
	if len(parts) != 6 || !strings.HasSuffix(parts[1], ".git") || parts[2] != "info" || parts[3] != "lfs" || parts[4] != "objects" || len(parts[5]) != 64 {
		return false
	}
	for _, b := range parts[5] {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

// hasForwardedMarker identifies a previous hop, not an authenticated caller.
func hasForwardedMarker(header http.Header) bool {
	return slices.Contains(exactHeaderValues(header, ForwardedHeader), ForwardedHeaderValue)
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
