package gitpack

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
)

func (w *Workspace) resolve(ctx context.Context, byOffset map[int64]*staged, resolve Resolver) error {
	external := make(map[string]*staged)
	missing := make(map[string]bool)
	for pass := 0; pass <= w.limits.MaxDeltaDepth; pass++ {
		pending, progress := 0, false
		for _, entry := range w.entries {
			if entry.ID != "" {
				continue
			}
			pending++
			if err := ctx.Err(); err != nil {
				return err
			}
			var base *staged
			if entry.baseOffset >= 0 {
				base = byOffset[entry.baseOffset]
				if base.ID == "" {
					continue
				}
			} else {
				base = w.objects[entry.baseID]
				if base == nil {
					base = external[entry.baseID]
				}
				if base == nil && resolve != nil && !missing[entry.baseID] {
					object, err := resolve(ctx, entry.baseID)
					if errors.Is(err, ErrNotFound) {
						missing[entry.baseID] = true
						continue
					}
					if err != nil {
						return err
					}
					if int64(len(object.Data)) > w.limits.MaxObjectBytes {
						return ErrLimit
					}
					info := objectEntry(object)
					if typeCode(object.Type) == 0 || info.ID != entry.baseID {
						return ErrInvalid
					}
					path, err := w.stage(ctx, bytes.NewReader(object.Data), info.Size)
					if err != nil {
						return err
					}
					base = &staged{Entry: info, path: path}
					external[entry.baseID] = base
				}
				if base == nil {
					continue
				}
			}
			if base.depth >= w.limits.MaxDeltaDepth {
				return ErrLimit
			}
			if err := w.apply(ctx, entry, base); err != nil {
				return err
			}
			progress = true
		}
		if pending == 0 {
			return nil
		}
		if !progress {
			return ErrInvalid
		}
	}
	return ErrLimit
}

func (w *Workspace) apply(ctx context.Context, entry, base *staged) (resultErr error) {
	delta, err := os.Open(entry.path)
	if err != nil {
		return err
	}
	defer func() {
		if delta != nil {
			resultErr = errors.Join(resultErr, delta.Close())
		}
	}()
	r := bufio.NewReader(contextReader{ctx, delta})
	source, err := deltaSize(r)
	if err != nil || source != base.Size {
		return invalid(err)
	}
	target, err := deltaSize(r)
	if err != nil {
		return err
	}
	if target > w.limits.MaxObjectBytes || target > w.limits.MaxDecodedBytes-w.decodedBytes || target > w.limits.MaxDiskBytes-w.diskBytes {
		return ErrLimit
	}
	baseFile, err := os.Open(base.path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, baseFile.Close()) }()
	f, err := os.CreateTemp(w.dir, "resolved-*")
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			resultErr = errors.Join(resultErr, f.Close())
		}
	}()
	path := f.Name()
	gitHash, strongHash := objectHashes(base.Type, target)
	out := io.MultiWriter(f, gitHash, strongHash)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		op, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || op == 0 {
			return invalid(err)
		}
		if op&128 == 0 {
			count := int64(op)
			if count > target-written {
				return ErrInvalid
			}
			if _, err := io.CopyN(out, r, count); err != nil {
				return invalid(err)
			}
			written += count
			continue
		}
		var offset, count int64
		for i := range uint(7) {
			if op&(1<<i) == 0 {
				continue
			}
			b, err := r.ReadByte()
			if err != nil {
				return invalid(err)
			}
			if i < 4 {
				offset |= int64(b) << (8 * i)
			} else {
				count |= int64(b) << (8 * (i - 4))
			}
		}
		if count == 0 {
			count = 0x10000
		}
		if offset > base.Size || count > base.Size-offset || count > target-written {
			return ErrInvalid
		}
		if _, err := io.CopyN(out, contextReader{ctx, io.NewSectionReader(baseFile, offset, count)}, count); err != nil {
			return err
		}
		written += count
	}
	if written != target {
		return ErrInvalid
	}
	if err := f.Close(); err != nil {
		return err
	}
	f = nil
	if err := delta.Close(); err != nil {
		return err
	}
	delta = nil
	if err := os.Remove(entry.path); err != nil {
		return err
	}
	w.diskBytes += target - entry.Size
	w.decodedBytes += target
	entry.path = path
	entry.Type, entry.Size, entry.depth = base.Type, target, base.depth+1
	entry.ID, entry.SHA256 = hex.EncodeToString(gitHash.Sum(nil)), hex.EncodeToString(strongHash.Sum(nil))
	// The original location encoded a delta, so it is not suitable for independent
	// S3 range reads. Write produces canonical locations for all objects.
	entry.Offset, entry.Length, entry.CRC32 = 0, 0, 0
	if prior := w.objects[entry.ID]; prior != nil && (prior.Type != entry.Type || prior.Size != entry.Size || prior.SHA256 != entry.SHA256) {
		return ErrInvalid
	}
	w.objects[entry.ID] = entry
	return nil
}

func deltaSize(r io.ByteReader) (int64, error) {
	var size uint64
	for shift := uint(0); shift < 63; shift += 7 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, invalid(err)
		}
		value := uint64(b & 127)
		if value > uint64(math.MaxInt64)>>shift {
			return 0, ErrInvalid
		}
		size |= value << shift
		if b&128 == 0 {
			return int64(size), nil // #nosec G115 -- Each shifted value is bounded against MaxInt64 above.
		}
	}
	return 0, ErrInvalid
}
