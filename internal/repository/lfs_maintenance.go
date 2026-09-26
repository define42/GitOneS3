package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

func (s *Store) verifyLFSReferences(ctx context.Context, repositoryID string, pointers map[string]int64) (int64, error) {
	var total int64
	buffer := make([]byte, 32<<10)
	for oid, size := range pointers {
		record, err := s.readLFSRecord(ctx, repositoryID, oid)
		if err != nil {
			return total, fmt.Errorf("verify LFS object %s: %w", oid, errors.Join(ErrCorrupt, err))
		}
		if record.Object.Size != size || size == math.MaxInt64 || size > math.MaxInt64-total {
			return total, ErrCorrupt
		}
		body, info, err := s.objects.Get(ctx, "repos/"+repositoryID+"/"+record.Key)
		if err != nil {
			return total, errors.Join(ErrCorrupt, err)
		}
		digest := sha256.New()
		n, readErr := io.CopyBuffer(digest, &contextReader{ctx: ctx, reader: io.LimitReader(body, size+1)}, buffer)
		if err := errors.Join(readErr, body.Close()); err != nil {
			return total, err
		}
		if n != size || info.Size != size || info.Version != record.Version || hex.EncodeToString(digest.Sum(nil)) != oid {
			return total, fmt.Errorf("LFS object %s failed verification: %w", oid, ErrCorrupt)
		}
		total += n
	}
	return total, nil
}

func (s *Store) markSnapshotLFS(ctx context.Context, snap snapshot, marked map[string]bool) (err error) {
	var pointers map[string]int64
	if snap.manifest.LFS != nil {
		pointers = snap.manifest.LFS.Objects
	} else {
		// Old generations have no index. Derive roots before collecting LFS;
		// a missing index must never mean that a generation has no pointers.
		reader, err := s.openGitSnapshot(snap)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, reader.Close()) }()
		pointers, err = collectLFSPointers(ctx, snap.manifest.Objects, reader.object)
		if err != nil {
			return err
		}
	}
	for oid := range pointers {
		record, err := s.readLFSRecord(ctx, snap.metadata.ID, oid)
		if err != nil {
			return err
		}
		marked["lfs/verified/"+oid+".json"] = true
		marked[record.Key] = true
	}
	return nil
}

func collectibleLFSArtifact(key string) bool {
	if validLFSDataKey(key) {
		return true
	}
	if suffix, ok := strings.CutPrefix(key, "lfs/verified/"); ok {
		oid := strings.TrimSuffix(suffix, ".json")
		return ValidLFSOID(oid) && key == "lfs/verified/"+oid+".json"
	}
	id := strings.TrimSuffix(strings.TrimPrefix(key, "lfs/uploads/"), ".json")
	return idPattern.MatchString(id) && key == "lfs/uploads/"+id+".json"
}

// prepareLFSGC protects live uploads and every recent verified record together
// with its payload. Expired reservations can be removed because finalization
// must still find their exact version while holding the repository lock.
func (s *Store) prepareLFSGC(ctx context.Context, repositoryID string, artifacts []storage.ObjectInfo, marked map[string]bool, cutoff time.Time) ([]lfsReservation, error) {
	prefix := "repos/" + repositoryID + "/"
	var expired []lfsReservation
	for _, info := range artifacts {
		key := strings.TrimPrefix(info.Key, prefix)
		switch {
		case strings.HasPrefix(key, "lfs/verified/"):
			if !collectibleLFSArtifact(key) {
				return nil, ErrCorrupt
			}
			oid := strings.TrimSuffix(strings.TrimPrefix(key, "lfs/verified/"), ".json")
			record, err := s.readLFSRecord(ctx, repositoryID, oid)
			if err != nil {
				return nil, err
			}
			if marked[key] || info.LastModified.IsZero() || !info.LastModified.Before(cutoff) {
				marked[key] = true
				marked[record.Key] = true
			}
		case strings.HasPrefix(key, "lfs/uploads/"):
			reservation, err := s.readLFSReservation(ctx, repositoryID, info)
			if err != nil {
				return nil, err
			}
			if time.Now().Before(reservation.ExpiresAt) || info.LastModified.IsZero() || !info.LastModified.Before(cutoff) {
				marked[key] = true
				marked[reservation.Key] = true
			} else {
				expired = append(expired, reservation)
			}
		}
	}
	return expired, nil
}

func (s *Store) abortExpiredLFS(ctx context.Context, repositoryID string, reservations []lfsReservation) error {
	for _, reservation := range reservations {
		if reservation.UploadID == "" {
			continue
		}
		multipart, ok := s.objects.(storage.MultipartStore)
		if !ok {
			return storage.ErrMultipartUnsupported
		}
		err := multipart.AbortMultipart(ctx, "repos/"+repositoryID+"/"+reservation.Key, reservation.UploadID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return nil
}

func lfsDeletionPriority(key string) int {
	if strings.Contains(key, "/lfs/uploads/") {
		return 0
	}
	if strings.Contains(key, "/lfs/verified/") {
		return 1
	}
	return 2
}
