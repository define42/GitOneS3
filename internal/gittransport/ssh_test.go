package gittransport

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

type sshTestStream struct {
	input  *bytes.Reader
	output bytes.Buffer
}

func (s *sshTestStream) Read(data []byte) (int, error)  { return s.input.Read(data) }
func (s *sshTestStream) Write(data []byte) (int, error) { return s.output.Write(data) }

func sshTestHandler(t *testing.T) (*Handler, *repository.GitSnapshot) {
	t.Helper()
	store, err := repository.New(storage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(t.Context(), "alice", repository.CreateInput{
		Name: "demo", CreatedBy: "alice", AuthorName: "Alice",
		AuthorEmail: "alice@example.test", InitializeReadme: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadGit(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	return handler, snapshot
}

func TestServeSSHAdvertisement(t *testing.T) {
	t.Parallel()
	for _, service := range []string{upload, receive} {
		t.Run(service, func(t *testing.T) {
			handler, snapshot := sshTestHandler(t)
			stream := &sshTestStream{input: bytes.NewReader([]byte("0000"))}
			ctx := WithWriteAuthorization(t.Context(), func(context.Context) error { return nil })
			err := handler.ServeSSH(ctx, SSHRequest{
				Namespace: "alice", Repository: "demo", Service: service, Stream: stream,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stream.output.Bytes(), referenceAdvertisement(snapshot, service)) {
				t.Fatalf("SSH advertisement has HTTP framing: %q", stream.output.String())
			}
		})
	}
}

func TestServeSSHAdmission(t *testing.T) {
	t.Parallel()
	handler, _ := sshTestHandler(t)
	for _, tc := range []struct {
		name, service string
		stream        io.ReadWriter
	}{
		{name: "unknown service", service: "shell", stream: &bytes.Buffer{}},
		{name: "missing stream", service: upload},
		{name: "missing write authorization", service: receive, stream: &bytes.Buffer{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := handler.ServeSSH(t.Context(), SSHRequest{
				Namespace: "alice", Repository: "demo", Service: tc.service, Stream: tc.stream,
			}); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
	handler.slots <- struct{}{}
	defer func() { <-handler.slots }()
	stream := &sshTestStream{input: bytes.NewReader([]byte("0000"))}
	if err := handler.ServeSSH(t.Context(), SSHRequest{
		Namespace: "alice", Repository: "demo", Service: upload, Stream: stream,
	}); err == nil || stream.output.Len() != 0 {
		t.Fatal("busy SSH session accessed repository")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/alice/demo.git/info/refs?service=git-upload-pack", nil,
	))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatal("HTTP and SSH do not share admission")
	}
}

func TestServeSSHFetchNegotiation(t *testing.T) {
	t.Parallel()
	handler, snapshot := sshTestHandler(t)
	server, client := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	if err := server.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := client.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- handler.ServeSSH(t.Context(), SSHRequest{
			Namespace: "alice", Repository: "demo", Service: upload, Stream: server,
		})
	}()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-finished; err != nil {
			t.Error(err)
		}
	})
	for {
		_, flush, err := readPkt(client)
		if err != nil {
			t.Fatal(err)
		}
		if flush {
			break
		}
	}
	id := snapshot.References["refs/heads/main"]
	if err := writeSSH(client, []byte(pkt("want "+id+" side-band-64k\n")+"0000")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := writeSSH(client, []byte(pkt("have "+id+"\n")+"0000")); err != nil {
			t.Fatal(err)
		}
		line, flush, err := readPkt(client)
		if err != nil || flush || string(line) != "NAK\n" {
			t.Fatalf("negotiation response = %q, %v, %v", line, flush, err)
		}
	}
	if err := writeSSH(client, []byte(pkt("done\n"))); err != nil {
		t.Fatal(err)
	}
	line, _, err := readPkt(client)
	if err != nil || string(line) != "NAK\n" {
		t.Fatalf("final acknowledgement = %q, %v", line, err)
	}
	var pack bytes.Buffer
	for {
		line, flush, err := readPkt(client)
		if err != nil {
			t.Fatal(err)
		}
		if flush {
			break
		}
		if len(line) == 0 || line[0] != 1 {
			t.Fatalf("invalid sideband %q", line)
		}
		pack.Write(line[1:])
	}
	objects, err := decodePack(t.Context(), pack.Bytes(), nil)
	if err != nil || len(objects) != len(snapshot.Objects) {
		t.Fatalf("negotiated pack has %d objects: %v", len(objects), err)
	}
}

func TestServeSSHPushRechecksAuthorization(t *testing.T) {
	t.Parallel()
	for _, revoked := range []bool{false, true} {
		name := "allowed"
		if revoked {
			name = "revoked"
		}
		t.Run(name, func(t *testing.T) {
			handler, snapshot := sshTestHandler(t)
			id := snapshot.References["refs/heads/main"]
			commands := []byte(pkt(zeroID+" "+id+" refs/heads/feature\x00report-status\n") + "0000")
			pack, err := encodePack(t.Context(), map[string]repository.GitObject{})
			if err != nil {
				t.Fatal(err)
			}
			// Trailing bytes model an open stdin: receiving a push must stop at
			// the pack checksum, not consume data until EOF.
			input := append(append(commands, pack...), []byte("must not be read")...)
			stream := &sshTestStream{input: bytes.NewReader(input)}
			checks := 0
			ctx := WithWriteAuthorization(t.Context(), func(context.Context) error {
				checks++
				if revoked {
					return repository.ErrForbidden
				}
				return nil
			})
			if err := handler.ServeSSH(ctx, SSHRequest{
				Namespace: "alice", Repository: "demo", Service: receive, Stream: stream,
			}); err != nil {
				t.Fatal(err)
			}
			if checks != 1 || stream.input.Len() != len("must not be read") {
				t.Fatalf("authorization checks=%d, unread bytes=%d", checks, stream.input.Len())
			}
			current, err := handler.store.ReadGit(t.Context(), "alice", "demo")
			if err != nil {
				t.Fatal(err)
			}
			if revoked {
				if current.References["refs/heads/feature"] != "" ||
					!strings.Contains(stream.output.String(), "write permission revoked or expired") {
					t.Fatal("revoked push was not rejected")
				}
				return
			}
			if current.References["refs/heads/feature"] != id {
				t.Fatal("successful push did not update reference")
			}
		})
	}
}

func TestReadStreamPack(t *testing.T) {
	t.Parallel()
	object := repository.GitObject{Type: "blob", Data: []byte(strings.Repeat("hello\n", 100))}
	valid, err := encodePack(t.Context(), map[string]repository.GitObject{repository.GitObjectID(object): object})
	if err != nil {
		t.Fatal(err)
	}
	countLimit := append([]byte{}, valid[:12]...)
	binary.BigEndian.PutUint32(countLimit[8:], repository.MaxGitObjects+1)
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{name: "valid", data: valid},
		{name: "truncated header", data: valid[:6], want: io.ErrUnexpectedEOF},
		{name: "invalid magic", data: []byte("FAIL12345678"), want: errPack},
		{name: "object count limit", data: countLimit, want: repository.ErrLimit},
		{name: "truncated checksum", data: valid[:len(valid)-1], want: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &packCapture{reader: bytes.NewReader(tc.data)}
			err := readStreamPack(t.Context(), reader)
			if !errors.Is(err, tc.want) {
				t.Fatalf("readStreamPack = %v, want %v", err, tc.want)
			}
			if tc.want == nil && !bytes.Equal(reader.data.Bytes(), valid) {
				t.Fatal("captured pack changed")
			}
		})
	}
}
