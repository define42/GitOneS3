package gittransport

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1" // #nosec G505 -- Git's SHA-1 pack wire format mandates this checksum; it is not authentication.
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"slices"

	"github.com/define42/GitOneS3/internal/repository"
)

const maxPackBytes = repository.MaxGitBytes + (8 << 20)
const maxDeltaDepth = 64

var errPack = errors.New("gittransport: invalid pack")

type packedObject struct {
	offset     int
	baseOffset int
	baseID     string
	kind       string
	data       []byte
	id         string
	depth      int
}

// decodePack bounds advertised sizes before decompression and bounds delta
// targets before allocating. Its input has already passed the HTTP body limit.
// Format: https://git-scm.com/docs/gitformat-pack
func decodePack(ctx context.Context, data []byte, existing map[string]repository.GitObject) (map[string]repository.GitObject, error) {
	if len(data) < 32 || len(data) > maxPackBytes || string(data[:4]) != "PACK" {
		return nil, errPack
	}
	version := binary.BigEndian.Uint32(data[4:8])
	count := binary.BigEndian.Uint32(data[8:12])
	if (version != 2 && version != 3) || count > repository.MaxGitObjects {
		return nil, errPack
	}
	sum := sha1.Sum(data[:len(data)-20]) // #nosec G401 -- Verify the Git-mandated pack checksum, not an authentication credential.
	if !bytes.Equal(sum[:], data[len(data)-20:]) {
		return nil, errPack
	}
	r := bytes.NewReader(data[12 : len(data)-20])
	entries := make([]*packedObject, 0, count)
	byOffset := map[int]*packedObject{}
	byID := map[string]*packedObject{}
	objects := map[string]repository.GitObject{}
	total := 0
	for range count {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		offset := len(data) - 20 - r.Len()
		first, err := r.ReadByte()
		if err != nil {
			return nil, errPack
		}
		kind := (first >> 4) & 7
		size := uint64(first & 15)
		last := first
		for shift := uint(4); last&128 != 0; shift += 7 {
			if shift > 25 {
				return nil, errPack
			}
			last, err = r.ReadByte()
			if err != nil {
				return nil, errPack
			}
			size |= uint64(last&127) << shift
		}
		if size > repository.MaxGitObjectBytes {
			return nil, repository.ErrLimit
		}
		entry := &packedObject{offset: offset, baseOffset: -1}
		switch kind {
		case 1:
			entry.kind = "commit"
		case 2:
			entry.kind = "tree"
		case 3:
			entry.kind = "blob"
		case 4:
			entry.kind = "tag"
		case 6:
			b, err := r.ReadByte()
			if err != nil {
				return nil, errPack
			}
			distance := int64(b & 127)
			for n := 0; b&128 != 0; n++ {
				if n >= 8 {
					return nil, errPack
				}
				b, err = r.ReadByte()
				if err != nil {
					return nil, errPack
				}
				distance = ((distance + 1) << 7) | int64(b&127)
				if distance > int64(offset) {
					return nil, errPack
				}
			}
			entry.baseOffset = offset - int(distance)
			if distance == 0 || byOffset[entry.baseOffset] == nil {
				return nil, errPack
			}
		case 7:
			var hash [20]byte
			if _, err := io.ReadFull(r, hash[:]); err != nil {
				return nil, errPack
			}
			entry.baseID = hex.EncodeToString(hash[:])
		default:
			return nil, errPack
		}
		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, errPack
		}
		content, readErr := io.ReadAll(io.LimitReader(zr, int64(size)+1))
		closeErr := zr.Close()
		if readErr != nil || closeErr != nil || uint64(len(content)) != size {
			return nil, errPack
		}
		total += len(content)
		if total > repository.MaxGitBytes {
			return nil, repository.ErrLimit
		}
		entry.data = content
		if entry.kind != "" {
			entry.id = repository.GitObjectID(repository.GitObject{Type: entry.kind, Data: content})
			if prior, ok := objects[entry.id]; ok && (prior.Type != entry.kind || !bytes.Equal(prior.Data, content)) {
				return nil, errPack
			}
			objects[entry.id] = repository.GitObject{Type: entry.kind, Data: content}
			byID[entry.id] = entry
		}
		entries = append(entries, entry)
		byOffset[offset] = entry
	}
	if r.Len() != 0 {
		return nil, errPack
	}
	// A bounded iterative resolver accepts forward REF deltas and thin packs.
	// Each pass advances at least one layer; cycles/missing bases fail closed.
	for pass := 0; pass <= maxDeltaDepth; pass++ {
		pending, progress := 0, false
		for _, entry := range entries {
			if entry.id != "" {
				continue
			}
			pending++
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var base repository.GitObject
			depth := 0
			if entry.baseOffset >= 0 {
				parent := byOffset[entry.baseOffset]
				if parent.id == "" {
					continue
				}
				base = objects[parent.id]
				depth = parent.depth
			} else {
				var ok bool
				base, ok = objects[entry.baseID]
				if !ok {
					base, ok = existing[entry.baseID]
				}
				if !ok {
					continue
				}
				if parent := byID[entry.baseID]; parent != nil {
					depth = parent.depth
				}
			}
			if depth >= maxDeltaDepth {
				return nil, repository.ErrLimit
			}
			content, err := applyDelta(base.Data, entry.data)
			if err != nil {
				return nil, err
			}
			total += len(content)
			if total > repository.MaxGitBytes {
				return nil, repository.ErrLimit
			}
			entry.kind, entry.data, entry.depth = base.Type, content, depth+1
			object := repository.GitObject{Type: entry.kind, Data: content}
			entry.id = repository.GitObjectID(object)
			if prior, ok := objects[entry.id]; ok && (prior.Type != object.Type || !bytes.Equal(prior.Data, object.Data)) {
				return nil, errPack
			}
			objects[entry.id] = object
			byID[entry.id] = entry
			progress = true
		}
		if pending == 0 {
			return objects, nil
		}
		if !progress {
			return nil, errPack
		}
	}
	return nil, repository.ErrLimit
}

func deltaSize(r *bytes.Reader) (uint64, error) {
	var size uint64
	for shift := uint(0); shift <= 28; shift += 7 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, errPack
		}
		size |= uint64(b&127) << shift
		if b&128 == 0 {
			return size, nil
		}
	}
	return 0, errPack
}

func applyDelta(base, delta []byte) ([]byte, error) {
	r := bytes.NewReader(delta)
	source, err := deltaSize(r)
	if err != nil || source != uint64(len(base)) {
		return nil, errPack
	}
	target, err := deltaSize(r)
	if err != nil {
		return nil, errPack
	}
	if target > repository.MaxGitObjectBytes {
		return nil, repository.ErrLimit
	}
	result := make([]byte, 0, int(target))
	for r.Len() > 0 {
		op, err := r.ReadByte()
		if err != nil || op == 0 {
			return nil, errPack
		}
		if op&128 == 0 {
			n := int(op)
			if len(result)+n > int(target) || r.Len() < n {
				return nil, errPack
			}
			start := len(result)
			result = append(result, make([]byte, n)...)
			if _, err := io.ReadFull(r, result[start:]); err != nil {
				return nil, errPack
			}
			continue
		}
		var offset, size uint32
		for i := range uint(7) {
			if op&(1<<i) == 0 {
				continue
			}
			b, err := r.ReadByte()
			if err != nil {
				return nil, errPack
			}
			if i < 4 {
				offset |= uint32(b) << (8 * i)
			} else {
				size |= uint32(b) << (8 * (i - 4))
			}
		}
		if size == 0 {
			size = 0x10000
		}
		if uint64(offset)+uint64(size) > uint64(len(base)) || uint64(len(result))+uint64(size) > target {
			return nil, errPack
		}
		result = append(result, base[int(offset):int(offset)+int(size)]...)
	}
	if uint64(len(result)) != target {
		return nil, errPack
	}
	return result, nil
}

func encodePack(ctx context.Context, objects map[string]repository.GitObject) ([]byte, error) {
	count := len(objects)
	if count > repository.MaxGitObjects {
		return nil, repository.ErrLimit
	}
	var output bytes.Buffer
	output.WriteString("PACK")
	output.Write(binary.BigEndian.AppendUint32(nil, 2))
	output.Write(binary.BigEndian.AppendUint32(nil, uint32(count)))
	ids := make([]string, 0, len(objects))
	for id := range objects {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		object := objects[id]
		var kind byte
		switch object.Type {
		case "commit":
			kind = 1
		case "tree":
			kind = 2
		case "blob":
			kind = 3
		case "tag":
			kind = 4
		default:
			return nil, errPack
		}
		size := len(object.Data)
		b := kind<<4 | byte(size&15)
		size >>= 4
		for size > 0 {
			output.WriteByte(b | 128)
			b = byte(size & 127)
			size >>= 7
		}
		output.WriteByte(b)
		writer := zlib.NewWriter(&output)
		if _, err := writer.Write(object.Data); err != nil {
			return nil, errors.Join(err, writer.Close())
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		if output.Len() > maxPackBytes-20 {
			return nil, repository.ErrLimit
		}
	}
	sum := sha1.Sum(output.Bytes()) // #nosec G401 -- Emit the checksum required by Git's SHA-1 pack wire format.
	output.Write(sum[:])
	return output.Bytes(), nil
}
