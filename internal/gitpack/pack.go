// Package gitpack reads and writes bounded Git packs. Incoming object bodies and
// delta instructions are staged in a private temporary directory; only indexes
// are kept in memory. Callers must close a successfully decoded Workspace.
package gitpack

import (
	"compress/zlib"
	"context"
	"crypto/sha1" // #nosec G505 -- Git's wire format requires SHA-1, independently checked with SHA-256.
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"os"
	"slices"
)

var (
	ErrInvalid  = errors.New("gitpack: invalid pack or object")
	ErrLimit    = errors.New("gitpack: limit exceeded")
	ErrNotFound = errors.New("gitpack: object not found")
)

// Limits must have positive values. MaxDecodedBytes includes delta instructions
// and reconstructed bodies, preventing small delta packs from bypassing budgets.
// MaxDiskBytes bounds temporary file contents, including external delta bases.
type Limits struct {
	MaxPackBytes    int64
	MaxObjectBytes  int64
	MaxDecodedBytes int64
	MaxDiskBytes    int64
	MaxObjects      int
	MaxDeltaDepth   int
}

func (l Limits) valid() bool {
	return l.MaxPackBytes >= 32 && l.MaxPackBytes < math.MaxInt64 &&
		l.MaxObjectBytes > 0 && l.MaxObjectBytes < math.MaxInt64 &&
		l.MaxDecodedBytes > 0 && l.MaxDiskBytes > 0 &&
		l.MaxObjects > 0 && uint64(l.MaxObjects) <= math.MaxUint32 && l.MaxDeltaDepth > 0
}

// Object is an individual Git object's uncompressed body.
type Object struct {
	Type string
	Data []byte
}

// Resolver returns ErrNotFound for unavailable objects. Decode uses it for thin
// pack bases; Write uses it to load one object at a time.
type Resolver func(context.Context, string) (Object, error)

// Entry describes one non-delta pack entry. Offset and Length cover its packed
// header and zlib stream. SHA256 covers the canonical Git header and body, while
// CRC32 covers the encoded pack entry, as in a Git pack index.
type Entry struct {
	ID     string
	Type   string
	SHA256 string
	Size   int64
	Offset int64
	Length int64
	CRC32  uint32
}

type staged struct {
	Entry
	path       string
	baseID     string
	baseOffset int64
	depth      int
}

// Workspace owns a private directory of verified uncompressed object bodies.
// Methods must not run concurrently with Close. Mutations are internal to Decode.
type Workspace struct {
	dir          string
	limits       Limits
	diskBytes    int64
	decodedBytes int64
	objects      map[string]*staged
	entries      []*staged
}

// Close removes all staged data. It is safe to call more than once.
func (w *Workspace) Close() error {
	if w == nil || w.dir == "" {
		return nil
	}
	err := os.RemoveAll(w.dir)
	if err == nil {
		w.dir = ""
	}
	return err
}

// IDs returns the unique incoming object IDs in sorted order.
func (w *Workspace) IDs() []string {
	ids := make([]string, 0, len(w.objects))
	for id := range w.objects {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Info reports verified object metadata. Decoded delta entries have no canonical
// pack location until Write has encoded them.
func (w *Workspace) Info(id string) (Entry, bool) {
	entry, ok := w.objects[id]
	if !ok {
		return Entry{}, false
	}
	return entry.Entry, true
}

// Get loads one object and verifies its stored identity and size.
func (w *Workspace) Get(ctx context.Context, id string) (_ Object, resultErr error) {
	entry, ok := w.objects[id]
	if !ok {
		return Object{}, ErrNotFound
	}
	if w.dir == "" {
		return Object{}, os.ErrClosed
	}
	f, err := os.Open(entry.path)
	if err != nil {
		return Object{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, entry.Size+1))
	if err != nil {
		return Object{}, err
	}
	object := Object{Type: entry.Type, Data: data}
	got := objectEntry(object)
	if got.Size != entry.Size || got.ID != id || got.SHA256 != entry.SHA256 {
		return Object{}, ErrInvalid
	}
	return object, nil
}

// Decode consumes exactly one complete PACK stream, verifies its checksum, and
// resolves OFS/REF deltas with bounded disk and decoded byte budgets. On failure,
// it removes its temporary directory. It stops at the checksum without waiting
// for EOF (SSH keeps the connection open for the response). The caller validates
// trailing data if its transport requires EOF. A buffered input implementing
// io.ByteReader is reused, preserving bytes already read from the transport.
func Decode(ctx context.Context, input io.Reader, limits Limits, resolve Resolver) (_ *Workspace, resultErr error) {
	if !limits.valid() {
		return nil, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "gitone-pack-*")
	if err != nil {
		return nil, err
	}
	w := &Workspace{dir: dir, limits: limits, objects: make(map[string]*staged)}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, w.Close())
		}
	}()
	reader, ok := input.(byteReadReader)
	if !ok {
		reader = byteReader{input}
	}
	r := &packReader{reader: &boundedReader{ctx: ctx, reader: reader, remaining: limits.MaxPackBytes}, sum: sha1.New()} // #nosec G401 -- Git pack checksum.
	var header [12]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, invalid(err)
	}
	version := binary.BigEndian.Uint32(header[4:8])
	count := binary.BigEndian.Uint32(header[8:12])
	if string(header[:4]) != "PACK" || (version != 2 && version != 3) {
		return nil, ErrInvalid
	}
	if uint64(count) > uint64(limits.MaxObjects) { // #nosec G115 -- valid() rejected nonpositive limits above.
		return nil, ErrLimit
	}
	w.entries = make([]*staged, 0, int(count))
	byOffset := make(map[int64]*staged, int(count))
	for range count {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := &staged{Entry: Entry{Offset: r.count}, baseOffset: -1}
		r.crc = crc32.NewIEEE()
		kind, size, err := readHeader(r)
		if err != nil {
			return nil, err
		}
		if size > limits.MaxObjectBytes {
			return nil, ErrLimit
		}
		entry.Type = typeName(kind)
		switch kind {
		case 1, 2, 3, 4:
		case 6:
			distance, err := readDistance(r)
			if err != nil || distance <= 0 || distance > entry.Offset {
				return nil, invalid(err)
			}
			entry.baseOffset = entry.Offset - distance
			if byOffset[entry.baseOffset] == nil {
				return nil, ErrInvalid
			}
		case 7:
			var id [20]byte
			if _, err := io.ReadFull(r, id[:]); err != nil {
				return nil, invalid(err)
			}
			entry.baseID = hex.EncodeToString(id[:])
		default:
			return nil, ErrInvalid
		}
		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, invalid(err)
		}
		path, err := w.stage(ctx, zr, size)
		err = errors.Join(err, zr.Close())
		if err != nil {
			return nil, invalid(err)
		}
		entry.path, entry.Size = path, size
		entry.Length, entry.CRC32 = r.count-entry.Offset, r.crc.Sum32()
		if r.count > limits.MaxPackBytes-20 {
			return nil, ErrLimit
		}
		if entry.Type != "" {
			if err := w.identify(ctx, entry); err != nil {
				return nil, err
			}
		}
		w.entries = append(w.entries, entry)
		byOffset[entry.Offset] = entry
	}
	checksum := r.sum.Sum(nil)
	r.sum, r.crc = nil, nil
	var trailer [20]byte
	if _, err := io.ReadFull(r, trailer[:]); err != nil {
		return nil, invalid(err)
	}
	if !slices.Equal(checksum, trailer[:]) {
		return nil, ErrInvalid
	}
	if err := w.resolve(ctx, byOffset, resolve); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Workspace) stage(ctx context.Context, r io.Reader, size int64) (string, error) {
	if size < 0 || size > w.limits.MaxObjectBytes || size > w.limits.MaxDecodedBytes-w.decodedBytes || size > w.limits.MaxDiskBytes-w.diskBytes {
		return "", ErrLimit
	}
	f, err := os.CreateTemp(w.dir, "object-*")
	if err != nil {
		return "", err
	}
	n, copyErr := io.CopyN(f, contextReader{ctx, r}, size)
	var extra [1]byte
	_, endErr := io.ReadFull(contextReader{ctx, r}, extra[:])
	closeErr := f.Close()
	if copyErr != nil || !errors.Is(endErr, io.EOF) || closeErr != nil {
		return "", invalid(errors.Join(copyErr, endErr, closeErr))
	}
	w.diskBytes += n
	w.decodedBytes += n
	return f.Name(), nil
}

func (w *Workspace) identify(ctx context.Context, entry *staged) (resultErr error) {
	f, err := os.Open(entry.path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	gitHash, strongHash := objectHashes(entry.Type, entry.Size)
	n, err := io.Copy(io.MultiWriter(gitHash, strongHash), contextReader{ctx, f})
	if err != nil {
		return err
	}
	if n != entry.Size {
		return ErrInvalid
	}
	entry.ID, entry.SHA256 = hex.EncodeToString(gitHash.Sum(nil)), hex.EncodeToString(strongHash.Sum(nil))
	if prior := w.objects[entry.ID]; prior != nil && (prior.Type != entry.Type || prior.Size != entry.Size || prior.SHA256 != entry.SHA256) {
		return ErrInvalid
	}
	w.objects[entry.ID] = entry
	return nil
}

func objectHashes(kind string, size int64) (hash.Hash, hash.Hash) {
	gitHash, strongHash := sha1.New(), sha256.New() // #nosec G401 -- Git object identity, independently protected by SHA-256.
	header := fmt.Sprintf("%s %d\x00", kind, size)
	_, _ = io.WriteString(gitHash, header)
	_, _ = io.WriteString(strongHash, header)
	return gitHash, strongHash
}

func objectEntry(object Object) Entry {
	gitHash, strongHash := objectHashes(object.Type, int64(len(object.Data)))
	_, _ = gitHash.Write(object.Data)
	_, _ = strongHash.Write(object.Data)
	return Entry{ID: hex.EncodeToString(gitHash.Sum(nil)), Type: object.Type, Size: int64(len(object.Data)), SHA256: hex.EncodeToString(strongHash.Sum(nil))}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type packReader struct {
	reader byteReadReader
	sum    hash.Hash
	crc    hash.Hash32
	count  int64
}

type byteReadReader interface {
	io.Reader
	io.ByteReader
}

type byteReader struct{ io.Reader }

func (r byteReader) ReadByte() (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r.Reader, b[:])
	return b[0], err
}

type boundedReader struct {
	ctx       context.Context
	reader    byteReadReader
	remaining int64
}

func (r *boundedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining == 0 {
		return 0, ErrLimit
	}
	if int64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func (r *boundedReader) ReadByte() (byte, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining == 0 {
		return 0, ErrLimit
	}
	b, err := r.reader.ReadByte()
	if err == nil {
		r.remaining--
	}
	return b, err
}

func (r *packReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.record(p[:n])
	return n, err
}

func (r *packReader) ReadByte() (byte, error) {
	b, err := r.reader.ReadByte()
	if err == nil {
		r.record([]byte{b})
	}
	return b, err
}

func (r *packReader) record(p []byte) {
	r.count += int64(len(p))
	if r.sum != nil {
		_, _ = r.sum.Write(p)
	}
	if r.crc != nil {
		_, _ = r.crc.Write(p)
	}
}

func readHeader(r io.ByteReader) (byte, int64, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, 0, invalid(err)
	}
	kind, size := b>>4&7, uint64(b&15)
	for shift := uint(4); b&128 != 0; shift += 7 {
		if shift >= 63 {
			return 0, 0, ErrInvalid
		}
		b, err = r.ReadByte()
		if err != nil {
			return 0, 0, invalid(err)
		}
		value := uint64(b & 127)
		if value > uint64(math.MaxInt64)>>shift {
			return 0, 0, ErrInvalid
		}
		size |= value << shift
	}
	return kind, int64(size), nil
}

func readDistance(r io.ByteReader) (int64, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, invalid(err)
	}
	distance := int64(b & 127)
	for b&128 != 0 {
		if distance >= math.MaxInt64>>7 {
			return 0, ErrInvalid
		}
		b, err = r.ReadByte()
		if err != nil {
			return 0, invalid(err)
		}
		distance = ((distance + 1) << 7) | int64(b&127)
	}
	return distance, nil
}

func typeName(kind byte) string {
	switch kind {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return ""
}

func typeCode(kind string) byte {
	switch kind {
	case "commit":
		return 1
	case "tree":
		return 2
	case "blob":
		return 3
	case "tag":
		return 4
	}
	return 0
}

func invalid(err error) error {
	if errors.Is(err, ErrLimit) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.Join(ErrInvalid, err)
}
