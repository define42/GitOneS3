package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/define42/GitOneS3/internal/gitpack"
	"github.com/define42/GitOneS3/internal/storage"
)

// PackLimits bounds payload memory, graph metadata, input, and temporary disk.
// A workspace contains decoded objects/deltas; pack caches and output each have
// an additional MaxPackBytes bound. Admission controls the number of workspaces.
func PackLimits() gitpack.Limits {
	return gitpack.Limits{MaxPackBytes: MaxPackBytes, MaxObjectBytes: MaxGitObjectBytes,
		MaxDecodedBytes: MaxGitBytes, MaxDiskBytes: 2 * MaxGitBytes,
		MaxObjects: MaxGitObjects, MaxDeltaDepth: 64}
}

func validObjectInfo(id string, info objectInfo) bool {
	if !objectIDPattern.MatchString(id) || !digestPattern.MatchString(info.SHA256) ||
		info.Size < 0 || info.Size > MaxGitObjectBytes ||
		(info.Type != "blob" && info.Type != "tree" && info.Type != "commit" && info.Type != "tag") {
		return false
	}
	if info.PackKey == "" {
		return info.Size <= maxObjectBytes && info.Offset == 0 && info.Length == 0 && info.CRC32 == 0
	}
	digest := strings.TrimSuffix(strings.TrimPrefix(info.PackKey, "packs/"), ".pack")
	return digestPattern.MatchString(digest) && info.PackKey == "packs/"+digest+".pack" &&
		info.Offset >= 12 && info.Length > 0 && info.Length <= MaxGitObjectBytes+(1<<20) &&
		info.Offset <= MaxPackBytes-20-info.Length
}

func packEntry(id string, info objectInfo) gitpack.Entry {
	return gitpack.Entry{ID: id, Type: info.Type, Size: info.Size, SHA256: info.SHA256,
		Offset: info.Offset, Length: info.Length, CRC32: info.CRC32}
}

// GitReader pins one published manifest and lazily caches immutable packs on
// temporary disk. Payload memory is bounded by one decoded object, regardless
// of repository size. It is local workspace only and is removed by Close.
type GitReader struct {
	store      *Store
	base       *GitSnapshot
	dir        string
	cached     map[string]string
	cacheBytes int64
	closed     bool
}

func (s *Store) OpenGit(_ context.Context, base *GitSnapshot) (*GitReader, error) {
	if base == nil || !idPattern.MatchString(base.original.metadata.ID) || base.original.version == "" {
		return nil, ErrInvalid
	}
	reader, err := s.openGitSnapshot(base.original)
	if err != nil {
		return nil, err
	}
	reader.base = base
	return reader, nil
}

func (s *Store) openGitSnapshot(snap snapshot) (*GitReader, error) {
	if !idPattern.MatchString(snap.metadata.ID) {
		return nil, ErrInvalid
	}
	dir, err := os.MkdirTemp("", "gitone-read-*")
	if err != nil {
		return nil, fmt.Errorf("create Git workspace: %w", err)
	}
	return &GitReader{store: s, base: &GitSnapshot{DefaultBranch: snap.metadata.DefaultBranch, References: snap.refs.Refs, original: snap}, dir: dir, cached: map[string]string{}}, nil
}

func (r *GitReader) Close() error {
	r.closed = true
	return os.RemoveAll(r.dir)
}

func (r *GitReader) Get(ctx context.Context, id string) (gitpack.Object, error) {
	if err := ctx.Err(); err != nil {
		return gitpack.Object{}, err
	}
	if r.closed {
		return gitpack.Object{}, errors.New("git workspace closed")
	}
	info, ok := r.base.original.manifest.Objects[id]
	if !ok {
		return gitpack.Object{}, gitpack.ErrNotFound
	}
	if info.PackKey == "" {
		data, err := r.store.object(ctx, r.base.original, id, info.Type)
		return gitpack.Object{Type: info.Type, Data: data}, err
	}
	if !validObjectInfo(id, info) {
		return gitpack.Object{}, ErrCorrupt
	}
	fileName, err := r.cachePack(ctx, info.PackKey)
	if err != nil {
		return gitpack.Object{}, err
	}
	if fileName == "" {
		// A repository may reference small slices of many old packs. Avoid
		// unbounded disk use: uncached packs use validated S3 range reads.
		data, err := r.store.object(ctx, r.base.original, id, info.Type)
		return gitpack.Object{Type: info.Type, Data: data}, err
	}
	file, err := os.Open(fileName) // #nosec G304 -- Name is generated in our private temporary directory.
	if err != nil {
		return gitpack.Object{}, err
	}
	object, readErr := gitpack.DecodeEntry(ctx, io.NewSectionReader(file, info.Offset, info.Length), packEntry(id, info), MaxGitObjectBytes)
	if err := errors.Join(readErr, file.Close()); err != nil {
		return gitpack.Object{}, errors.Join(ErrCorrupt, err)
	}
	return object, nil
}

func (r *GitReader) cachePack(ctx context.Context, relative string) (string, error) {
	if name, ok := r.cached[relative]; ok {
		return name, nil
	}
	key := "repos/" + r.base.original.metadata.ID + "/" + relative
	info, err := r.store.objects.Head(ctx, key)
	if err != nil {
		return "", errors.Join(ErrCorrupt, err)
	}
	if info.Size < 32 || info.Size > MaxPackBytes {
		return "", ErrCorrupt
	}
	if info.Size > MaxPackBytes-r.cacheBytes {
		r.cached[relative] = ""
		return "", nil
	}
	body, got, err := r.store.objects.Get(ctx, key)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(r.dir, "pack-*")
	if err != nil {
		return "", errors.Join(err, body.Close())
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(file.Name())
		}
	}()
	digest := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, digest), &contextReader{ctx: ctx, reader: io.LimitReader(body, info.Size+1)})
	closeErr := errors.Join(body.Close(), file.Close())
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", err
	}
	if n != info.Size || got.Size != info.Size || relative != "packs/"+hex.EncodeToString(digest.Sum(nil))+".pack" {
		return "", ErrCorrupt
	}
	r.cacheBytes += n
	r.cached[relative] = file.Name()
	keep = true
	return file.Name(), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type gitGetter func(context.Context, string) (GitObject, error)

// walkObjects validates typed connectivity with bounded graph metadata. Blobs
// have no links; their body checksums are verified when decoded or transferred.
func walkObjects(ctx context.Context, refs map[string]string, infos map[string]objectInfo, get gitGetter) (map[string]objectInfo, error) {
	queue := make([]objectLink, 0, len(refs))
	scheduled := make(map[string]bool)
	enqueue := func(link objectLink) error {
		info, ok := infos[link.id]
		// Validate every edge, even when another edge has scheduled this ID.
		if !ok || (link.kind != "" && info.Type != link.kind) {
			return ErrInvalid
		}
		if !scheduled[link.id] {
			if len(scheduled) >= MaxGitObjects {
				return ErrLimit
			}
			scheduled[link.id] = true
			queue = append(queue, link)
		}
		return nil
	}
	for ref, id := range refs {
		kind := ""
		if strings.HasPrefix(ref, "refs/heads/") {
			kind = "commit"
		}
		if err := enqueue(objectLink{id: id, kind: kind}); err != nil {
			return nil, err
		}
	}
	result := make(map[string]objectInfo)
	var total int64
	for len(queue) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		link := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		info, ok := infos[link.id]
		if !ok || (link.kind != "" && info.Type != link.kind) {
			return nil, ErrInvalid
		}
		if _, ok := result[link.id]; ok {
			continue
		}
		if info.Size < 0 || info.Size > MaxGitObjectBytes || !objectIDPattern.MatchString(link.id) {
			return nil, ErrInvalid
		}
		total += info.Size
		if total > MaxGitBytes || len(result) >= MaxGitObjects {
			return nil, ErrLimit
		}
		result[link.id] = info
		if info.Type == "blob" {
			continue
		}
		object, err := get(ctx, link.id)
		if err != nil {
			return nil, err
		}
		if GitObjectID(object) != link.id || object.Type != info.Type || gitObjectInfo(object).SHA256 != info.SHA256 {
			return nil, ErrCorrupt
		}
		links, err := gitLinks(object)
		if err != nil {
			return nil, err
		}
		for _, link := range links {
			if err := enqueue(link); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func (r *GitReader) object(ctx context.Context, id string) (GitObject, error) {
	object, err := r.Get(ctx, id)
	return GitObject{Type: object.Type, Data: object.Data}, err
}

// Validate checks the published graph before accepting client have IDs. Orphan
// manifest entries must never be treated as client/server common history.
func (r *GitReader) Validate(ctx context.Context) error {
	objects, err := walkObjects(ctx, r.base.References, r.base.original.manifest.Objects, r.object)
	if err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	r.base.available = objects
	return nil
}

func (s *GitSnapshot) HasObject(id string) bool {
	if s.available != nil {
		_, ok := s.available[id]
		return ok
	}
	_, ok := s.Objects[id]
	return ok
}

func (r *GitReader) Reachable(ctx context.Context, refs map[string]string) ([]string, error) {
	objects, err := walkObjects(ctx, refs, r.base.available, r.object)
	if err != nil {
		return nil, err
	}
	ids := slices.Sorted(maps.Keys(objects))
	return ids, nil
}

// PublishPack publishes validated incoming objects without keeping their bodies
// in memory. Incoming thin packs have already been resolved in local workspace.
func (s *Store) PublishPack(ctx context.Context, base *GitSnapshot, updates []RefUpdate, incoming *gitpack.Workspace, authorize func(context.Context) error) error {
	infos := map[string]objectInfo{}
	if incoming != nil {
		for _, id := range incoming.IDs() {
			entry, ok := incoming.Info(id)
			if !ok {
				return ErrInvalid
			}
			infos[id] = objectInfo{Type: entry.Type, Size: entry.Size, SHA256: entry.SHA256}
		}
	}
	get := func(ctx context.Context, id string) (GitObject, error) {
		if incoming == nil {
			return GitObject{}, ErrNotFound
		}
		object, err := incoming.Get(ctx, id)
		return GitObject{Type: object.Type, Data: object.Data}, err
	}
	return s.publishObjects(ctx, base, updates, infos, get, false, authorize)
}

func applyRefUpdates(base *GitSnapshot, updates []RefUpdate, repack bool) (map[string]string, error) {
	if base == nil || !idPattern.MatchString(base.original.metadata.ID) || base.original.version == "" ||
		(!repack && len(updates) == 0) || len(updates) > 1000 {
		return nil, ErrInvalid
	}
	refs := maps.Clone(base.original.refs.Refs)
	seen := map[string]bool{}
	for _, update := range updates {
		if !ValidRef(update.Name) || seen[update.Name] || (update.Old != "" && !objectIDPattern.MatchString(update.Old)) ||
			(update.New != "" && !objectIDPattern.MatchString(update.New)) || update.Old == update.New {
			return nil, ErrInvalid
		}
		seen[update.Name] = true
		if refs[update.Name] != update.Old {
			return nil, ErrConflict
		}
		if update.New == "" {
			delete(refs, update.Name)
		} else {
			refs[update.Name] = update.New
		}
	}
	if len(refs) > 1000 {
		return nil, ErrLimit
	}
	for ref := range refs {
		parts := strings.Split(ref, "/")
		for i := 2; i < len(parts); i++ {
			if _, ok := refs[strings.Join(parts[:i], "/")]; ok {
				return nil, ErrInvalid
			}
		}
	}
	return refs, nil
}

func (s *Store) publishObjects(ctx context.Context, base *GitSnapshot, updates []RefUpdate, incoming map[string]objectInfo, getIncoming gitGetter, repack bool, authorize func(context.Context) error) (err error) {
	if authorize == nil {
		return ErrForbidden
	}
	refs, err := applyRefUpdates(base, updates, repack)
	if err != nil {
		return err
	}
	if base.original.state.Generation == ^uint64(0) {
		return ErrLimit
	}
	if len(incoming) > MaxGitObjects {
		return ErrLimit
	}
	reader, err := s.OpenGit(ctx, base)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	infos := maps.Clone(base.original.manifest.Objects)
	for id, info := range incoming {
		if !objectIDPattern.MatchString(id) || info.Size < 0 || info.Size > MaxGitObjectBytes {
			return ErrInvalid
		}
		if prior, ok := infos[id]; ok {
			if prior.Type != info.Type || prior.Size != info.Size || prior.SHA256 != info.SHA256 {
				return ErrCorrupt
			}
		} else {
			infos[id] = info
		}
	}
	get := func(ctx context.Context, id string) (GitObject, error) {
		if _, ok := incoming[id]; ok {
			return getIncoming(ctx, id)
		}
		return reader.object(ctx, id)
	}
	reachable, err := walkObjects(ctx, refs, infos, get)
	if err != nil {
		return err
	}
	lfsPointers, err := collectLFSPointers(ctx, reachable, get)
	if err != nil {
		return err
	}
	unlock, err := s.lockRepository(ctx, base.original.metadata.ID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	_, currentVersion, err := s.repositories.LoadState(ctx, base.original.metadata.ID)
	if err != nil {
		return err
	}
	if currentVersion != base.original.version {
		return ErrConflict
	}
	if err := s.validateLFSPointers(ctx, base.original.metadata.ID, lfsPointers); err != nil {
		return err
	}
	manifest := objectManifest{SchemaVersion: 2, ObjectFormat: "sha1", Objects: reachable, LFS: &lfsIndex{Version: 1, Objects: lfsPointers}}
	ids := []string{}
	for id := range reachable {
		if _, exists := base.original.manifest.Objects[id]; repack || !exists {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if len(ids) > 0 {
		if err := s.storePack(ctx, base.original.metadata.ID, ids, get, &manifest); err != nil {
			return err
		}
	}
	refsKey, err := s.snapshotKey(ctx, base.original.metadata.ID, "refs", refsSnapshot{SchemaVersion: 1, Refs: refs})
	if err != nil {
		return err
	}
	manifestKey, err := s.snapshotKey(ctx, base.original.metadata.ID, "manifest", manifest)
	if err != nil {
		return err
	}
	next := base.original.state
	next.Generation++
	next.RefsSnapshot, next.PackManifest = refsKey, manifestKey
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := authorize(ctx); err != nil {
		return fmt.Errorf("%w: %s", ErrForbidden, err.Error())
	}
	if err := s.repositories.CompareAndSwapState(ctx, base.original.metadata.ID, base.original.version, next); err != nil {
		if errors.Is(err, storage.ErrPreconditionFailed) || errors.Is(err, storage.ErrConditionalConflict) {
			return ErrConflict
		}
		return err
	}
	return nil
}

func (s *Store) snapshotKey(ctx context.Context, id, kind string, value any) (string, error) {
	key, err := s.putSnapshot(ctx, id, kind, value)
	if errors.Is(err, storage.ErrAlreadyExists) {
		return s.existingSnapshot(ctx, id, kind, value)
	}
	return key, err
}

func (s *Store) storePack(ctx context.Context, repositoryID string, ids []string, get gitGetter, manifest *objectManifest) (err error) {
	file, err := os.CreateTemp("", "gitone-pack-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close(), os.Remove(file.Name())) }()
	digest := sha256.New()
	entries, err := gitpack.Write(ctx, io.MultiWriter(file, digest), ids, func(ctx context.Context, id string) (gitpack.Object, error) {
		object, err := get(ctx, id)
		return gitpack.Object{Type: object.Type, Data: object.Data}, err
	}, PackLimits())
	if err != nil {
		return err
	}
	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	relative := "packs/" + hex.EncodeToString(digest.Sum(nil)) + ".pack"
	key := "repos/" + repositoryID + "/" + relative
	err = s.repositories.PutImmutable(ctx, key, file, size)
	if errors.Is(err, storage.ErrAlreadyExists) {
		// Content-addressed names still require verification after a collision.
		body, info, readErr := s.objects.Get(ctx, key)
		if readErr != nil {
			return readErr
		}
		actual := sha256.New()
		n, readErr := io.Copy(actual, &contextReader{ctx: ctx, reader: io.LimitReader(body, size+1)})
		closeErr := body.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if n != size || info.Size != size || !slices.Equal(actual.Sum(nil), digest.Sum(nil)) {
			return ErrCorrupt
		}
	} else if err != nil {
		return err
	}
	for _, entry := range entries {
		manifest.Objects[entry.ID] = objectInfo{Type: entry.Type, Size: entry.Size, SHA256: entry.SHA256,
			PackKey: relative, Offset: entry.Offset, Length: entry.Length, CRC32: entry.CRC32}
	}
	return nil
}

// Repack migrates loose objects and consolidates the current reachable graph
// into one immutable pack, then atomically publishes a new generation.
func (s *Store) Repack(ctx context.Context, namespace, name string) error {
	base, err := s.ReadGitReferences(ctx, namespace, name)
	if err != nil {
		return err
	}
	return s.publishObjects(ctx, base, nil, nil, nil, true, func(context.Context) error { return nil })
}
