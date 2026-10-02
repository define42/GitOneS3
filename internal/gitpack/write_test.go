package gitpack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"
)

func TestWriteCompressorReusePreservesCanonicalBytes(t *testing.T) {
	t.Parallel()
	noise := make([]byte, 64<<10)
	for i := range noise {
		noise[i] = byte((i*11939 + i/71) % 256)
	}
	objects := map[string]Object{}
	var ids []string
	for _, object := range []Object{
		{Type: "blob"},
		{Type: "tree"},
		{Type: "commit", Data: []byte("tree 0123456789012345678901234567890123456789\n\nmessage\n")},
		{Type: "tag", Data: []byte("object 0123456789012345678901234567890123456789\ntype commit\ntag v1\n\nrelease\n")},
		{Type: "blob", Data: bytes.Repeat([]byte("compressible payload"), 5000)},
		{Type: "blob", Data: noise},
		{Type: "blob", Data: []byte("last small stream")},
	} {
		id := objectEntry(object).ID
		ids = append(ids, id)
		objects[id] = object
	}
	slices.Sort(ids)
	fixtures := make([]fixtureEntry, 0, len(ids))
	for _, id := range ids {
		object := objects[id]
		fixtures = append(fixtures, fixtureEntry{kind: typeCode(object.Type), data: object.Data})
	}
	// This existing fixture encoder creates a fresh compressor per object.
	want := fixturePack(t, fixtures...)
	slices.Reverse(ids)
	var got bytes.Buffer
	entries, err := Write(t.Context(), &got, ids, objectResolver(objects), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatal("reusing the compressor changed the canonical pack bytes")
	}
	for _, entry := range entries {
		encoded := want[entry.Offset : entry.Offset+entry.Length]
		object, err := DecodeEntry(t.Context(), bytes.NewReader(encoded), entry, testLimits().MaxObjectBytes)
		if err != nil || object.Type != objects[entry.ID].Type || !bytes.Equal(object.Data, objects[entry.ID].Data) {
			t.Fatalf("independent stream %s: %v", entry.ID, err)
		}
	}
}

type failPackWriter struct {
	remaining int64
	err       error
}

type cancelPackWriter struct {
	written, cancelAt int64
	cancel            context.CancelFunc
}

func (w *cancelPackWriter) Write(p []byte) (int, error) {
	w.written += int64(len(p))
	if w.written >= w.cancelAt {
		w.cancel()
	}
	return len(p), nil
}

func (w *failPackWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		n := int(w.remaining)
		w.remaining = 0
		return n, w.err
	}
	w.remaining -= int64(len(p))
	return len(p), nil
}

func TestWriteReusedCompressorFailures(t *testing.T) {
	t.Parallel()
	objects := map[string]Object{}
	var ids []string
	for _, data := range []string{"first", "second", "third"} {
		object := Object{Type: "blob", Data: []byte(data)}
		id := objectEntry(object).ID
		objects[id], ids = object, append(ids, id)
	}
	entries, err := Write(t.Context(), io.Discard, ids, objectResolver(objects), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	second := entries[1]
	failure := errors.New("output failed")
	for _, test := range []struct {
		name  string
		limit int64
		err   error
		want  error
	}{
		{name: "zlib header", limit: second.Offset + 1, err: failure, want: failure},
		{name: "close checksum", limit: second.Offset + second.Length - 2, err: failure, want: failure},
		{name: "close short write", limit: second.Offset + second.Length - 2, want: io.ErrShortWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			resolve := func(ctx context.Context, id string) (Object, error) {
				calls++
				return objectResolver(objects)(ctx, id)
			}
			out := &failPackWriter{remaining: test.limit, err: test.err}
			if _, err := Write(t.Context(), out, ids, resolve, testLimits()); !errors.Is(err, test.want) {
				t.Fatalf("output failure = %v, want %v", err, test.want)
			}
			if calls != 2 {
				t.Fatalf("resolved %d objects after second stream failed", calls)
			}
		})
	}
	t.Run("cancellation during reused stream", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		resolve := func(_ context.Context, id string) (Object, error) {
			calls++
			return objects[id], nil
		}
		out := &cancelPackWriter{cancelAt: second.Offset + 2, cancel: cancel}
		if _, err := Write(ctx, out, ids, resolve, testLimits()); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation = %v", err)
		}
		if calls != 2 {
			t.Fatalf("resolved %d objects after cancellation", calls)
		}
	})
}
