package gittransport

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1" // #nosec G505 -- Fixtures must use the checksum required by Git's SHA-1 pack wire format.
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"

	"github.com/define42/GitOneS3/internal/repository"
)

type packFixture struct {
	data  []byte
	count uint32
}

func (p *packFixture) add(t *testing.T, kind byte, base, data []byte) int {
	t.Helper()
	if p.data == nil {
		p.data = append([]byte("PACK"), 0, 0, 0, 2, 0, 0, 0, 0)
	}
	offset := len(p.data)
	size := len(data)
	b := kind<<4 | byte(size&15)
	size >>= 4
	for size > 0 {
		p.data = append(p.data, b|128)
		b = byte(size & 127)
		size >>= 7
	}
	p.data = append(p.data, b)
	p.data = append(p.data, base...)
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	p.data = append(p.data, compressed.Bytes()...)
	p.count++
	return offset
}

func (p *packFixture) finish() []byte {
	data := bytes.Clone(p.data)
	binary.BigEndian.PutUint32(data[8:12], p.count)
	sum := sha1.Sum(data) // #nosec G401 -- Produce the Git-mandated wire checksum for a pack fixture.
	return append(data, sum[:]...)
}

func ofsDistance(n int) []byte {
	result := []byte{byte(n & 127)}
	for n >>= 7; n > 0; n >>= 7 {
		n--
		result = append([]byte{byte(n&127) | 128}, result...)
	}
	return result
}

func TestDecodePackDeltas(t *testing.T) {
	t.Parallel()
	base := repository.GitObject{Type: "blob", Data: []byte("hello world")}
	id := repository.GitObjectID(base)
	hash, err := hex.DecodeString(id)
	if err != nil {
		t.Fatal(err)
	}
	// Copy 'hello ', insert 'git': source11 target9 copyoffset0 length6 insert3.
	delta := []byte{11, 9, 0x90, 6, 3, 'g', 'i', 't'}
	for _, name := range []string{"ofs", "ref", "thin", "forward"} {
		t.Run(name, func(t *testing.T) {
			var fixture packFixture
			existing := map[string]repository.GitObject{}
			switch name {
			case "ofs":
				offset := fixture.add(t, 3, nil, base.Data)
				fixture.add(t, 6, ofsDistance(len(fixture.data)-offset), delta)
			case "ref":
				fixture.add(t, 3, nil, base.Data)
				fixture.add(t, 7, hash, delta)
			case "thin":
				fixture.add(t, 7, hash, delta)
				existing[id] = base
			case "forward":
				fixture.add(t, 7, hash, delta)
				fixture.add(t, 3, nil, base.Data)
			}
			objects, err := decodePack(context.Background(), fixture.finish(), existing)
			if err != nil {
				t.Fatal(err)
			}
			want := repository.GitObject{Type: "blob", Data: []byte("hello git")}
			if got := objects[repository.GitObjectID(want)]; got.Type != "blob" || !bytes.Equal(got.Data, want.Data) {
				t.Fatalf("delta = %+v", got)
			}
		})
	}
}

func TestDecodePackRejectsCorruption(t *testing.T) {
	t.Parallel()
	var fixture packFixture
	fixture.add(t, 3, nil, []byte("test"))
	valid := fixture.finish()
	for cut := range len(valid) {
		if _, err := decodePack(context.Background(), valid[:cut], nil); err == nil {
			t.Fatalf("accepted truncation %d", cut)
		}
	}
	for i := range valid {
		corrupt := bytes.Clone(valid)
		corrupt[i] ^= 1
		if _, err := decodePack(context.Background(), corrupt, nil); err == nil {
			t.Fatalf("accepted corruption %d", i)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := decodePack(ctx, valid, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	for _, name := range []string{"count", "version", "huge object", "reserved type", "missing base", "trailing data", "decompression bomb"} {
		t.Run(name, func(t *testing.T) {
			var p packFixture
			switch name {
			case "missing base":
				p.add(t, 7, make([]byte, 20), []byte{0, 0})
			case "huge object":
				p.add(t, 3, nil, bytes.Repeat([]byte{'a'}, repository.MaxGitObjectBytes+1))
			case "reserved type":
				p.add(t, 5, nil, []byte("bad"))
			case "decompression bomb":
				p.add(t, 3, nil, bytes.Repeat([]byte{'a'}, repository.MaxGitObjectBytes*2))
				p.data = append(append(bytes.Clone(p.data[:12]), 0x31), p.data[16:]...)
			default:
				p.add(t, 3, nil, []byte("test"))
			}
			data := p.finish()
			if name == "count" {
				binary.BigEndian.PutUint32(data[8:12], 0xffffffff)
			}
			if name == "version" {
				binary.BigEndian.PutUint32(data[4:8], 9)
			}
			if name == "trailing data" {
				data = append(data[:len(data)-20], 0)
				data = append(data, make([]byte, 20)...)
			}
			sum := sha1.Sum(data[:len(data)-20]) // #nosec G401 -- Repair the wire checksum so validation reaches the corrupted fields.
			copy(data[len(data)-20:], sum[:])
			if _, err := decodePack(context.Background(), data, nil); err == nil {
				t.Fatal("accepted invalid pack")
			}
		})
	}
}

func TestDecodePackDeltaDepth(t *testing.T) {
	t.Parallel()
	for _, depth := range []int{64, 65} {
		var p packFixture
		previous := p.add(t, 3, nil, []byte{'a'})
		for n := 1; n <= depth; n++ {
			delta := []byte{byte(n), byte(n + 1), 0x90, byte(n), 1, 'a'}
			previous = p.add(t, 6, ofsDistance(len(p.data)-previous), delta)
		}
		_, err := decodePack(context.Background(), p.finish(), nil)
		if depth == 64 && err != nil {
			t.Fatalf("depth64 = %v", err)
		}
		if depth == 65 && !errors.Is(err, repository.ErrLimit) {
			t.Fatalf("depth65 = %v", err)
		}
	}
}

func TestApplyDeltaBounds(t *testing.T) {
	t.Parallel()
	for _, delta := range [][]byte{{}, {1}, {1, 1, 0}, {1, 1, 0x90, 2}, {1, 1, 0x91, 255, 1}, {1, 2, 1, 'a'}, {2, 1, 1, 'a'}, {1, 0x81, 0x80, 0x40, 1, 'a'}, {1, 1, 0x80}, {1, 1, 0xff}, {1, 1, 2, 'a', 'b'}} {
		if _, err := applyDelta([]byte{'a'}, delta); err == nil {
			t.Fatalf("accepted delta %x", delta)
		}
	}
}

func TestPackRoundTrip(t *testing.T) {
	t.Parallel()
	objects := map[string]repository.GitObject{}
	for _, kind := range []string{"blob", "tree", "commit", "tag"} {
		object := repository.GitObject{Type: kind, Data: []byte("test " + kind)}
		objects[repository.GitObjectID(object)] = object
	}
	encoded, err := encodePack(context.Background(), objects)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodePack(context.Background(), encoded, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(objects) {
		t.Fatal("missing objects")
	}
	for id, want := range objects {
		if got := decoded[id]; got.Type != want.Type || !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("object %s differs", id)
		}
	}
}

func TestEncodePackRejectsObjectCountAboveLimit(t *testing.T) {
	t.Parallel()
	objects := make(map[string]repository.GitObject, repository.MaxGitObjects+1)
	for i := range repository.MaxGitObjects + 1 {
		objects[strconv.Itoa(i)] = repository.GitObject{Type: "blob"}
	}
	data, err := encodePack(t.Context(), objects)
	if !errors.Is(err, repository.ErrLimit) || data != nil {
		t.Fatalf("pack above object limit: got %d bytes, error %v", len(data), err)
	}
}

func FuzzDecodePack(f *testing.F) {
	f.Add([]byte("PACK"))
	object := repository.GitObject{Type: "blob", Data: []byte("valid pack seed\n")}
	seed, err := encodePack(context.Background(), map[string]repository.GitObject{repository.GitObjectID(object): object})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		_, _ = decodePack(context.Background(), data, nil)
		// Keep the raw path above for checksum/truncation coverage, then repair
		// only the checksum so mutations can reach object and delta decoding.
		if len(data) >= 32 {
			checked := bytes.Clone(data)
			sum := sha1.Sum(checked[:len(checked)-20]) // #nosec G401 -- Repair the Git wire checksum to fuzz object and delta decoding.
			copy(checked[len(checked)-20:], sum[:])
			_, _ = decodePack(context.Background(), checked, nil)
		}
	})
}

func FuzzApplyDelta(f *testing.F) {
	f.Add([]byte("hello"), []byte{5, 5, 0x90, 5})
	f.Fuzz(func(t *testing.T, base, delta []byte) {
		if len(base) > 1<<20 || len(delta) > 1<<20 {
			return
		}
		_, _ = applyDelta(base, delta)
	})
}
