package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/shard"
)

func TestNewHandlerValidatesOptions(t *testing.T) {
	t.Parallel()

	router := proxyTestRouter(t, 2)
	resolver := resolverFunc(func(shard.ShardID) (*url.URL, error) {
		return url.Parse("http://gitone-0.headless.default.svc:8080")
	})
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	valid := HandlerOptions{
		LocalShard: 0,
		Router:     router,
		Resolver:   resolver,
		Next:       next,
	}

	tests := []struct {
		name   string
		mutate func(*HandlerOptions)
	}{
		{name: "missing router", mutate: func(o *HandlerOptions) { o.Router = nil }},
		{name: "missing resolver", mutate: func(o *HandlerOptions) { o.Resolver = nil }},
		{name: "missing next", mutate: func(o *HandlerOptions) { o.Next = nil }},
		{name: "local shard out of range", mutate: func(o *HandlerOptions) { o.LocalShard = 2 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := valid
			test.mutate(&options)
			_, err := NewHandler(options)
			if !errors.Is(err, ErrInvalidHandlerConfig) {
				t.Fatalf("NewHandler() error = %v, expected handler config error", err)
			}
		})
	}
}

func TestHandlerServesOwnerLocallyAndStripsUntrustedHeaders(t *testing.T) {
	t.Parallel()

	var nextCalled bool
	next := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		nextCalled = true
		assertInternalHeadersAbsent(t, request.Header)
		if actual := request.Header.Get("Authorization"); actual != "Bearer original" {
			t.Errorf("Authorization = %q, expected original credential", actual)
		}
		response.WriteHeader(http.StatusNoContent)
	})
	handler := proxyTestHandler(t, 0, nil, next)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/acme/repo.git/info/refs", nil)
	request.Header.Set("Authorization", "Bearer original")
	request.Header.Set("X-User", "attacker")
	request.Header.Set("X-GitOne-Role", "owner")
	request.Header.Set(ForwardedHeader, ForwardedHeaderValue)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if !nextCalled {
		t.Fatal("next handler was not called")
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, expected %d", response.Code, http.StatusNoContent)
	}
}

func TestHandlerForwardsDirectlyAndReplacesSpoofedHeaders(t *testing.T) {
	t.Parallel()

	var outbound *http.Request
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		outbound = request.Clone(request.Context())
		outbound.Header = request.Header.Clone()
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("reading outbound body: %v", err)
		}
		if string(body) != "push body" {
			t.Errorf("outbound body = %q, expected push body", body)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header: http.Header{
				"Content-Type":          []string{"application/octet-stream"},
				"X-GitOne-Debug-Secret": []string{"do-not-leak"},
				"X-Upstream":            []string{"owner"},
			},
			Body: io.NopCloser(strings.NewReader("forwarded")),
		}, nil
	})
	handler := proxyTestHandler(t, 0, transport, http.NotFoundHandler())
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/alice/repo.git/git-receive-pack?trace=1",
		strings.NewReader("push body"),
	)
	request.Header.Set("Authorization", "Bearer original")
	request.Header.Set("X-User", "attacker")
	request.Header.Set("X-GitOne-Role", "owner")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, expected %d", response.Code, http.StatusCreated)
	}
	if response.Body.String() != "forwarded" {
		t.Errorf("body = %q, expected forwarded", response.Body.String())
	}
	if response.Header().Get("X-GitOne-Debug-Secret") != "" {
		t.Error("internal response header leaked to client")
	}
	if response.Header().Get("X-Upstream") != "owner" {
		t.Error("ordinary upstream response header was not preserved")
	}
	if outbound == nil {
		t.Fatal("transport did not receive outbound request")
	}
	if outbound.URL.Scheme != "http" || outbound.URL.Host != "gitone-1.gitone-headless.default.svc:8080" {
		t.Errorf("outbound URL = %q, expected shard 1 destination", outbound.URL.String())
	}
	if outbound.URL.Path != "/alice/repo.git/git-receive-pack" || outbound.URL.RawQuery != "trace=1" {
		t.Errorf("outbound request target = %q, expected original path and query", outbound.URL.RequestURI())
	}
	if outbound.Host != "gitone-1.gitone-headless.default.svc:8080" {
		t.Errorf("outbound Host = %q, expected destination host", outbound.Host)
	}
	if outbound.Header.Get(ForwardedHeader) != ForwardedHeaderValue {
		t.Errorf("forward marker = %q, expected %q", outbound.Header.Get(ForwardedHeader), ForwardedHeaderValue)
	}
	if outbound.Header.Get("X-User") != "" || outbound.Header.Get("X-GitOne-Role") != "" {
		t.Error("spoofed identity header reached owner")
	}
	if outbound.Header.Get("Authorization") != "Bearer original" {
		t.Error("original authorization credential was not preserved")
	}
}

func TestHandlerForwardsToOwnerWithoutCredentials(t *testing.T) {
	t.Parallel()

	var ownerCalled bool
	owner := proxyTestHandler(t, 1, nil, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		ownerCalled = true
		assertInternalHeadersAbsent(t, request.Header)
		response.WriteHeader(http.StatusNoContent)
	}))
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "" || request.Header.Get("X-GitOne-Internal-Token") != "" {
			t.Error("forwarded request unexpectedly contains credentials")
		}
		if request.Header.Get(ForwardedHeader) != ForwardedHeaderValue {
			t.Error("forwarded request is missing its one-hop marker")
		}
		response := httptest.NewRecorder()
		owner.ServeHTTP(response, request)
		return response.Result(), nil
	})
	entry := proxyTestHandler(t, 0, transport, http.NotFoundHandler())
	request := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/alice/repo.git/info/refs?service=git-upload-pack", nil,
	)
	response := httptest.NewRecorder()

	entry.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || !ownerCalled {
		t.Fatalf("status = %d, owner called = %t; want 204 and true", response.Code, ownerCalled)
	}
}

func TestHandlerAcceptsForwardedRequestOnlyAtOwner(t *testing.T) {
	t.Parallel()

	t.Run("owner serves locally", func(t *testing.T) {
		t.Parallel()
		var nextCalled bool
		next := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			nextCalled = true
			assertInternalHeadersAbsent(t, request.Header)
		})
		handler := proxyTestHandler(t, 0, nil, next)
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/acme/repo.git/info/refs", nil)
		setForwardedRequest(request)

		handler.ServeHTTP(httptest.NewRecorder(), request)
		if !nextCalled {
			t.Fatal("owner did not serve forwarded request")
		}
	})

	t.Run("non owner rejects without second hop", func(t *testing.T) {
		t.Parallel()
		var resolved bool
		resolver := resolverFunc(func(shard.ShardID) (*url.URL, error) {
			resolved = true
			return url.Parse("http://should-not-be-used.invalid")
		})
		handler := newProxyTestHandler(
			t,
			HandlerOptions{
				LocalShard: 0,
				Router:     proxyTestRouter(t, 2),
				Resolver:   resolver,
				Next:       http.NotFoundHandler(),
			},
		)
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/alice/repo.git/info/refs", nil)
		setForwardedRequest(request)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, expected routing mismatch", response.Code)
		}
		if resolved {
			t.Fatal("forwarded request was forwarded a second time")
		}
	})
}

func TestHandlerRejectsInvalidPathBeforeRouting(t *testing.T) {
	t.Parallel()

	var nextCalled bool
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true })
	handler := proxyTestHandler(t, 0, nil, next)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/Alice/repo.git", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, expected %d", response.Code, http.StatusBadRequest)
	}
	if nextCalled {
		t.Fatal("invalid path reached local handler")
	}
}

func TestHandlerReportsResolutionAndTransportFailures(t *testing.T) {
	t.Parallel()

	t.Run("resolver failure", func(t *testing.T) {
		t.Parallel()
		resolver := resolverFunc(func(shard.ShardID) (*url.URL, error) {
			return nil, errors.New("dns configuration failed")
		})
		handler := newProxyTestHandler(t, HandlerOptions{
			LocalShard: 0,
			Router:     proxyTestRouter(t, 2),
			Resolver:   resolver,
			Next:       http.NotFoundHandler(),
		})
		response := httptest.NewRecorder()
		handler.ServeHTTP(
			response,
			httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/alice/repo.git", nil),
		)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, expected bad gateway", response.Code)
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		t.Parallel()
		transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection failed")
		})
		handler := proxyTestHandler(t, 0, transport, http.NotFoundHandler())
		response := httptest.NewRecorder()
		handler.ServeHTTP(
			response,
			httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/alice/repo.git", nil),
		)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, expected bad gateway", response.Code)
		}
	})
}

func TestHandlerStreamsRequestWithoutPrebuffering(t *testing.T) {
	t.Parallel()

	transportStarted := make(chan struct{})
	firstByteRead := make(chan struct{})
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		close(transportStarted)
		buffer := make([]byte, 1)
		if _, err := io.ReadFull(request.Body, buffer); err != nil {
			return nil, err
		}
		close(firstByteRead)
		return proxyResponse(http.StatusOK, "ok"), nil
	})
	handler := proxyTestHandler(t, 0, transport, http.NotFoundHandler())
	bodyReader, bodyWriter := io.Pipe()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alice/repo.git/git-receive-pack", bodyReader)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	waitForSignal(t, transportStarted, "transport start before request body completion")
	if _, err := bodyWriter.Write([]byte("x")); err != nil {
		t.Fatalf("writing first request byte: %v", err)
	}
	waitForSignal(t, firstByteRead, "transport reading first request byte")
	if err := bodyWriter.Close(); err != nil {
		t.Fatalf("closing request body: %v", err)
	}
	waitForSignal(t, done, "streaming request completion")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, expected ok", response.Code)
	}
}

func TestHandlerStreamsResponseBeforeUpstreamEOF(t *testing.T) {
	t.Parallel()

	responseReader, responseWriter := io.Pipe()
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          responseReader,
			ContentLength: -1,
		}, nil
	})
	handler := proxyTestHandler(t, 0, transport, http.NotFoundHandler())
	observed := newObservingResponseWriter()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(
			observed,
			httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/alice/repo.git/git-upload-pack", nil),
		)
		close(done)
	}()

	if _, err := responseWriter.Write([]byte("first")); err != nil {
		t.Fatalf("writing first response chunk: %v", err)
	}
	waitForSignal(t, observed.firstWrite, "downstream receiving response before upstream eof")
	if err := responseWriter.Close(); err != nil {
		t.Fatalf("closing upstream response: %v", err)
	}
	waitForSignal(t, done, "streaming response completion")
	if actual := observed.bodyString(); actual != "first" {
		t.Fatalf("streamed body = %q, expected first", actual)
	}
}

func TestHandlerPropagatesRequestCancellation(t *testing.T) {
	t.Parallel()

	transportStarted := make(chan struct{})
	observedCancellation := make(chan error, 1)
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		close(transportStarted)
		<-request.Context().Done()
		observedCancellation <- request.Context().Err()
		return nil, request.Context().Err()
	})
	handler := proxyTestHandler(t, 0, transport, http.NotFoundHandler())
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/alice/repo.git/info/refs", nil)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	waitForSignal(t, transportStarted, "transport start")
	cancel()
	select {
	case err := <-observedCancellation:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("transport context error = %v, expected canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transport did not observe request cancellation")
	}
	waitForSignal(t, done, "canceled proxy completion")
}

func TestIsValidDestination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		destination *url.URL
		expected    bool
	}{
		{
			name:        "valid http destination",
			destination: &url.URL{Scheme: "http", Host: "gitone-1.internal:8080"},
			expected:    true,
		},
		{name: "nil destination", destination: nil},
		{name: "missing host", destination: &url.URL{Scheme: "http"}},
		{name: "unsupported scheme", destination: &url.URL{Scheme: "file", Host: "internal"}},
		{
			name:        "userinfo",
			destination: &url.URL{Scheme: "http", Host: "internal", User: url.User("secret")},
		},
		{name: "base path", destination: &url.URL{Scheme: "http", Host: "internal", Path: "/base"}},
		{name: "raw path", destination: &url.URL{Scheme: "http", Host: "internal", RawPath: "/base"}},
		{name: "query", destination: &url.URL{Scheme: "http", Host: "internal", RawQuery: "admin=1"}},
		{name: "fragment", destination: &url.URL{Scheme: "http", Host: "internal", Fragment: "secret"}},
		{name: "opaque url", destination: &url.URL{Scheme: "http", Opaque: "//internal"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if actual := isValidDestination(test.destination); actual != test.expected {
				t.Fatalf("isValidDestination() = %t, expected %t", actual, test.expected)
			}
		})
	}
}

func proxyTestHandler(
	t *testing.T,
	localShard shard.ShardID,
	transport http.RoundTripper,
	next http.Handler,
) *Handler {
	t.Helper()
	resolver, err := NewStatefulSetResolver(StatefulSetResolverOptions{
		Scheme:          "http",
		StatefulSet:     "gitone",
		HeadlessService: "gitone-headless",
		Namespace:       "default",
		Port:            8080,
	})
	if err != nil {
		t.Fatalf("NewStatefulSetResolver() error = %v", err)
	}
	return newProxyTestHandler(t, HandlerOptions{
		LocalShard: localShard,
		Router:     proxyTestRouter(t, 2),
		Resolver:   resolver,
		Next:       next,
		Transport:  transport,
	})
}

func newProxyTestHandler(t *testing.T, options HandlerOptions) *Handler {
	t.Helper()
	handler, err := NewHandler(options)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return handler
}

func proxyTestRouter(t *testing.T, shardCount uint32) *shard.Router {
	t.Helper()
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		t.Fatalf("shard.NewParser() error = %v", err)
	}
	router, err := shard.NewRouter(shardCount, parser)
	if err != nil {
		t.Fatalf("shard.NewRouter() error = %v", err)
	}
	return router
}

func setForwardedRequest(request *http.Request) {
	request.Header.Set(ForwardedHeader, ForwardedHeaderValue)
}

func assertInternalHeadersAbsent(t *testing.T, header http.Header) {
	t.Helper()
	for name := range header {
		lowerName := strings.ToLower(name)
		if lowerName == "x-user" || lowerName == "x-gitone" || strings.HasPrefix(lowerName, "x-gitone-") {
			t.Errorf("untrusted header %q was not stripped", name)
		}
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func proxyResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type resolverFunc func(shard.ShardID) (*url.URL, error)

func (function resolverFunc) Resolve(id shard.ShardID) (*url.URL, error) {
	return function(id)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type observingResponseWriter struct {
	header     http.Header
	firstWrite chan struct{}
	once       sync.Once
	mu         sync.Mutex
	body       bytes.Buffer
	status     int
}

func newObservingResponseWriter() *observingResponseWriter {
	return &observingResponseWriter{
		header:     make(http.Header),
		firstWrite: make(chan struct{}),
	}
}

func (w *observingResponseWriter) Header() http.Header {
	return w.header
}

func (w *observingResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}

func (w *observingResponseWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	written, err := w.body.Write(data)
	w.once.Do(func() { close(w.firstWrite) })
	return written, err
}

func (w *observingResponseWriter) Flush() {}

func (w *observingResponseWriter) bodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}
