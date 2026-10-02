package repository

import (
	"context"
	"errors"
	"io"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

// Completed history is represented by two counters. Outstanding reservations
// occupy the bounded map; simultaneous uploads of one OID are charged separately.
const maxLFSReservations = 1024

type lfsQuota struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Bytes         int64                `json:"bytes"`
	Objects       int                  `json:"objects"`
	Reservations  map[string]LFSObject `json:"reservations"`
	Dirty         bool                 `json:"dirty"`
	version       storage.Version
}

func lfsQuotaKey(repositoryID string) string {
	return "repos/" + repositoryID + "/lfs/quota.json"
}

func (s *Store) readLFSQuota(ctx context.Context, repositoryID string) (lfsQuota, error) {
	var quota lfsQuota
	body, info, err := s.objects.Get(ctx, lfsQuotaKey(repositoryID))
	if err != nil {
		return quota, err
	}
	const limit = 256 << 10
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err = errors.Join(err, body.Close()); err != nil {
		return quota, err
	}
	if len(data) > limit {
		return quota, ErrLimit
	}
	if info.Size != int64(len(data)) {
		return quota, ErrCorrupt
	}
	if err := decodeJSON(data, &quota); err != nil {
		return quota, err
	}
	if info.Version == "" || quota.SchemaVersion != 1 || quota.Bytes < 0 || quota.Objects < 0 ||
		quota.Objects > maxLFSRecords || quota.Reservations == nil || len(quota.Reservations) > maxLFSReservations {
		return quota, ErrCorrupt
	}
	var reserved int64
	for token, object := range quota.Reservations {
		if !idPattern.MatchString(token) || !ValidLFSOID(object.OID) || object.Size < 0 || object.Size > math.MaxInt64-reserved {
			return quota, ErrCorrupt
		}
		reserved += object.Size
	}
	if quota.Objects < len(quota.Reservations) || quota.Bytes < reserved {
		return quota, ErrCorrupt
	}
	quota.version = info.Version
	return quota, nil
}

// All ledger operations require the durable repository lock. The dirty write
// must succeed before changing artifacts. A failed or ambiguous write never
// permits a subsequent artifact mutation, and a dirty ledger is never used to
// admit an upload. This also makes cancellation and partial GC recoverable.
func (s *Store) saveLFSQuota(ctx context.Context, repositoryID string, quota *lfsQuota, dirty bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	quota.Dirty = dirty
	options := storage.PutOptions{IfMatch: quota.version}
	if quota.version == "" {
		options = storage.PutOptions{IfNoneMatch: true}
	}
	info, err := s.putLFSJSON(ctx, lfsQuotaKey(repositoryID), quota, options)
	if err != nil {
		return err
	}
	if info.Version == "" {
		return ErrCorrupt
	}
	quota.version = info.Version
	return nil
}

func (s *Store) loadLFSQuota(ctx context.Context, repositoryID string) (lfsQuota, error) {
	quota, err := s.readLFSQuota(ctx, repositoryID)
	if errors.Is(err, storage.ErrNotFound) {
		return s.rebuildLFSQuota(ctx, repositoryID, "")
	}
	if err != nil || !quota.Dirty {
		return quota, err
	}
	return s.rebuildLFSQuota(ctx, repositoryID, quota.version)
}

func (quota lfsQuota) check(object LFSObject, limits LFSLimits) error {
	if len(quota.Reservations) >= maxLFSReservations || quota.Objects >= maxLFSRecords ||
		quota.Bytes > limits.MaxRepositoryBytes || object.Size > limits.MaxRepositoryBytes-quota.Bytes {
		return errors.Join(ErrLimit, ErrLFSQuota)
	}
	return nil
}

// The caller still holds the repository lock and has not returned the upload
// ID to a streaming caller. Both possible outcomes of its final ledger CAS are
// known. Recover the actual version, fence cleanup with a dirty CAS, then undo
// this reservation without rescanning completed history. Unexpected state or
// any failed cleanup stays charged or dirty for later reconciliation.
func (s *Store) cleanupLFSInitialization(ctx context.Context, repositoryID, key string, version storage.Version, reservation lfsReservation, proposed lfsQuota, multipart storage.MultipartStore) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	token := strings.TrimPrefix(reservation.Key, "lfs/objects/")
	previous := proposed
	previous.Bytes -= reservation.Object.Size
	previous.Objects--
	previous.Reservations = maps.Clone(proposed.Reservations)
	delete(previous.Reservations, token)
	current, err := s.readLFSQuota(cleanupCtx, repositoryID)
	if err != nil {
		return err
	}
	matches := func(expected lfsQuota) bool {
		return current.Bytes == expected.Bytes && current.Objects == expected.Objects &&
			maps.Equal(current.Reservations, expected.Reservations)
	}
	if !matches(previous) && !matches(proposed) {
		return ErrConflict
	}
	if err := s.saveLFSQuota(cleanupCtx, repositoryID, &current, true); err != nil {
		return err
	}
	err = multipart.AbortMultipart(cleanupCtx, "repos/"+repositoryID+"/"+reservation.Key, reservation.UploadID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	// Completion is impossible for a never-streamed upload after a confirmed
	// abort. Fail closed if the provider nevertheless exposes a payload.
	if _, err := s.objects.Head(cleanupCtx, "repos/"+repositoryID+"/"+reservation.Key); !errors.Is(err, storage.ErrNotFound) {
		return errors.Join(ErrCorrupt, err)
	}
	if err := s.objects.Delete(cleanupCtx, key, version); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	previous.version = current.version
	return s.saveLFSQuota(cleanupCtx, repositoryID, &previous, false)
}

// Reconciliation is needed once for legacy repositories and after interrupted
// mutations. It counts physical orphans, validates verified records, and counts
// reservations only where their payload has not already completed.
func (s *Store) rebuildLFSQuota(ctx context.Context, repositoryID string, version storage.Version) (lfsQuota, error) {
	quota := lfsQuota{SchemaVersion: 1, Reservations: map[string]LFSObject{}, version: version}
	artifacts, err := s.lfsArtifacts(ctx, repositoryID)
	if err != nil {
		return quota, err
	}
	physical := map[string]storage.ObjectInfo{}
	prefix := "repos/" + repositoryID + "/"
	add := func(size int64) error {
		if quota.Objects >= maxLFSRecords || size > math.MaxInt64-quota.Bytes {
			return errors.Join(ErrLimit, ErrLFSQuota)
		}
		quota.Objects++
		quota.Bytes += size
		return nil
	}
	for _, info := range artifacts {
		if err := ctx.Err(); err != nil {
			return quota, err
		}
		key := strings.TrimPrefix(info.Key, prefix)
		if !strings.HasPrefix(key, "lfs/objects/") {
			continue
		}
		if !validLFSDataKey(key) || info.Size < 0 || info.Version == "" {
			return quota, ErrCorrupt
		}
		if err := add(info.Size); err != nil {
			return quota, err
		}
		physical[key] = info
	}
	for _, info := range artifacts {
		if err := ctx.Err(); err != nil {
			return quota, err
		}
		key := strings.TrimPrefix(info.Key, prefix)
		switch {
		case strings.HasPrefix(key, "lfs/verified/"):
			oid := strings.TrimSuffix(strings.TrimPrefix(key, "lfs/verified/"), ".json")
			if !ValidLFSOID(oid) || key != "lfs/verified/"+oid+".json" {
				return quota, ErrCorrupt
			}
			record, err := s.readLFSRecord(ctx, repositoryID, oid)
			if err != nil {
				return quota, err
			}
			payload, ok := physical[record.Key]
			if !ok || payload.Size != record.Object.Size || payload.Version != record.Version {
				return quota, ErrCorrupt
			}
		case strings.HasPrefix(key, "lfs/uploads/"):
			reservation, err := s.readLFSReservation(ctx, repositoryID, info)
			if err != nil {
				return quota, err
			}
			if len(quota.Reservations) >= maxLFSReservations {
				return quota, errors.Join(ErrLimit, ErrLFSQuota)
			}
			token := strings.TrimPrefix(reservation.Key, "lfs/objects/")
			quota.Reservations[token] = reservation.Object
			if payload, ok := physical[reservation.Key]; ok {
				if payload.Size != reservation.Object.Size {
					return quota, ErrCorrupt
				}
			} else if err := add(reservation.Object.Size); err != nil {
				return quota, err
			}
		}
	}
	err = s.saveLFSQuota(ctx, repositoryID, &quota, false)
	return quota, err
}

// GC can start with a dirty or missing ledger: it does not need a successful
// migration before collecting expired reservations that exceed the new bound.
func (s *Store) invalidateLFSQuota(ctx context.Context, repositoryID string) (lfsQuota, error) {
	quota, err := s.readLFSQuota(ctx, repositoryID)
	if errors.Is(err, storage.ErrNotFound) {
		quota = lfsQuota{SchemaVersion: 1, Reservations: map[string]LFSObject{}}
	} else if err != nil {
		return quota, err
	}
	err = s.saveLFSQuota(ctx, repositoryID, &quota, true)
	return quota, err
}
