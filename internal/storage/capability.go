package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// VerifyConditionalOperations exercises the atomic preconditions GitOne uses
// to publish repository state, fence writers, and collect orphan artifacts.
// It also checks one object's immediate listing visibility after create, update,
// and deletion. These observations cannot prove provider consistency guarantees.
// The probe creates and removes one unique object beneath prefix and leaves no
// object behind when cleanup succeeds.
func VerifyConditionalOperations(
	ctx context.Context,
	store ObjectStore,
	prefix string,
) (returnErr error) {
	if store == nil {
		return fmt.Errorf("verify conditional operations: object store is nil")
	}
	if err := ValidatePrefix(prefix); err != nil {
		return fmt.Errorf("verify conditional operations: %w", err)
	}

	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("verify conditional operations: generate probe key: %w", err)
	}
	key := strings.TrimSuffix(prefix, "/") + "/" + hex.EncodeToString(random)
	created := false
	defer func() {
		if !created {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cleanupErr := store.Delete(cleanupCtx, key, "")
		if cleanupErr != nil && !errors.Is(cleanupErr, ErrNotFound) {
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("verify conditional operations: clean up probe: %w", cleanupErr),
			)
		}
	}()

	initial := []byte("gitone-conditional-probe-v1")
	first, err := store.Put(
		ctx,
		key,
		bytes.NewReader(initial),
		int64(len(initial)),
		PutOptions{IfNoneMatch: true},
	)
	if err != nil {
		return fmt.Errorf("verify conditional operations: create probe: %w", err)
	}
	created = true
	if first.Version == "" {
		return fmt.Errorf(
			"verify conditional operations: create returned no version: %w",
			ErrConditionalUnsupported,
		)
	}
	if err := verifyProbeListing(ctx, store, key, &first); err != nil {
		return err
	}

	duplicate := []byte("gitone-duplicate-must-not-win")
	_, err = store.Put(
		ctx,
		key,
		bytes.NewReader(duplicate),
		int64(len(duplicate)),
		PutOptions{IfNoneMatch: true},
	)
	if err == nil {
		return fmt.Errorf(
			"verify conditional operations: If-None-Match overwrite succeeded: %w",
			ErrConditionalUnsupported,
		)
	}
	if !errors.Is(err, ErrAlreadyExists) {
		return fmt.Errorf("verify conditional operations: repeat create: %w", err)
	}
	if err := verifyObject(ctx, store, key, initial, first.Version); err != nil {
		return err
	}

	wrong := []byte("gitone-wrong-version-must-not-win")
	_, err = store.Put(
		ctx,
		key,
		bytes.NewReader(wrong),
		int64(len(wrong)),
		PutOptions{IfMatch: Version("gitone-invalid-version")},
	)
	if err == nil {
		return fmt.Errorf(
			"verify conditional operations: wrong If-Match overwrite succeeded: %w",
			ErrConditionalUnsupported,
		)
	}
	if !errors.Is(err, ErrPreconditionFailed) {
		return fmt.Errorf("verify conditional operations: wrong-version update: %w", err)
	}
	if err := verifyObject(ctx, store, key, initial, first.Version); err != nil {
		return err
	}

	updated := []byte("gitone-conditional-probe-v2")
	second, err := store.Put(
		ctx,
		key,
		bytes.NewReader(updated),
		int64(len(updated)),
		PutOptions{IfMatch: first.Version},
	)
	if err != nil {
		return fmt.Errorf("verify conditional operations: matching-version update: %w", err)
	}
	if second.Version == "" || second.Version == first.Version {
		return fmt.Errorf(
			"verify conditional operations: update did not advance object version: %w",
			ErrConditionalUnsupported,
		)
	}
	if err := verifyObject(ctx, store, key, updated, second.Version); err != nil {
		return err
	}
	if err := verifyProbeListing(ctx, store, key, &second); err != nil {
		return err
	}

	stale := []byte("gitone-stale-version-must-not-win")
	_, err = store.Put(
		ctx,
		key,
		bytes.NewReader(stale),
		int64(len(stale)),
		PutOptions{IfMatch: first.Version},
	)
	if err == nil {
		return fmt.Errorf(
			"verify conditional operations: stale If-Match overwrite succeeded: %w",
			ErrConditionalUnsupported,
		)
	}
	if !errors.Is(err, ErrPreconditionFailed) {
		return fmt.Errorf("verify conditional operations: stale-version update: %w", err)
	}
	if err := verifyObject(ctx, store, key, updated, second.Version); err != nil {
		return err
	}

	for _, version := range []Version{"gitone-invalid-version", first.Version} {
		if err := store.Delete(ctx, key, version); err == nil {
			return fmt.Errorf(
				"verify conditional operations: wrong If-Match delete succeeded: %w",
				ErrConditionalUnsupported,
			)
		} else if !errors.Is(err, ErrPreconditionFailed) {
			return fmt.Errorf("verify conditional operations: wrong-version delete: %w", err)
		}
		if err := verifyObject(ctx, store, key, updated, second.Version); err != nil {
			return err
		}
	}

	if err := store.Delete(ctx, key, second.Version); err != nil {
		return fmt.Errorf("verify conditional operations: delete probe: %w", err)
	}
	if _, err := store.Head(ctx, key); err == nil {
		return fmt.Errorf(
			"verify conditional operations: delete left probe present: %w",
			ErrConditionalUnsupported,
		)
	} else if !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("verify conditional operations: verify probe deletion: %w", err)
	}
	if err := verifyProbeListing(ctx, store, key, nil); err != nil {
		return err
	}
	created = false
	return nil
}

func verifyObject(
	ctx context.Context,
	store ObjectStore,
	key string,
	expected []byte,
	expectedVersion Version,
) error {
	body, info, err := store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("verify conditional operations: read probe: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(body, int64(len(expected))+1))
	closeErr := body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return fmt.Errorf("verify conditional operations: read probe body: %w", err)
	}
	if !bytes.Equal(data, expected) || info.Version != expectedVersion {
		return fmt.Errorf(
			"verify conditional operations: conditional write changed probe: %w",
			ErrConditionalUnsupported,
		)
	}
	return nil
}

func verifyProbeListing(ctx context.Context, store ObjectStore, key string, expected *ObjectInfo) error {
	// The random full key is also its own prefix, isolating this one-object
	// probe from other instances checking the same shard at startup.
	page, err := store.ListPage(ctx, key, "", 1)
	if err != nil {
		return fmt.Errorf("verify storage listing consistency: %w", err)
	}
	if expected == nil {
		if len(page.Objects) == 0 && page.NextAfter == "" {
			return nil
		}
		return fmt.Errorf("listing retained deleted probe: %w", ErrConsistencyUnsupported)
	}
	if len(page.Objects) != 1 || page.NextAfter != "" {
		return fmt.Errorf("listing omitted or duplicated written probe: %w", ErrConsistencyUnsupported)
	}
	got := page.Objects[0]
	if got.Key != key || got.Size != expected.Size || got.Version != expected.Version {
		return fmt.Errorf("listing returned stale probe metadata: %w", ErrConsistencyUnsupported)
	}
	return nil
}
