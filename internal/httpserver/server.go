// Package httpserver owns GitOne's HTTP server lifecycle.
package httpserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultIdleTimeout       = 2 * time.Minute
	defaultShutdownTimeout   = 30 * time.Second
	defaultReadinessTimeout  = 3 * time.Second
	defaultMaxHeaderBytes    = 1 << 20
)

// Checker verifies a required backing service for readiness.
type Checker interface {
	Check(context.Context) error
}

// Server serves client and forwarded requests on one HTTP listener.
type Server struct {
	httpServer *http.Server
	logger     *slog.Logger
}

// New constructs a streaming-safe HTTP server. Read and write timeouts remain
// unset because Git and LFS transfers can legitimately be long-lived.
func New(address string, handler http.Handler, logger *slog.Logger) (*Server, error) {
	if handler == nil {
		return nil, errors.New("handler is required")
	}
	if address == "" {
		return nil, errors.New("listen address is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{httpServer: newServer(address, handler), logger: logger}, nil
}

// Run serves until context cancellation or a listener failure, then shuts down.
func (s *Server) Run(ctx context.Context) error {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", s.httpServer.Addr)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Cancellation is a normal shutdown, including during listener setup.
			return nil
		}
		return fmt.Errorf("listen HTTP: %w", err)
	}
	s.logger.InfoContext(ctx, "http server starting", "address", listener.Addr().String())
	serveErrors := make(chan error, 1)
	go func() {
		err := s.httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErrors <- err
	}()

	var runErr error
	var stopped bool
	select {
	case <-ctx.Done():
	case runErr = <-serveErrors:
		stopped = true
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultShutdownTimeout)
	defer cancel()
	shutdownErr := s.httpServer.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, s.httpServer.Close())
	}
	if !stopped {
		runErr = <-serveErrors
	}
	if err := errors.Join(runErr, shutdownErr); err != nil {
		return fmt.Errorf("run HTTP server: %w", err)
	}
	return nil
}

// WithHealth reserves liveness and readiness routes ahead of namespace
// routing. All other requests are delegated unchanged.
func WithHealth(next http.Handler, checker Checker) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/livez":
			if request.Method != http.MethodGet && request.Method != http.MethodHead {
				response.Header().Set("Allow", "GET, HEAD")
				http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			response.Header().Set("Content-Type", "text/plain; charset=utf-8")
			response.WriteHeader(http.StatusOK)
			return
		case "/readyz":
			serveReadiness(response, request, checker)
			return
		default:
			next.ServeHTTP(response, request)
		}
	})
}

// LogRequests emits low-cardinality request completion records.
func LogRequests(next http.Handler, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		tracked := &statusWriter{ResponseWriter: response, status: http.StatusOK}
		next.ServeHTTP(tracked, request)
		logger.InfoContext(
			request.Context(),
			"http request completed",
			"method", request.Method,
			"status", tracked.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

func newServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultIdleTimeout,
		MaxHeaderBytes:    defaultMaxHeaderBytes,
	}
}

func serveReadiness(response http.ResponseWriter, request *http.Request, checker Checker) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if checker == nil {
		http.Error(response, "not ready", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), defaultReadinessTimeout)
	defer cancel()
	if err := checker.Check(ctx); err != nil {
		http.Error(response, "not ready", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(http.StatusOK)
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	return w.ResponseWriter.Write(data)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}

	return hijacker.Hijack()
}

func (w *statusWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}

	return pusher.Push(target, opts)
}

func (w *statusWriter) ReadFrom(reader io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(reader)
	}

	return io.Copy(w.ResponseWriter, reader)
}
