package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/define42/GitOneS3/internal/cache"
	"github.com/define42/GitOneS3/internal/gitpack"
)

// Option configures a repository store. Without options all reads reach storage.
type Option func(*Store)

// WithCache shares verified immutable content across requests. The cache must be
// scoped to the same storage endpoint and bucket for every Store using it.
// Namespace authority, repository identity and mutable state are never cached.
func WithCache(shared *cache.Cache) Option {
	return func(s *Store) { s.cache = shared }
}

// SharedCache is the serving cache, or nil for uncached/maintenance stores.
func (s *Store) SharedCache() *cache.Cache { return s.cache }

func (s *Store) withoutCache() *Store {
	copy := *s
	copy.cache = nil
	return &copy
}

// CacheKey identifies the exact immutable inputs for an operation. Cache itself
// supplies storage isolation. Callers append their format and selection digest.
func (r *GitReader) CacheKey(kind string) string {
	snap := r.base.original
	return kind + ":" + snap.metadata.ID + ":" + snap.state.RefsSnapshot + ":" + snap.state.PackManifest
}

// PackSizeHint sizes a cache reservation from the selected decoded objects. The
// allowance covers compression framing/growth conservatively for ordinary packs
// without evicting a maximum-size pack's worth of data for a tiny clone. This is
// only a cache hint: the bounded writer may reject it and callers must fall back
// to normal pack generation. The protocol's MaxPackBytes limit still applies.
func (r *GitReader) PackSizeHint(ids []string) (int64, error) {
	if r.closed || len(ids) > MaxGitObjects {
		return 0, ErrInvalid
	}
	size := int64(32)
	for _, id := range ids {
		info, ok := r.base.available[id]
		if !ok || !validObjectInfo(id, info) {
			return 0, ErrInvalid
		}
		size = min(int64(MaxPackBytes), size+2*info.Size+1024)
	}
	return size, nil
}

func snapshotCacheKey(repositoryID, relative string) string {
	return "snapshot:" + repositoryID + ":" + relative
}

func packCacheKey(repositoryID, relative string) string {
	return "pack:" + repositoryID + ":" + relative
}

// snapshotData caches only digest-verified immutable bytes. JSON and semantic
// validation still run before decoded values enter the memory cache.
func (s *Store) snapshotData(ctx context.Context, repositoryID, relative string, limit int64) ([]byte, error) {
	load := func(ctx context.Context) ([]byte, error) {
		data, err := s.read(ctx, "repos/"+repositoryID+"/"+relative, limit)
		if err != nil {
			return nil, fmt.Errorf("read repository snapshot: %w", errors.Join(ErrCorrupt, err))
		}
		digest := sha256.Sum256(data)
		if !strings.HasSuffix(relative, "-"+hex.EncodeToString(digest[:])+".json") {
			return nil, ErrCorrupt
		}
		return data, nil
	}
	if s.cache == nil {
		return load(ctx)
	}
	file, err := s.cache.LoadFile(ctx, snapshotCacheKey(repositoryID, relative), limit, func(ctx context.Context, out io.Writer) error {
		data, err := load(ctx)
		if err != nil {
			return err
		}
		_, err = out.Write(data)
		return err
	})
	if errors.Is(err, cache.ErrCapacity) {
		return load(ctx)
	}
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: file}, limit+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) > limit || !strings.HasSuffix(relative, "-"+hex.EncodeToString(digest[:])+".json") {
		return nil, ErrCorrupt
	}
	return data, nil
}

func validateManifest(ctx context.Context, manifest objectManifest) error {
	if (manifest.SchemaVersion != 1 && manifest.SchemaVersion != 2) || manifest.ObjectFormat != "sha1" ||
		manifest.Objects == nil || len(manifest.Objects) > maxObjects || !validLFSIndex(manifest.LFS) {
		return ErrCorrupt
	}
	for id, info := range manifest.Objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validObjectInfo(id, info) || (manifest.SchemaVersion == 1 && info.PackKey != "") {
			return ErrCorrupt
		}
	}
	return nil
}

// Account for maps, strings and entries as well as payload bytes. These are
// conservative retained-size estimates, not a process/RSS or decoder limit.
func manifestMemoryBytes(manifest objectManifest) int64 {
	size := int64(256)
	for id, info := range manifest.Objects {
		size += int64(256 + len(id) + len(info.Type) + len(info.SHA256) + len(info.PackKey))
	}
	if manifest.LFS != nil {
		size += int64(128 + len(manifest.LFS.Objects)*128)
	}
	return size
}

func (s *Store) cachedObject(ctx context.Context, snap snapshot, id, kind string) ([]byte, error) {
	info, ok := snap.manifest.Objects[id]
	if !ok || !validObjectInfo(id, info) || info.Type != kind {
		return nil, ErrCorrupt
	}
	load := func(ctx context.Context) ([]byte, error) { return s.readObject(ctx, snap, id, kind) }
	// Large blob payloads live on disk. Metadata and small browser previews are
	// useful in RAM; a single object cannot consume the complete memory budget.
	if s.cache == nil || info.Size > 1<<20 {
		return load(ctx)
	}
	// A new generation may carry different storage/verification metadata for
	// the same Git ID. Reusing bytes must not conceal an invalid new entry.
	key := fmt.Sprintf("object:%s:%s:%s:%s:%d:%s:%d:%d:%d", snap.metadata.ID, id, info.SHA256, kind,
		info.Size, info.PackKey, info.Offset, info.Length, info.CRC32)
	value, err := s.cache.LoadMemory(ctx, key, func(ctx context.Context) (any, int64, error) {
		data, err := load(ctx)
		return data, int64(len(data) + 256 + len(key)), err
	})
	if err != nil {
		return nil, err
	}
	// Never let mutable caller buffers become shared cache values.
	return slices.Clone(value.([]byte)), nil
}

func (s *Store) packedObject(ctx context.Context, snap snapshot, id string, info objectInfo) ([]byte, error) {
	decode := func(input io.Reader) ([]byte, error) {
		object, err := gitpack.DecodeEntry(ctx, input, packEntry(id, info), MaxGitObjectBytes)
		if err != nil {
			return nil, errors.Join(ErrCorrupt, err)
		}
		return object.Data, nil
	}
	load := func(ctx context.Context, out io.Writer) error {
		body, _, err := s.objects.GetRange(ctx, "repos/"+snap.metadata.ID+"/"+info.PackKey, info.Offset, info.Length)
		if err != nil {
			return errors.Join(ErrCorrupt, err)
		}
		_, readErr := gitpack.DecodeEntry(ctx, io.TeeReader(body, out), packEntry(id, info), MaxGitObjectBytes)
		if err := errors.Join(readErr, body.Close()); err != nil {
			return errors.Join(ErrCorrupt, err)
		}
		return nil
	}
	if s.cache != nil {
		// A full pack cached by another request can satisfy sparse browser reads.
		file, err := s.cache.OpenFile(ctx, packCacheKey(snap.metadata.ID, info.PackKey))
		if err == nil {
			data, readErr := decode(io.NewSectionReader(file, info.Offset, info.Length))
			return data, errors.Join(readErr, file.Close())
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := fmt.Sprintf("entry:%s:%s:%d:%d:%s", snap.metadata.ID, info.PackKey, info.Offset, info.Length, info.SHA256)
		file, err = s.cache.LoadFile(ctx, key, info.Length, load)
		if err == nil {
			data, readErr := decode(file)
			return data, errors.Join(readErr, file.Close())
		}
		if !errors.Is(err, cache.ErrCapacity) {
			return nil, err
		}
	}
	body, _, err := s.objects.GetRange(ctx, "repos/"+snap.metadata.ID+"/"+info.PackKey, info.Offset, info.Length)
	if err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	data, readErr := decode(body)
	return data, errors.Join(readErr, body.Close())
}

func (r *GitReader) loadSharedPack(ctx context.Context, relative string, selected int64) (bool, error) {
	repositoryID := r.base.original.metadata.ID
	key := packCacheKey(repositoryID, relative)
	file, err := r.store.cache.OpenFile(ctx, key)
	if err == nil {
		if file.Size() > MaxPackBytes-r.cacheBytes {
			return true, file.Close() // Sparse reads can still open it individually.
		}
		r.sharedPacks[relative] = file
		r.cacheBytes += file.Size()
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, cache.ErrCapacity) {
		return false, err
	}
	storageKey := "repos/" + repositoryID + "/" + relative
	size, ok := r.packSizes[relative]
	if !ok {
		info, err := r.store.objects.Head(ctx, storageKey)
		if err != nil {
			return false, errors.Join(ErrCorrupt, err)
		}
		if info.Size < 32 || info.Size > MaxPackBytes {
			return false, ErrCorrupt
		}
		size = info.Size
		r.packSizes[relative] = size
	}
	if selected < (size+1)/2 || size > MaxPackBytes-r.cacheBytes {
		return true, nil
	}
	file, err = r.store.cache.LoadFile(ctx, key, size, func(ctx context.Context, out io.Writer) error {
		body, info, err := r.store.objects.Get(ctx, storageKey)
		if err != nil {
			return err
		}
		digest := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(out, digest), &contextReader{ctx: ctx, reader: io.LimitReader(body, size+1)})
		if err := errors.Join(copyErr, body.Close()); err != nil {
			return err
		}
		if n != size || info.Size != size || relative != "packs/"+hex.EncodeToString(digest.Sum(nil))+".pack" {
			return ErrCorrupt
		}
		return nil
	})
	if errors.Is(err, cache.ErrCapacity) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r.sharedPacks[relative] = file
	r.cacheBytes += file.Size()
	return true, nil
}
