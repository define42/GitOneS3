package httpserver

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestServerClosesStalledBodies(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		request string
		handler http.Handler
	}{
		{
			name:    "liveness content length",
			request: "GET /livez HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n",
			handler: WithHealth(http.NotFoundHandler(), nil),
		},
		{
			name:    "liveness chunked",
			request: "GET /livez HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: chunked\r\n\r\n",
			handler: WithHealth(http.NotFoundHandler(), nil),
		},
		{
			name:    "unsupported expectation before handler",
			request: "POST /livez HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\nExpect: unsupported\r\n\r\n",
			handler: WithHealth(http.NotFoundHandler(), nil),
		},
		{
			name:    "rejected request",
			request: "POST /private HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "authentication required", http.StatusUnauthorized)
			}),
		},
		{
			name:    "handler clears deadline before returning",
			request: "POST /stream HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if err := http.NewResponseController(w).SetReadDeadline(time.Time{}); err != nil {
					panic(err)
				}
				w.WriteHeader(http.StatusOK)
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				client := servePipe(t, test.handler)
				started := time.Now()
				writeRequest(t, client, test.request)
				// A client-side timeout fails the test instead of masking a
				// server that leaves the connection open indefinitely.
				if err := client.SetReadDeadline(started.Add(2 * defaultBodyReadTimeout)); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadAll(client); err != nil {
					t.Fatalf("server did not close the stalled connection: %v", err)
				}
				if elapsed := time.Since(started); elapsed > defaultBodyReadTimeout+time.Second {
					t.Fatalf("stalled body held connection for %v", elapsed)
				}
			})
		})
	}
}

func TestServerBodyReadTimesOut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		readErrors := make(chan error, 1)
		client := servePipe(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := io.Copy(io.Discard, r.Body)
			readErrors <- err
			w.WriteHeader(http.StatusRequestTimeout)
		}))
		writeRequest(t, client, "POST /api HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n")
		select {
		case err := <-readErrors:
			if timeout, ok := errors.AsType[net.Error](err); !ok || !timeout.Timeout() {
				t.Fatalf("body read error = %v, want timeout", err)
			}
		case <-time.After(2 * defaultBodyReadTimeout):
			t.Fatal("handler remained blocked reading request body")
		}
	})
}

func TestServerStreamingOverrideAndKeepAlive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/stream" {
				if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(3 * defaultBodyReadTimeout)); err != nil {
					t.Error(err)
					return
				}
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "data" {
				t.Errorf("request body = %q, error = %v", body, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		// Exercise the ResponseController through the production logging wrapper.
		client := servePipe(t, LogRequests(handler, slog.New(slog.NewTextHandler(io.Discard, nil))))
		reader := bufio.NewReader(client)
		writeRequest(t, client, "POST /stream HTTP/1.1\r\nHost: localhost\r\nContent-Length: 4\r\n\r\n")
		synctest.Wait()
		time.Sleep(2 * defaultBodyReadTimeout)
		writeRequest(t, client, "data")
		readNoContent(t, reader)
		synctest.Wait()
		// The server must install a fresh deadline for a reused connection.
		time.Sleep(defaultBodyReadTimeout + time.Second)
		writeRequest(t, client, "POST /next HTTP/1.1\r\nHost: localhost\r\nContent-Length: 4\r\n\r\n")
		time.Sleep(defaultBodyReadTimeout / 2)
		writeRequest(t, client, "data")
		readNoContent(t, reader)
	})
}

func TestServerDoesNotTimeOutBodylessResponses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		client := servePipe(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * defaultBodyReadTimeout)
			if err := r.Context().Err(); err != nil {
				t.Errorf("bodyless response context canceled: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		writeRequest(t, client, "GET /download HTTP/1.1\r\nHost: localhost\r\n\r\n")
		readNoContent(t, bufio.NewReader(client))
	})
}

func servePipe(t *testing.T, handler http.Handler) net.Conn {
	t.Helper()
	client, connection := net.Pipe()
	listener := &pipeListener{connection: connection, closed: make(chan struct{})}
	server := newServer("unused", handler)
	connectionClosed := make(chan struct{})
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			close(connectionClosed)
		}
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		<-serveDone
		<-connectionClosed
	})
	if err := client.SetDeadline(time.Now().Add(10 * defaultBodyReadTimeout)); err != nil {
		t.Fatal(err)
	}
	return client
}

func writeRequest(t *testing.T, client net.Conn, request string) {
	t.Helper()
	if _, err := io.Copy(client, strings.NewReader(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func readNoContent(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

// pipeListener runs the real HTTP server with in-memory connections so synctest
// can advance the production deadlines without wall-clock waits.
type pipeListener struct {
	connection net.Conn
	accepted   bool
	closed     chan struct{}
	once       sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.connection, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return l.connection.LocalAddr() }
