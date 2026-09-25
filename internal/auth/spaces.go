package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

const spaceIndexRoot = "auth/space-index/v1/"
const spaceIndexReadyKey = spaceIndexRoot + "ready.json"
const spacePageSize = 100
const spaceCursorLabel = "gitone-space-page-v1"

// ErrSpaceIndexNotReady prevents an unmigrated shard from silently omitting groups.
var ErrSpaceIndexNotReady = errors.New("space index is not ready; deploy all writers in scan mode and run gitone backfill-space-index before enabling indexed mode")

func spaceCandidatePrefix(id string) string {
	digest := sha256.Sum256([]byte(id))
	return spaceIndexRoot + "users/" + hex.EncodeToString(digest[:]) + "/"
}

// ensureSpaceCandidate only adds lookup hints; current membership is always read
// from the group. Never delete hints on failed CAS, removal, or cancellation:
// doing so could erase a concurrent invitation using the same key.
func (s *Service) ensureSpaceCandidate(ctx context.Context, name, id string) error {
	if err := s.validateNamespaceOwner(name); err != nil {
		return err
	}
	if !validUserID(id) {
		return errInvalidMember
	}
	key := spaceCandidatePrefix(id) + name + ".json"
	const data = `{"schemaVersion":1}`
	for range 8 {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := s.store.Put(ctx, key, strings.NewReader(data), int64(len(data)), storage.PutOptions{IfNoneMatch: true})
		if err == nil || errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrPreconditionFailed) {
			return nil
		}
		if !errors.Is(err, storage.ErrConditionalConflict) {
			return fmt.Errorf("write space candidate: %w", err)
		}
		if _, err := s.store.Head(ctx, key); err == nil {
			return nil
		} else if !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("check space candidate: %w", err)
		}
	}
	return fmt.Errorf("write space candidate: %w", storage.ErrConditionalConflict)
}

func spaceIndexService(store storage.ObjectStore, router *shard.Router, local shard.ShardID) (*Service, error) {
	if store == nil || router == nil || uint32(local) >= router.ShardCount() {
		return nil, errors.New("space index requires shard-local storage and a valid router")
	}
	return &Service{store: store, router: router, local: local}, nil
}

func (s *Service) spaceIndexState() []byte {
	return fmt.Appendf(nil, `{"schemaVersion":1,"shard":%d,"shardCount":%d}`, s.local, s.ShardCount())
}

func (s *Service) spaceIndexReady(ctx context.Context) (bool, error) {
	data, version, err := s.readObject(ctx, spaceIndexReadyKey)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if version == "" || !bytes.Equal(data, s.spaceIndexState()) {
		return false, errors.New("invalid space index readiness record")
	}
	return true, nil
}

func (s *Service) publishSpaceIndexReady(ctx context.Context) error {
	data := s.spaceIndexState()
	_, err := s.store.Put(ctx, spaceIndexReadyKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfNoneMatch: true})
	if !isNamespaceConflict(err) {
		return err
	}
	ready, err := s.spaceIndexReady(ctx)
	if err != nil {
		return err
	}
	if !ready {
		return ErrSpaceIndexNotReady
	}
	return nil
}

// InitializeSpaceIndex permits automatic initialization only for an empty shard.
// Existing namespaces require the explicit backfill after all old writers have
// been replaced. A readiness marker is not evidence that old writers are gone.
func InitializeSpaceIndex(ctx context.Context, store storage.ObjectStore, router *shard.Router, local shard.ShardID) error {
	s, err := spaceIndexService(store, router, local)
	if err != nil {
		return err
	}
	ready, err := s.spaceIndexReady(ctx)
	if err != nil || ready {
		return err
	}
	page, err := store.ListPage(ctx, "auth/users/", "", 1)
	if err != nil {
		return err
	}
	if len(page.Objects) != 0 || page.NextAfter != "" {
		return ErrSpaceIndexNotReady
	}
	return s.publishSpaceIndexReady(ctx)
}

// BackfillSpaceIndex rebuilds discovery hints with bounded storage pages. It is
// idempotent and never changes memberships. Operators must first deploy index-
// aware writers everywhere in scan mode; concurrent new writers add hints before
// publishing grants, including keys that fall behind the migration's cursor.
func BackfillSpaceIndex(ctx context.Context, store storage.ObjectStore, router *shard.Router, local shard.ShardID) error {
	s, err := spaceIndexService(store, router, local)
	if err != nil {
		return err
	}
	// A failed rebuild must not leave a previous readiness assertion in place.
	info, err := store.Head(ctx, spaceIndexReadyKey)
	if err == nil {
		if info.Version == "" {
			return errors.New("space index readiness record has no version")
		}
		if err := store.Delete(ctx, spaceIndexReadyKey, info.Version); err != nil {
			return fmt.Errorf("invalidate space index readiness: %w", err)
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := store.ListPage(ctx, "auth/users/", after, spacePageSize)
		if err != nil {
			return fmt.Errorf("list namespaces for space index: %w", err)
		}
		for _, object := range page.Objects {
			name, err := s.spaceName(object.Key, "auth/users/")
			if err != nil {
				return err
			}
			record, _, err := s.loadNamespace(ctx, name)
			if err != nil {
				return fmt.Errorf("backfill namespace %q: %w", name, err)
			}
			if record.Type != groupNamespace {
				continue
			}
			for id := range record.Members {
				if err := s.ensureSpaceCandidate(ctx, name, id); err != nil {
					return err
				}
			}
			for id := range record.Invitations {
				if err := s.ensureSpaceCandidate(ctx, name, id); err != nil {
					return err
				}
			}
		}
		if page.NextAfter == "" {
			break
		}
		if page.NextAfter <= after {
			return errors.New("space index backfill cursor did not advance")
		}
		after = page.NextAfter
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.publishSpaceIndexReady(ctx)
}

func (s *Service) spaceName(key, prefix string) (string, error) {
	name, ok := strings.CutPrefix(key, prefix)
	if !ok || !strings.HasSuffix(name, ".json") {
		return "", errors.New("invalid space discovery key")
	}
	name = strings.TrimSuffix(name, ".json")
	if err := s.validateNamespaceOwner(name); err != nil {
		return "", err
	}
	return name, nil
}

func (s *Service) spaceListPrefix(id string) string {
	if s.spaceDiscoveryMode == "scan" {
		return "auth/users/"
	}
	return spaceCandidatePrefix(id)
}

func (s *Service) listSpaces(ctx context.Context, id, after string, limit int) ([]spaceView, string, error) {
	if s.spaceDiscoveryMode != "scan" {
		ready, err := s.spaceIndexReady(ctx)
		if err != nil {
			return nil, "", err
		}
		if !ready {
			return nil, "", ErrSpaceIndexNotReady
		}
	}
	prefix := s.spaceListPrefix(id)
	page, err := s.store.ListPage(ctx, prefix, after, limit)
	if err != nil {
		return nil, "", err
	}
	spaces := make([]spaceView, 0, len(page.Objects))
	for _, object := range page.Objects {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		name, err := s.spaceName(object.Key, prefix)
		if err != nil {
			return nil, "", err
		}
		record, _, err := s.loadNamespace(ctx, name)
		if errors.Is(err, storage.ErrNotFound) {
			continue // A candidate can precede a failed or unfinished group claim.
		}
		if err != nil {
			return nil, "", err // Do not silently hide storage failures as revocations.
		}
		if record.Type != groupNamespace {
			continue
		}
		role, invited := record.Members[id], false
		if role == "" {
			role, invited = record.Invitations[id], true
		}
		if role != "" {
			spaces = append(spaces, spaceView{Name: name, Type: groupNamespace, Role: role, Invited: invited})
		}
	}
	return spaces, page.NextAfter, nil
}

type spaceCursor struct {
	UserID     string
	Shard      shard.ShardID
	ShardCount uint32
	Mode       string
	Origin     string
	After      string
}

func (s *Service) decodeSpaceCursor(encoded, id string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	var cursor spaceCursor
	if err := s.sessionCodec.Decode(spaceCursorLabel, encoded, &cursor); err != nil {
		return "", err
	}
	if cursor.UserID != id || cursor.Shard != s.local || cursor.ShardCount != s.ShardCount() ||
		cursor.Mode != s.spaceDiscoveryMode || cursor.Origin != s.origin {
		return "", errors.New("space cursor belongs to a different discovery scope")
	}
	if _, err := s.spaceName(cursor.After, s.spaceListPrefix(id)); err != nil {
		return "", err
	}
	return cursor.After, nil
}

func (s *Service) encodeSpaceCursor(after, id string) (string, error) {
	return s.sessionCodec.Encode(spaceCursorLabel, spaceCursor{
		UserID: id, Shard: s.local, ShardCount: s.ShardCount(),
		Mode: s.spaceDiscoveryMode, Origin: s.origin, After: after,
	})
}
