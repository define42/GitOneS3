package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

// ErrMaintenanceBusy means a repository writer or maintenance operation holds
// the durable repository lock. Locks never expire automatically: a paused
// process must not resume writing after another process starts collecting.
var ErrMaintenanceBusy = errors.New("repository: write or maintenance operation in progress")

// MaintenanceLock identifies the operation that currently fences writers.
// A token is an operator recovery identifier, not an authentication credential.
type MaintenanceLock struct {
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"createdAt"`
}

func maintenanceLockKey(repositoryID string) string {
	return "repos/" + repositoryID + "/maintenance-lock"
}

// lockRepository serializes durable writes with garbage collection. Every
// mutation of existing repository artifacts must hold this lock. Readers need
// no lock because collection retains all immutable generation snapshots.
func (s *Store) lockRepository(ctx context.Context, repositoryID string) (func() error, error) {
	if !idPattern.MatchString(repositoryID) {
		return nil, ErrInvalid
	}
	var randomToken [16]byte
	if _, err := rand.Read(randomToken[:]); err != nil {
		return nil, fmt.Errorf("generate repository lock token: %w", err)
	}
	lock := MaintenanceLock{Token: hex.EncodeToString(randomToken[:]), CreatedAt: time.Now().UTC()}
	data, err := json.Marshal(lock)
	if err != nil {
		return nil, fmt.Errorf("encode repository lock: %w", err)
	}
	key := maintenanceLockKey(repositoryID)
	info, err := s.objects.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfNoneMatch: true})
	if err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrPreconditionFailed) || errors.Is(err, storage.ErrConditionalConflict) {
			return nil, errors.Join(ErrMaintenanceBusy, ErrConflict)
		}
		return nil, fmt.Errorf("acquire repository lock: %w", err)
	}
	if info.Version == "" {
		return nil, fmt.Errorf("repository lock has no conditional version: %w", storage.ErrConditionalUnsupported)
	}
	return func() error {
		// Release must survive cancellation of the request that acquired it.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := s.objects.Delete(releaseCtx, key, info.Version); err != nil {
			return fmt.Errorf("release repository lock (token %s): %w", lock.Token, err)
		}
		return nil
	}, nil
}

// GetMaintenanceLock returns the durable lock, if any. ErrNotFound means that
// no writer or maintenance operation currently holds it.
func (s *Store) GetMaintenanceLock(ctx context.Context, namespace, name string) (MaintenanceLock, error) {
	metadata, err := s.maintenanceMetadata(ctx, namespace, name)
	if err != nil {
		return MaintenanceLock{}, err
	}
	data, err := s.read(ctx, maintenanceLockKey(metadata.ID), 4096)
	if errors.Is(err, storage.ErrNotFound) {
		return MaintenanceLock{}, ErrNotFound
	}
	if err != nil {
		return MaintenanceLock{}, err
	}
	var lock MaintenanceLock
	if err := decodeJSON(data, &lock); err != nil {
		return MaintenanceLock{}, err
	}
	if !idPattern.MatchString(lock.Token) || lock.CreatedAt.IsZero() {
		return MaintenanceLock{}, ErrCorrupt
	}
	return lock, nil
}

// UnlockMaintenance recovers a lock left by a terminated process. The operator
// must first stop every process using this repository, including maintenance
// commands; offline must explicitly acknowledge that requirement. Automatic
// timeout recovery would let a paused writer race with destructive collection.
func (s *Store) UnlockMaintenance(ctx context.Context, namespace, name, token string, offline bool) error {
	if !offline || !idPattern.MatchString(token) {
		return ErrInvalid
	}
	metadata, err := s.maintenanceMetadata(ctx, namespace, name)
	if err != nil {
		return err
	}
	key := maintenanceLockKey(metadata.ID)
	body, info, err := s.objects.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("read repository lock: %w", err)
	}
	var lock MaintenanceLock
	decoder := json.NewDecoder(io.LimitReader(body, 4097))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&lock)
	if decodeErr == nil {
		if trailingErr := decoder.Decode(new(any)); trailingErr != io.EOF {
			decodeErr = ErrCorrupt
		}
	}
	closeErr := body.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return fmt.Errorf("decode repository lock: %w", err)
	}
	if lock.Token != token || info.Version == "" || lock.CreatedAt.IsZero() || info.Size > 4096 {
		return ErrConflict
	}
	if err := s.objects.Delete(ctx, key, info.Version); err != nil {
		return fmt.Errorf("recover repository lock: %w", err)
	}
	return nil
}
