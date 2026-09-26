package gittransport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	read, write           []time.Time
	writeWithoutDeadlines bool
	failure               string
}

func (w *deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	w.read = append(w.read, deadline)
	if w.failure == "read" && !deadline.IsZero() {
		return errors.New("read deadline failed")
	}
	return nil
}

func (w *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.write = append(w.write, deadline)
	if w.failure == "write" && !deadline.IsZero() {
		return errors.New("write deadline failed")
	}
	return nil
}

func (w *deadlineRecorder) Write(data []byte) (int, error) {
	if len(w.read) != 1 || len(w.write) != 1 || w.read[0].IsZero() || w.write[0].IsZero() {
		w.writeWithoutDeadlines = true
	}
	return w.ResponseRecorder.Write(data)
}

type deadlineStore struct {
	storage.ObjectStore
	check func(context.Context)
}

func (s deadlineStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	s.check(ctx)
	return s.ObjectStore.Get(ctx, key)
}

func TestHandlerDeadlines(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"", "read", "write"} {
		t.Run("deadline "+failure, func(t *testing.T) {
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), failure: failure}
			loads := 0
			checking := false
			objects := deadlineStore{ObjectStore: storage.NewMemoryStore(), check: func(ctx context.Context) {
				if !checking {
					return
				}
				loads++
				deadline, ok := ctx.Deadline()
				if !ok || len(w.read) != 1 || len(w.write) != 1 || !w.read[0].Equal(deadline) || !w.write[0].Equal(deadline) {
					t.Error("snapshot loaded before both context deadlines were installed")
				}
			}}
			store, err := repository.New(objects)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(context.Background(), "alice", repository.CreateInput{Name: "demo", CreatedBy: "alice"}); err != nil {
				t.Fatal(err)
			}
			// Creation does not exercise a transport request's deadline contract.
			loads = 0
			checking = true
			handler, err := New(store)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(10*time.Second))
			defer cancel()
			r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/alice/demo.git/info/refs?service=git-upload-pack", nil)
			handler.ServeHTTP(w, r)
			want := http.StatusOK
			if failure != "" {
				want = http.StatusServiceUnavailable
				if loads != 0 {
					t.Error("failed deadline setup still loaded repository")
				}
			}
			if w.Code != want {
				t.Fatalf("status = %d, want %d", w.Code, want)
			}
			if w.writeWithoutDeadlines {
				t.Error("response written before deadline setup")
			}
			if len(w.read) != 2 || len(w.write) != 2 || !w.read[1].IsZero() || !w.write[1].IsZero() {
				t.Fatalf("deadlines not cleared: read %v write %v", w.read, w.write)
			}
		})
	}
}

func TestHandlerValidation(t *testing.T) {
	t.Parallel()
	store, err := repository.New(storage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), "alice", repository.CreateInput{Name: "demo", CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	handler, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, method, path, contentType string
		status                          int
	}{
		{"advertise", "GET", "/alice/demo.git/info/refs?service=git-upload-pack", "", 200},
		{"missing repository", "GET", "/alice/missing.git/info/refs?service=git-upload-pack", "", 404},
		{"wrong method", "PUT", "/alice/demo.git/git-upload-pack", "", 400},
		{"unknown service", "GET", "/alice/demo.git/info/refs?service=git-other", "", 400},
		{"duplicate service", "GET", "/alice/demo.git/info/refs?service=git-upload-pack&service=git-upload-pack", "", 400},
		{"escaped slash", "GET", "/alice%2fdemo.git/info/refs?service=git-upload-pack", "", 400},
		{"traversal", "GET", "/alice/../demo.git/info/refs?service=git-upload-pack", "", 400},
		{"missing authorization", "POST", "/alice/demo.git/git-receive-pack", "application/x-git-receive-pack-request", 403},
		{"wrong media type", "POST", "/alice/demo.git/git-upload-pack", "text/plain", 415},
		{"malformed pktline", "POST", "/alice/demo.git/git-upload-pack", "application/x-git-upload-pack-request", 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, strings.NewReader("garbage"))
			r.Header.Set("Content-Type", tt.contentType)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("cacheable private response")
			}
		})
	}
}

type advertisementStore struct {
	storage.ObjectStore
	objectReads int
}

func (s *advertisementStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "/objects/") {
		s.objectReads++
	}
	return s.ObjectStore.Get(ctx, key)
}

func TestReferenceAdvertisementsDoNotReadObjectData(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"http", "ssh"} {
		for _, service := range []string{upload, receive} {
			t.Run(transport+"/"+service, func(t *testing.T) {
				t.Parallel()
				objects := &advertisementStore{ObjectStore: storage.NewMemoryStore()}
				store, err := repository.New(objects)
				if err != nil {
					t.Fatal(err)
				}
				_, err = store.Create(t.Context(), "alice", repository.CreateInput{
					Name: "demo", CreatedBy: "alice", InitializeReadme: true,
					AuthorName: "Alice", AuthorEmail: "alice@example.test",
				})
				if err != nil {
					t.Fatal(err)
				}
				handler, err := New(store)
				if err != nil {
					t.Fatal(err)
				}
				objects.objectReads = 0
				if transport == "http" {
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
						"/alice/demo.git/info/refs?service="+service, nil))
					if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "refs/heads/main") {
						t.Fatalf("advertisement = %d %s", w.Code, w.Body.String())
					}
				} else {
					stream := &sshTestStream{input: bytes.NewReader([]byte("0000"))}
					ctx := WithWriteAuthorization(t.Context(), func(context.Context) error { return nil })
					if err := handler.ServeSSH(ctx, SSHRequest{
						Namespace: "alice", Repository: "demo", Service: service, Stream: stream,
					}); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(stream.output.String(), "refs/heads/main") {
						t.Fatal("missing advertised branch")
					}
				}
				if objects.objectReads != 0 {
					t.Fatalf("ref-only request read %d Git objects; want zero", objects.objectReads)
				}
			})
		}
	}
}

func TestReadPktStrict(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "0", "000", "+004", "-004", "0001", "0002", "0003", "0005", "ffff", "000g"} {
		if _, _, err := readPkt(bytes.NewReader([]byte(value))); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	data, flush, err := readPkt(bytes.NewReader([]byte(pkt("hello"))))
	if err != nil || flush || string(data) != "hello" {
		t.Fatalf("valid packet = %s %v %v", data, flush, err)
	}
}

func TestUploadRejectsUnadvertisedWant(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	_, err := h.upload(context.Background(), &repository.GitSnapshot{References: map[string]string{}, Objects: map[string]repository.GitObject{}}, []byte(pkt("want "+strings.Repeat("a", 40)+"\n")+"0000"+pkt("done\n")))
	if err == nil {
		t.Fatal("unadvertised object accepted")
	}
}

func TestReceiveReportsInvalidPack(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	newID := strings.Repeat("a", 40)
	body := []byte(pkt(zeroID+" "+newID+" refs/heads/main\x00report-status\n") + "0000invalid pack")
	result, err := h.receive(t.Context(), &repository.GitSnapshot{}, body)
	if err != nil {
		t.Fatalf("unpack failure must use Git report-status, not a transport error: %v", err)
	}
	want := pkt("unpack invalid pack\n") + pkt("ng refs/heads/main unpack failed\n") + "0000"
	if string(result) != want {
		t.Fatalf("report-status = %q, want %q", result, want)
	}
}

func FuzzReadPkt(f *testing.F) {
	f.Add([]byte("0000"))
	f.Add([]byte("0009hello"))
	f.Fuzz(func(t *testing.T, data []byte) { _, _, _ = readPkt(bytes.NewReader(data)) })
}
