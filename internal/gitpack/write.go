package gitpack

import (
	"bufio"
	"compress/zlib"
	"context"
	"crypto/sha1" // #nosec G505 -- Git's pack checksum format.
	"encoding/binary"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"slices"
)

// Write emits a canonical version 2 pack with independent zlib objects. IDs are
// sorted for deterministic output and duplicates are rejected. Memory usage is
// bounded by one object body plus compressor buffers and the returned index.
func Write(ctx context.Context, output io.Writer, ids []string, resolve Resolver, limits Limits) ([]Entry, error) {
	if !limits.valid() || len(ids) > limits.MaxObjects {
		return nil, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if resolve == nil && len(ids) != 0 {
		return nil, ErrInvalid
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	for i, id := range ids {
		if !validID(id) || (i > 0 && ids[i-1] == id) {
			return nil, ErrInvalid
		}
	}
	out := &packWriter{ctx: ctx, writer: output, sum: sha1.New(), limit: limits.MaxPackBytes} // #nosec G401 -- Git pack checksum.
	header := append([]byte("PACK"), 0, 0, 0, 2)
	header = binary.BigEndian.AppendUint32(header, uint32(len(ids))) // #nosec G115 -- len(ids) <= MaxObjects <= MaxUint32, checked above.
	if _, err := out.Write(header); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(ids))
	var total int64
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		object, err := resolve(ctx, id)
		if err != nil {
			return nil, err
		}
		kind, size := typeCode(object.Type), int64(len(object.Data))
		if kind == 0 {
			return nil, ErrInvalid
		}
		if size > limits.MaxObjectBytes || size > limits.MaxDecodedBytes-total {
			return nil, ErrLimit
		}
		total += size
		entry := objectEntry(object)
		if entry.ID != id {
			return nil, ErrInvalid
		}
		entry.Offset = out.count
		out.crc = crc32.NewIEEE()
		if _, err := out.Write(encodeHeader(kind, size)); err != nil {
			return nil, err
		}
		zw := zlib.NewWriter(out)
		_, writeErr := zw.Write(object.Data)
		if err := errors.Join(writeErr, zw.Close()); err != nil {
			return nil, err
		}
		entry.Length, entry.CRC32 = out.count-entry.Offset, out.crc.Sum32()
		entries = append(entries, entry)
	}
	checksum := out.sum.Sum(nil)
	out.sum, out.crc = nil, nil
	if _, err := out.Write(checksum); err != nil {
		return nil, err
	}
	return entries, nil
}

// DecodeEntry validates and decodes one exact range of a canonical pack. It
// checks the packed CRC, canonical header, object ID and independent SHA-256.
func DecodeEntry(ctx context.Context, input io.Reader, entry Entry, maxObjectBytes int64) (Object, error) {
	if entry.Size < 0 || entry.Size > maxObjectBytes || entry.Size == math.MaxInt64 {
		return Object{}, ErrLimit
	}
	if entry.Length <= 0 || entry.Length == math.MaxInt64 || !validID(entry.ID) || typeCode(entry.Type) == 0 {
		return Object{}, ErrInvalid
	}
	r := &packReader{reader: bufio.NewReader(io.LimitReader(contextReader{ctx, input}, entry.Length+1)), crc: crc32.NewIEEE()}
	kind, size, err := readHeader(r)
	if err != nil {
		return Object{}, err
	}
	if typeName(kind) != entry.Type || size != entry.Size {
		return Object{}, ErrInvalid
	}
	zr, err := zlib.NewReader(r)
	if err != nil {
		return Object{}, invalid(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(contextReader{ctx, zr}, size+1))
	if err := errors.Join(readErr, zr.Close()); err != nil {
		return Object{}, invalid(err)
	}
	if r.count != entry.Length || r.crc.Sum32() != entry.CRC32 {
		return Object{}, ErrInvalid
	}
	if _, err := r.ReadByte(); !errors.Is(err, io.EOF) {
		return Object{}, invalid(err)
	}
	object := Object{Type: entry.Type, Data: data}
	actual := objectEntry(object)
	if actual.ID != entry.ID || actual.Size != entry.Size || actual.SHA256 != entry.SHA256 {
		return Object{}, ErrInvalid
	}
	return object, nil
}

func encodeHeader(kind byte, size int64) []byte {
	result := make([]byte, 0, 10)
	b := kind<<4 | byte(size&15)
	size >>= 4
	for size > 0 {
		result = append(result, b|128)
		b = byte(size & 127)
		size >>= 7
	}
	return append(result, b)
}

func validID(id string) bool {
	if len(id) != 40 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

type packWriter struct {
	ctx    context.Context
	writer io.Writer
	sum    hash.Hash
	crc    hash.Hash32
	count  int64
	limit  int64
}

func (w *packWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.limit-w.count {
		return 0, ErrLimit
	}
	n, err := w.writer.Write(p)
	w.count += int64(n)
	if w.sum != nil {
		_, _ = w.sum.Write(p[:n])
	}
	if w.crc != nil {
		_, _ = w.crc.Write(p[:n])
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
