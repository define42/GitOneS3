package gitpack

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1" // #nosec G505 -- Git fixture checksums.
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testLimits() Limits {
	return Limits{MaxPackBytes: 128 << 20, MaxObjectBytes: 16 << 20, MaxDecodedBytes: 128 << 20, MaxDiskBytes: 256 << 20, MaxObjects: 100000, MaxDeltaDepth: 64}
}

func objectResolver(objects map[string]Object) Resolver {
	return func(ctx context.Context, id string) (Object, error) {
		if err := ctx.Err(); err != nil {
			return Object{}, err
		}
		object, ok := objects[id]
		if !ok {
			return Object{}, ErrNotFound
		}
		return object, nil
	}
}

func TestWriteDecodeAndRanges(t *testing.T) {
	objects := map[string]Object{}
	var ids []string
	for _, object := range []Object{{Type: "blob", Data: []byte("hello\n")}, {Type: "blob", Data: nil}, {Type: "tree", Data: nil}} {
		id := objectEntry(object).ID
		objects[id] = object
		ids = append(ids, id)
	}
	var pack bytes.Buffer
	entries, err := Write(t.Context(), &pack, ids, objectResolver(objects), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	w, err := Decode(t.Context(), bytes.NewReader(pack.Bytes()), testLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	})
	if len(w.IDs()) != len(ids) {
		t.Fatalf("got %d objects", len(w.IDs()))
	}
	for _, entry := range entries {
		got, err := w.Get(t.Context(), entry.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := objects[entry.ID]
		if got.Type != want.Type || !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("object mismatch: %s", entry.ID)
		}
		encoded := pack.Bytes()[entry.Offset : entry.Offset+entry.Length]
		got, err = DecodeEntry(t.Context(), bytes.NewReader(encoded), entry, testLimits().MaxObjectBytes)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Data, want.Data) {
			t.Fatal("range read mismatch")
		}
		corrupt := entry
		corrupt.SHA256 = strings.Repeat("0", 64)
		if _, err := DecodeEntry(t.Context(), bytes.NewReader(encoded), corrupt, testLimits().MaxObjectBytes); !errors.Is(err, ErrInvalid) {
			t.Fatalf("SHA-256: %v", err)
		}
		corrupt = entry
		corrupt.CRC32++
		if _, err := DecodeEntry(t.Context(), bytes.NewReader(encoded), corrupt, testLimits().MaxObjectBytes); !errors.Is(err, ErrInvalid) {
			t.Fatalf("CRC: %v", err)
		}
		if _, err := DecodeEntry(t.Context(), bytes.NewReader(append(bytes.Clone(encoded), 0)), entry, testLimits().MaxObjectBytes); !errors.Is(err, ErrInvalid) {
			t.Fatalf("trailing byte: %v", err)
		}
	}
	dir := w.dir
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace remained: %v", err)
	}
}

type fixtureEntry struct {
	kind         byte
	prefix, data []byte
}

func fixturePack(t *testing.T, entries ...fixtureEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("PACK")
	b.Write(binary.BigEndian.AppendUint32(nil, 2))
	b.Write(binary.BigEndian.AppendUint32(nil, uint32(len(entries)))) // #nosec G115 -- Fixtures contain fewer than 10 entries.
	for _, entry := range entries {
		b.Write(encodeHeader(entry.kind, int64(len(entry.data))))
		b.Write(entry.prefix)
		zw := zlib.NewWriter(&b)
		if _, err := zw.Write(entry.data); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha1.Sum(b.Bytes()) // #nosec G401 -- Git fixture checksum.
	b.Write(sum[:])
	return b.Bytes()
}

func TestForwardAndThinDeltas(t *testing.T) {
	base := Object{Type: "blob", Data: []byte("hello world")}
	id := objectEntry(base).ID
	rawID, err := hex.DecodeString(id)
	if err != nil {
		t.Fatal(err)
	}
	// Copy "hello", then append " there".
	delta := []byte{11, 11, 0x90, 5, 6, ' ', 't', 'h', 'e', 'r', 'e'}
	for _, thin := range []bool{false, true} {
		t.Run(fmt.Sprintf("thin=%v", thin), func(t *testing.T) {
			entries := []fixtureEntry{{kind: 7, prefix: rawID, data: delta}}
			var resolve Resolver
			if thin {
				resolve = objectResolver(map[string]Object{id: base})
			} else {
				entries = append(entries, fixtureEntry{kind: 3, data: base.Data})
			}
			w, err := Decode(t.Context(), bytes.NewReader(fixturePack(t, entries...)), testLimits(), resolve)
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestResource(t, w)
			want := Object{Type: "blob", Data: []byte("hello there")}
			got, err := w.Get(t.Context(), objectEntry(want).ID)
			if err != nil || !bytes.Equal(got.Data, want.Data) {
				t.Fatalf("delta result: %q %v", got.Data, err)
			}
			if thin && len(w.IDs()) != 1 {
				t.Fatal("external base included as incoming object")
			}
		})
	}
}

func TestDecodeLimitsAndCorruption(t *testing.T) {
	valid := fixturePack(t, fixtureEntry{kind: 3, data: []byte("test data")})
	tests := []struct {
		name   string
		data   []byte
		change func(*Limits)
		want   error
	}{
		{name: "checksum", data: append(bytes.Clone(valid[:len(valid)-1]), valid[len(valid)-1]^1), want: ErrInvalid},
		{name: "truncated", data: valid[:len(valid)-1], want: ErrInvalid},
		{name: "object bytes", data: valid, change: func(l *Limits) { l.MaxObjectBytes = 4 }, want: ErrLimit},
		{name: "decoded bytes", data: valid, change: func(l *Limits) { l.MaxDecodedBytes = 4 }, want: ErrLimit},
		{name: "disk bytes", data: valid, change: func(l *Limits) { l.MaxDiskBytes = 4 }, want: ErrLimit},
		{name: "pack bytes", data: valid, change: func(l *Limits) { l.MaxPackBytes = 32 }, want: ErrLimit},
		{name: "object count", data: fixturePack(t, fixtureEntry{kind: 3}, fixtureEntry{kind: 3}), change: func(l *Limits) { l.MaxObjects = 1 }, want: ErrLimit},
		{name: "missing base", data: fixturePack(t, fixtureEntry{kind: 7, prefix: make([]byte, 20), data: []byte{1, 1, 1, 'a'}}), want: ErrInvalid},
		{name: "bad offset", data: fixturePack(t, fixtureEntry{kind: 6, prefix: []byte{1}, data: []byte{1, 1, 1, 'a'}}), want: ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := testLimits()
			if tt.change != nil {
				tt.change(&limits)
			}
			w, err := Decode(t.Context(), bytes.NewReader(tt.data), limits, nil)
			if w != nil {
				closeTestResource(t, w)
				t.Fatal("workspace returned on failure")
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDeltaExpansionAndDepthLimits(t *testing.T) {
	base := Object{Type: "blob", Data: []byte("x")}
	baseID, err := hex.DecodeString(objectEntry(base).ID)
	if err != nil {
		t.Fatal(err)
	}
	// The target size is checked before any target body is allocated or written.
	pack := fixturePack(t, fixtureEntry{kind: 7, prefix: baseID, data: []byte{1, 0x80, 0x80, 0x80, 0x10}})
	if _, err := Decode(t.Context(), bytes.NewReader(pack), testLimits(), objectResolver(map[string]Object{objectEntry(base).ID: base})); !errors.Is(err, ErrLimit) {
		t.Fatalf("delta bomb: %v", err)
	}
	first := Object{Type: "blob", Data: []byte("xy")}
	firstID, err := hex.DecodeString(objectEntry(first).ID)
	if err != nil {
		t.Fatal(err)
	}
	pack = fixturePack(t,
		fixtureEntry{kind: 3, data: base.Data},
		fixtureEntry{kind: 7, prefix: baseID, data: []byte{1, 2, 2, 'x', 'y'}},
		fixtureEntry{kind: 7, prefix: firstID, data: []byte{2, 3, 3, 'x', 'y', 'z'}},
	)
	limits := testLimits()
	limits.MaxDeltaDepth = 1
	if _, err := Decode(t.Context(), bytes.NewReader(pack), limits, nil); !errors.Is(err, ErrLimit) {
		t.Fatalf("delta depth: %v", err)
	}
}

func TestDecodeStopsAtTrailer(t *testing.T) {
	pack := fixturePack(t, fixtureEntry{kind: 3, data: []byte("body")})
	for _, buffered := range []bool{false, true} {
		t.Run(fmt.Sprintf("buffered=%v", buffered), func(t *testing.T) {
			var r io.Reader = bytes.NewBuffer(append(bytes.Clone(pack), []byte("sentinel")...))
			if buffered {
				r = bufio.NewReader(r)
			}
			w, err := Decode(t.Context(), r, testLimits(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestResource(t, w)
			remainder, err := io.ReadAll(r)
			if err != nil || string(remainder) != "sentinel" {
				t.Fatalf("consumed beyond trailer: %q %v", remainder, err)
			}
		})
	}
}

func TestCancellationAndWriteFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Decode(ctx, bytes.NewReader(fixturePack(t)), testLimits(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("decode cancellation: %v", err)
	}
	if _, err := Write(ctx, io.Discard, nil, nil, testLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("write cancellation: %v", err)
	}
	object := Object{Type: "blob", Data: []byte("body")}
	id := objectEntry(object).ID
	if _, err := Write(t.Context(), shortWriter{}, []string{id}, objectResolver(map[string]Object{id: object}), testLimits()); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %v", err)
	}
	if _, err := Write(t.Context(), io.Discard, []string{id}, objectResolver(map[string]Object{id: {Type: "blob", Data: []byte("changed")}}), testLimits()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("identity mismatch: %v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestPackLargerThanFormerRepositoryLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("72 MiB streaming regression")
	}
	var ids []string
	byID := map[string]byte{}
	for i := range byte(18) {
		id := objectEntry(Object{Type: "blob", Data: bytes.Repeat([]byte{i}, 4<<20)}).ID
		ids = append(ids, id)
		byID[id] = i
	}
	resolve := func(ctx context.Context, id string) (Object, error) {
		return Object{Type: "blob", Data: bytes.Repeat([]byte{byID[id]}, 4<<20)}, ctx.Err()
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "large.pack"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, f)
	if _, err := Write(t.Context(), f, ids, resolve, testLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	w, err := Decode(t.Context(), bufio.NewReader(f), testLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, w)
	if len(w.IDs()) != 18 || w.diskBytes != 72<<20 {
		t.Fatalf("objects=%d disk=%d", len(w.IDs()), w.diskBytes)
	}
	got, err := w.Get(t.Context(), ids[10])
	if err != nil || len(got.Data) != 4<<20 || got.Data[0] != 10 {
		t.Fatalf("large object: size=%d err=%v", len(got.Data), err)
	}
}

func TestNativeGitPacks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	git := func(input []byte, args ...string) []byte {
		t.Helper()
		// #nosec G204 -- Fixed native Git test arguments, executed without a shell.
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Pack Test", "GIT_AUTHOR_EMAIL=pack@example.test", "GIT_COMMITTER_NAME=Pack Test", "GIT_COMMITTER_EMAIL=pack@example.test")
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, stderr.String())
		}
		return out
	}
	git(nil, "init", "--quiet")
	var base string
	for i := range 8 {
		content := strings.Repeat("line common to all versions\n", 800) + fmt.Sprintf("version %d\n", i)
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		git(nil, "add", "file.txt")
		git(nil, "commit", "--quiet", "-m", fmt.Sprintf("revision %d", i))
		if i == 6 {
			base = strings.TrimSpace(string(git(nil, "rev-parse", "HEAD")))
		}
	}
	for _, offsets := range []bool{false, true} {
		t.Run(fmt.Sprintf("offsets=%v", offsets), func(t *testing.T) {
			args := []string{"pack-objects", "--stdout", "--all", "--window=50", "--depth=50", "--no-reuse-object"}
			if offsets {
				args = append(args, "--delta-base-offset")
			}
			pack := git(nil, args...)
			w, err := Decode(t.Context(), bytes.NewReader(pack), testLimits(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestResource(t, w)
			deltas := 0
			for _, entry := range w.entries {
				if entry.depth > 0 {
					deltas++
				}
			}
			if deltas == 0 {
				t.Fatal("native git did not produce deltas")
			}
			var rewritten bytes.Buffer
			if _, err := Write(t.Context(), &rewritten, w.IDs(), w.Get, testLimits()); err != nil {
				t.Fatal(err)
			}
			git(rewritten.Bytes(), "index-pack", "--stdin", "--strict")
		})
	}
	thin := git([]byte("HEAD\n^"+base+"\n"), "pack-objects", "--stdout", "--revs", "--thin", "--window=50")
	resolved := 0
	w, err := Decode(t.Context(), bytes.NewReader(thin), testLimits(), func(ctx context.Context, id string) (Object, error) {
		resolved++
		kind := strings.TrimSpace(string(git(nil, "cat-file", "-t", id)))
		return Object{Type: kind, Data: git(nil, "cat-file", kind, id)}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, w)
	if resolved == 0 {
		t.Fatal("native git did not produce a thin delta")
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x00"))
	f.Add([]byte{})
	object := Object{Type: "blob", Data: []byte("a valid compressed object")}
	id := objectEntry(object).ID
	var seed bytes.Buffer
	if _, err := Write(f.Context(), &seed, []string{id}, objectResolver(map[string]Object{id: object}), testLimits()); err != nil {
		f.Fatal(err)
	}
	f.Add(seed.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		limits := Limits{MaxPackBytes: 1 << 20, MaxObjectBytes: 64 << 10, MaxDecodedBytes: 1 << 20, MaxDiskBytes: 2 << 20, MaxObjects: 100, MaxDeltaDepth: 8}
		w, err := Decode(t.Context(), bytes.NewReader(data), limits, nil)
		if err == nil {
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func closeTestResource(t *testing.T, resource io.Closer) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Error(err)
	}
}
