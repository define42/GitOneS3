package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

const (
	// LFSPartBytes is the maximum upload payload buffered by one transfer.
	LFSPartBytes = 8 << 20
	// LFSReservationLifetime bounds abandoned reservations. Finalization checks
	// the reservation under the repository lock, fencing paused uploaders.
	LFSReservationLifetime = 24 * time.Hour
	maxLFSRecords          = 100_000
)

var (
	ErrLFSMissing      = errors.New("repository: LFS object is missing")
	ErrLFSHashMismatch = errors.New("repository: LFS object hash or size does not match")
	ErrLFSQuota        = errors.New("repository: LFS storage quota exceeded")
)

// LFSObject identifies verified content. Storage keys never leave the service.
type LFSObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// LFSLimits applies to physical content, including unpublished completed
// uploads, plus outstanding reservations that have no completed payload yet.
type LFSLimits struct {
	MaxObjectBytes     int64
	MaxRepositoryBytes int64
}

type lfsRecord struct {
	SchemaVersion int             `json:"schemaVersion"`
	Object        LFSObject       `json:"object"`
	Key           string          `json:"key"`
	Version       storage.Version `json:"version"`
}

type lfsReservation struct {
	SchemaVersion int       `json:"schemaVersion"`
	Object        LFSObject `json:"object"`
	Key           string    `json:"key"`
	UploadID      string    `json:"uploadId"`
	CreatedAt     time.Time `json:"createdAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// ValidLFSOID accepts the canonical SHA-256 identifiers used by Git LFS.
func ValidLFSOID(oid string) bool { return digestPattern.MatchString(oid) }

// LFSRepository validates the route and reads repository identity without
// loading the Git manifest or any Git object payloads.
func (s *Store) LFSRepository(ctx context.Context, namespace, name string) (Metadata, error) {
	return s.maintenanceMetadata(ctx, namespace, name)
}

func lfsRecordKey(repositoryID, oid string) string {
	return "repos/" + repositoryID + "/lfs/verified/" + oid + ".json"
}

func validLFSDataKey(key string) bool {
	token := strings.TrimPrefix(key, "lfs/objects/")
	return key == "lfs/objects/"+token && idPattern.MatchString(token)
}

func validLFSRecord(record lfsRecord, oid string) bool {
	return record.SchemaVersion == 1 && record.Object.OID == oid && ValidLFSOID(oid) &&
		record.Object.Size >= 0 && validLFSDataKey(record.Key) && record.Version != ""
}

func (s *Store) readLFSRecord(ctx context.Context, repositoryID, oid string) (lfsRecord, error) {
	var record lfsRecord
	data, err := s.read(ctx, lfsRecordKey(repositoryID, oid), 4096)
	if errors.Is(err, storage.ErrNotFound) {
		return record, errors.Join(ErrNotFound, ErrLFSMissing)
	}
	if err != nil {
		return record, err
	}
	if err := decodeJSON(data, &record); err != nil {
		return record, err
	}
	if !validLFSRecord(record, oid) {
		return record, ErrCorrupt
	}
	return record, nil
}

func (s *Store) checkLFSRecord(ctx context.Context, repositoryID, oid string) (lfsRecord, error) {
	record, err := s.readLFSRecord(ctx, repositoryID, oid)
	if err != nil {
		return record, err
	}
	info, err := s.objects.Head(ctx, "repos/"+repositoryID+"/"+record.Key)
	if err != nil {
		return record, errors.Join(ErrCorrupt, err)
	}
	if info.Size != record.Object.Size || info.Version != record.Version {
		return record, ErrCorrupt
	}
	return record, nil
}

// LFSStat finds verified content within an existing repository.
func (s *Store) LFSStat(ctx context.Context, namespace, name, oid string) (LFSObject, error) {
	if !ValidLFSOID(oid) {
		return LFSObject{}, ErrInvalid
	}
	metadata, err := s.maintenanceMetadata(ctx, namespace, name)
	if err != nil {
		return LFSObject{}, err
	}
	record, err := s.checkLFSRecord(ctx, metadata.ID, oid)
	return record.Object, err
}

// LFSOpen opens an immutable object or byte range without staging or buffering
// its contents. The caller owns Close and must propagate client cancellation.
// A length of -1 opens the complete object and requires offset zero.
func (s *Store) LFSOpen(ctx context.Context, namespace, name, oid string, offset, length int64) (io.ReadCloser, LFSObject, error) {
	if !ValidLFSOID(oid) || offset < 0 || length < -1 || (length == -1 && offset != 0) {
		return nil, LFSObject{}, ErrInvalid
	}
	metadata, err := s.maintenanceMetadata(ctx, namespace, name)
	if err != nil {
		return nil, LFSObject{}, err
	}
	record, err := s.readLFSRecord(ctx, metadata.ID, oid)
	if err != nil {
		return nil, LFSObject{}, err
	}
	if length != -1 && (length == 0 || offset >= record.Object.Size || length > record.Object.Size-offset) {
		return nil, record.Object, storage.ErrInvalidRange
	}
	key := "repos/" + metadata.ID + "/" + record.Key
	var body io.ReadCloser
	var info storage.ObjectInfo
	if length == -1 {
		body, info, err = s.objects.Get(ctx, key)
	} else {
		body, info, err = s.objects.GetRange(ctx, key, offset, length)
	}
	if err != nil {
		return nil, record.Object, errors.Join(ErrCorrupt, err)
	}
	if info.Size != record.Object.Size || info.Version != record.Version {
		return nil, record.Object, errors.Join(ErrCorrupt, body.Close())
	}
	return body, record.Object, nil
}

// LFSUpload streams through one reusable multipart buffer. Hash and exact size
// are checked before completion; verified metadata is published only after a
// final authorization check under the same lock used by Git publication and GC.
func (s *Store) LFSUpload(ctx context.Context, namespace, name, oid string, size int64, body io.Reader, limits LFSLimits, authorize func(context.Context) error) (object LFSObject, err error) {
	if !ValidLFSOID(oid) || size < 0 || body == nil || limits.MaxObjectBytes <= 0 || limits.MaxRepositoryBytes <= 0 {
		return object, ErrInvalid
	}
	if size > limits.MaxObjectBytes || size > 10_000*int64(LFSPartBytes) {
		return object, ErrLimit
	}
	if authorize == nil {
		return object, ErrForbidden
	}
	if err := authorize(ctx); err != nil {
		return object, errors.Join(ErrForbidden, err)
	}
	metadata, err := s.maintenanceMetadata(ctx, namespace, name)
	if err != nil {
		return object, err
	}
	multipart, ok := s.objects.(storage.MultipartStore)
	if !ok {
		return object, storage.ErrMultipartUnsupported
	}
	reservation, key, version, existing, err := s.reserveLFS(ctx, metadata.ID, LFSObject{OID: oid, Size: size}, limits, multipart)
	if err != nil {
		return object, err
	}
	if existing {
		if err := verifyLFSBody(ctx, body, oid, size); err != nil {
			return object, err
		}
		return s.finishExistingLFS(ctx, metadata.ID, reservation.Object, authorize)
	}
	complete := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if !complete {
			abortErr := multipart.AbortMultipart(cleanupCtx, "repos/"+metadata.ID+"/"+reservation.Key, reservation.UploadID)
			if abortErr != nil && !errors.Is(abortErr, storage.ErrNotFound) {
				err = errors.Join(err, abortErr)
				// Keep the reservation charged and recoverable until GC can
				// confirm that the provider discarded the partial upload.
				return
			}
		}
		err = errors.Join(err, s.releaseLFSReservation(cleanupCtx, metadata.ID, key, version))
	}()
	parts, err := streamLFSParts(ctx, multipart, "repos/"+metadata.ID+"/"+reservation.Key, reservation.UploadID, oid, size, body)
	if err != nil {
		return object, err
	}
	object, complete, err = s.finishLFS(ctx, metadata.ID, key, version, reservation, parts, multipart, authorize)
	return object, err
}

func streamLFSParts(ctx context.Context, multipart storage.MultipartStore, key, uploadID, oid string, size int64, body io.Reader) ([]storage.MultipartPart, error) {
	buffer := make([]byte, LFSPartBytes)
	digest := sha256.New()
	reader := &contextReader{ctx: ctx, reader: body}
	parts := make([]storage.MultipartPart, 0, size/int64(LFSPartBytes)+1)
	remaining := size
	for remaining > 0 || len(parts) == 0 {
		partSize := min(remaining, int64(len(buffer)))
		chunk := buffer[:int(partSize)]
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return nil, errors.Join(ErrLFSHashMismatch, ErrInvalid, err)
		}
		_, _ = digest.Write(chunk)
		part, err := multipart.UploadPart(ctx, key, uploadID, len(parts)+1, bytes.NewReader(chunk), partSize)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
		remaining -= partSize
	}
	var extra [1]byte
	n, err := io.ReadFull(reader, extra[:])
	if n != 0 || !errors.Is(err, io.EOF) || hex.EncodeToString(digest.Sum(nil)) != oid {
		return nil, errors.Join(ErrLFSHashMismatch, ErrInvalid, err)
	}
	return parts, nil
}

func verifyLFSBody(ctx context.Context, body io.Reader, oid string, size int64) error {
	digest := sha256.New()
	n, err := io.CopyBuffer(digest, &contextReader{ctx: ctx, reader: io.LimitReader(body, size+1)}, make([]byte, 32<<10))
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(digest.Sum(nil)) != oid {
		return errors.Join(ErrLFSHashMismatch, ErrInvalid)
	}
	return nil
}

func (s *Store) finishExistingLFS(ctx context.Context, repositoryID string, object LFSObject, authorize func(context.Context) error) (result LFSObject, err error) {
	unlock, err := s.lockLFS(ctx, repositoryID)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	if err := authorize(ctx); err != nil {
		return result, errors.Join(ErrForbidden, err)
	}
	// An old, unreferenced object can be collected while the retry body is
	// being validated. Never report success after collection won that race.
	record, err := s.checkLFSRecord(ctx, repositoryID, object.OID)
	if err != nil {
		return result, errors.Join(ErrConflict, err)
	}
	if record.Object != object {
		return result, ErrCorrupt
	}
	return record.Object, nil
}

func (s *Store) lockLFS(ctx context.Context, repositoryID string) (func() error, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		unlock, err := s.lockRepository(waitCtx, repositoryID)
		if !errors.Is(err, ErrMaintenanceBusy) {
			return unlock, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return nil, errors.Join(err, waitCtx.Err())
		case <-timer.C:
		}
	}
}

func (s *Store) reserveLFS(ctx context.Context, repositoryID string, object LFSObject, limits LFSLimits, multipart storage.MultipartStore) (reservation lfsReservation, key string, version storage.Version, existing bool, err error) {
	unlock, err := s.lockLFS(ctx, repositoryID)
	if err != nil {
		return reservation, key, version, false, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	record, err := s.checkLFSRecord(ctx, repositoryID, object.OID)
	if err == nil {
		if record.Object.Size != object.Size {
			return reservation, key, version, false, errors.Join(ErrInvalid, ErrLFSHashMismatch)
		}
		return lfsReservation{Object: record.Object}, "", "", true, nil
	}
	if !errors.Is(err, ErrLFSMissing) {
		return reservation, key, version, false, err
	}
	if err := s.checkLFSQuota(ctx, repositoryID, object, limits); err != nil {
		return reservation, key, version, false, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return reservation, key, version, false, err
	}
	id := hex.EncodeToString(token[:])
	reservation = lfsReservation{SchemaVersion: 1, Object: object, Key: "lfs/objects/" + id, CreatedAt: time.Now().UTC()}
	reservation.ExpiresAt = reservation.CreatedAt.Add(LFSReservationLifetime)
	key = "repos/" + repositoryID + "/lfs/uploads/" + id + ".json"
	info, err := s.putLFSJSON(ctx, key, reservation, storage.PutOptions{IfNoneMatch: true})
	if err != nil {
		return reservation, key, version, false, err
	}
	version = info.Version
	reservation.UploadID, err = multipart.CreateMultipart(ctx, "repos/"+repositoryID+"/"+reservation.Key)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return reservation, key, version, false, errors.Join(err, s.objects.Delete(cleanupCtx, key, version))
	}
	info, err = s.putLFSJSON(ctx, key, reservation, storage.PutOptions{IfMatch: version})
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		abortErr := multipart.AbortMultipart(cleanupCtx, "repos/"+repositoryID+"/"+reservation.Key, reservation.UploadID)
		if abortErr != nil && !errors.Is(abortErr, storage.ErrNotFound) {
			return reservation, key, version, false, errors.Join(err, abortErr)
		}
		// Read the current version after an ambiguous update. This unique key
		// belongs to this upload, and the repository lock fences collection.
		current, headErr := s.objects.Head(cleanupCtx, key)
		var deleteErr error
		if headErr == nil {
			deleteErr = s.objects.Delete(cleanupCtx, key, current.Version)
		}
		return reservation, key, version, false, errors.Join(err, abortErr, headErr, deleteErr)
	}
	return reservation, key, info.Version, false, nil
}

func (s *Store) putLFSJSON(ctx context.Context, key string, value any, options storage.PutOptions) (storage.ObjectInfo, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return s.objects.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
}

func (s *Store) finishLFS(ctx context.Context, repositoryID, reservationKey string, version storage.Version, reservation lfsReservation, parts []storage.MultipartPart, multipart storage.MultipartStore, authorize func(context.Context) error) (object LFSObject, completed bool, err error) {
	unlock, err := s.lockLFS(ctx, repositoryID)
	if err != nil {
		return object, false, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	info, err := s.objects.Head(ctx, reservationKey)
	if err != nil || info.Version != version || !time.Now().Before(reservation.ExpiresAt) {
		return object, false, errors.Join(ErrConflict, err)
	}
	if err := authorize(ctx); err != nil {
		return object, false, errors.Join(ErrForbidden, err)
	}
	info, err = multipart.CompleteMultipart(ctx, "repos/"+repositoryID+"/"+reservation.Key, reservation.UploadID, parts)
	if err != nil {
		return object, false, err
	}
	completed = true
	if info.Size != reservation.Object.Size || info.Version == "" {
		return object, completed, ErrCorrupt
	}
	if err := authorize(ctx); err != nil {
		return object, completed, errors.Join(ErrForbidden, err)
	}
	record := lfsRecord{SchemaVersion: 1, Object: reservation.Object, Key: reservation.Key, Version: info.Version}
	_, err = s.putLFSJSON(ctx, lfsRecordKey(repositoryID, reservation.Object.OID), record, storage.PutOptions{IfNoneMatch: true})
	if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrPreconditionFailed) {
		existing, readErr := s.checkLFSRecord(ctx, repositoryID, reservation.Object.OID)
		if readErr == nil && existing.Object == reservation.Object {
			return existing.Object, completed, nil
		}
		return object, completed, errors.Join(ErrCorrupt, err, readErr)
	}
	return reservation.Object, completed, err
}

func (s *Store) releaseLFSReservation(ctx context.Context, repositoryID, key string, version storage.Version) (err error) {
	unlock, err := s.lockLFS(ctx, repositoryID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	err = s.objects.Delete(ctx, key, version)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

func (s *Store) lfsArtifacts(ctx context.Context, repositoryID string) ([]storage.ObjectInfo, error) {
	prefix := "repos/" + repositoryID + "/lfs/"
	var result []storage.ObjectInfo
	after := ""
	for {
		page, err := s.objects.ListPage(ctx, prefix, after, storage.MaxListPageSize)
		if err != nil {
			return nil, err
		}
		if len(result)+len(page.Objects) > 3*maxLFSRecords {
			return nil, ErrLimit
		}
		for _, item := range page.Objects {
			if !strings.HasPrefix(item.Key, prefix) || item.Key <= after {
				return nil, ErrCorrupt
			}
			after = item.Key
			result = append(result, item)
		}
		if page.NextAfter == "" {
			return result, nil
		}
		if len(page.Objects) == 0 || page.NextAfter != after {
			return nil, ErrCorrupt
		}
	}
}

func (s *Store) readLFSReservation(ctx context.Context, repositoryID string, info storage.ObjectInfo) (lfsReservation, error) {
	var reservation lfsReservation
	data, err := s.read(ctx, info.Key, 8192)
	if err != nil {
		return reservation, err
	}
	if err := decodeJSON(data, &reservation); err != nil {
		return reservation, err
	}
	id := strings.TrimPrefix(reservation.Key, "lfs/objects/")
	if reservation.SchemaVersion != 1 || !ValidLFSOID(reservation.Object.OID) || reservation.Object.Size < 0 ||
		!validLFSDataKey(reservation.Key) || info.Key != "repos/"+repositoryID+"/lfs/uploads/"+id+".json" ||
		reservation.CreatedAt.IsZero() || !reservation.ExpiresAt.Equal(reservation.CreatedAt.Add(LFSReservationLifetime)) {
		return reservation, ErrCorrupt
	}
	return reservation, nil
}

func (s *Store) checkLFSQuota(ctx context.Context, repositoryID string, object LFSObject, limits LFSLimits) error {
	artifacts, err := s.lfsArtifacts(ctx, repositoryID)
	if err != nil {
		return err
	}
	var used int64
	physical := map[string]storage.ObjectInfo{}
	repositoryPrefix := "repos/" + repositoryID + "/"
	for _, info := range artifacts {
		key := strings.TrimPrefix(info.Key, repositoryPrefix)
		if !strings.HasPrefix(key, "lfs/objects/") {
			continue
		}
		if !validLFSDataKey(key) || info.Size < 0 || info.Version == "" {
			return ErrCorrupt
		}
		if len(physical) >= maxLFSRecords || info.Size > limits.MaxRepositoryBytes-used {
			return errors.Join(ErrLimit, ErrLFSQuota)
		}
		physical[key] = info
		used += info.Size
	}
	count := len(physical)
	records := 0
	prefix := "repos/" + repositoryID + "/lfs/"
	for _, info := range artifacts {
		key := strings.TrimPrefix(info.Key, prefix)
		var size int64
		switch {
		case strings.HasPrefix(key, "verified/"):
			oid := strings.TrimSuffix(strings.TrimPrefix(key, "verified/"), ".json")
			if !ValidLFSOID(oid) || key != "verified/"+oid+".json" {
				return ErrCorrupt
			}
			record, err := s.readLFSRecord(ctx, repositoryID, oid)
			if err != nil {
				return err
			}
			payload, ok := physical[record.Key]
			if !ok || payload.Size != record.Object.Size || payload.Version != record.Version {
				return ErrCorrupt
			}
		case strings.HasPrefix(key, "uploads/"):
			reservation, err := s.readLFSReservation(ctx, repositoryID, info)
			if err != nil {
				return err
			}
			if reservation.Object.OID == object.OID {
				return fmt.Errorf("LFS upload for this object is already reserved: %w", ErrConflict)
			}
			if payload, ok := physical[reservation.Key]; ok {
				if payload.Size != reservation.Object.Size {
					return ErrCorrupt
				}
			} else {
				size = reservation.Object.Size
				count++
			}
		default:
			continue
		}
		records++
		if count >= maxLFSRecords || records >= 2*maxLFSRecords || size > limits.MaxRepositoryBytes-used {
			return errors.Join(ErrLimit, ErrLFSQuota)
		}
		used += size
	}
	if count >= maxLFSRecords || object.Size > limits.MaxRepositoryBytes-used {
		return errors.Join(ErrLimit, ErrLFSQuota)
	}
	return nil
}
