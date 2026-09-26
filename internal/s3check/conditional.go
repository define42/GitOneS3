package s3check

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/define42/GitOneS3/internal/storage"
)

func (s *suite) verify(ctx context.Context, key string, expected []byte, version storage.Version) error {
	body, info, err := s.store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("read %q: %w", key, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(body, int64(len(expected))+1))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return err
	}
	if !bytes.Equal(data, expected) || info.Size != int64(len(expected)) || info.Version != version {
		return fmt.Errorf("get %q returned stale or incorrect content, size, or etag", key)
	}
	head, err := s.store.Head(ctx, key)
	if err != nil {
		return err
	}
	if head.Size != info.Size || head.Version != version {
		return fmt.Errorf("head %q disagrees with the written object", key)
	}
	return nil
}

func (s *suite) missing(ctx context.Context, key string) error {
	if _, err := s.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		return unexpectedProbeResult(fmt.Sprintf("head %q: expected not found", key), err)
	}
	body, _, err := s.store.Get(ctx, key)
	if body != nil {
		err = errors.Join(err, body.Close())
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return unexpectedProbeResult(fmt.Sprintf("get %q: expected not found", key), err)
	}
	return nil
}

func (s *suite) basic(ctx context.Context) error {
	key := s.key("basic/object")
	data := []byte("gitone\x00s3-range-check\xff")
	info, err := s.put(ctx, key, data, storage.PutOptions{})
	if err != nil {
		return err
	}
	if err := s.verify(ctx, key, data, info.Version); err != nil {
		return err
	}
	body, ranged, err := s.store.GetRange(ctx, key, 3, 8)
	if err != nil {
		return err
	}
	part, readErr := io.ReadAll(io.LimitReader(body, 9))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return err
	}
	if !bytes.Equal(part, data[3:11]) || ranged.Size != int64(len(data)) || ranged.Version != info.Version {
		return errors.New("ranged get returned incorrect content, total size, or etag")
	}
	if err := s.store.Delete(ctx, key, ""); err != nil {
		return err
	}
	return s.missing(ctx, key)
}

func (s *suite) conditionalCreate(ctx context.Context) error {
	key := s.key("conditional/create")
	data := []byte("first writer")
	info, err := s.put(ctx, key, data, storage.PutOptions{IfNoneMatch: true})
	if err != nil {
		return err
	}
	_, err = s.put(ctx, key, []byte("must not overwrite"), storage.PutOptions{IfNoneMatch: true})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		return unexpectedProbeResult("duplicate if-none-match put: expected precondition rejection", err)
	}
	return s.verify(ctx, key, data, info.Version)
}

func (s *suite) conditionalUpdate(ctx context.Context) error {
	key := s.key("conditional/update")
	_, err := s.put(ctx, key, []byte("must not create"), storage.PutOptions{IfMatch: `"missing-etag"`})
	if !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrPreconditionFailed) {
		return unexpectedProbeResult("if-match put on missing key: expected not found or precondition rejection", err)
	}
	if err := s.missing(ctx, key); err != nil {
		return fmt.Errorf("verify rejected if-match put left key absent: %w", err)
	}
	data := []byte("original")
	first, err := s.put(ctx, key, data, storage.PutOptions{})
	if err != nil {
		return err
	}
	_, err = s.put(ctx, key, []byte("must not win"), storage.PutOptions{IfMatch: `"invalid-etag"`})
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		return unexpectedProbeResult("wrong-etag put: expected precondition rejection", err)
	}
	if err := s.verify(ctx, key, data, first.Version); err != nil {
		return err
	}
	replacement := []byte("updated value")
	second, err := s.put(ctx, key, replacement, storage.PutOptions{IfMatch: first.Version})
	if err != nil {
		return fmt.Errorf("matching-etag put: %w", err)
	}
	if second.Version == first.Version {
		return errors.New("etag did not change after changing the object")
	}
	_, err = s.put(ctx, key, []byte("stale writer"), storage.PutOptions{IfMatch: first.Version})
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		return unexpectedProbeResult("stale-etag put: expected precondition rejection", err)
	}
	return s.verify(ctx, key, replacement, second.Version)
}

func (s *suite) conditionalDelete(ctx context.Context) error {
	key := s.key("conditional/delete")
	first, err := s.put(ctx, key, []byte("original"), storage.PutOptions{})
	if err != nil {
		return err
	}
	replacement := []byte("surviving replacement")
	second, err := s.put(ctx, key, replacement, storage.PutOptions{})
	if err != nil {
		return err
	}
	if first.Version == second.Version {
		return errors.New("etag did not change after changing the object")
	}
	for _, version := range []storage.Version{`"invalid-etag"`, first.Version} {
		if err := s.store.Delete(ctx, key, version); !errors.Is(err, storage.ErrPreconditionFailed) {
			return unexpectedProbeResult("wrong/stale-etag delete: expected precondition rejection", err)
		}
		if err := s.verify(ctx, key, replacement, second.Version); err != nil {
			return err
		}
	}
	if err := s.store.Delete(ctx, key, second.Version); err != nil {
		return fmt.Errorf("matching-etag delete: %w", err)
	}
	return s.missing(ctx, key)
}

type writeResult struct {
	info storage.ObjectInfo
	data []byte
	err  error
}

func (s *suite) competingWrites(ctx context.Context, create bool) error {
	for round := range 3 {
		key := s.key(fmt.Sprintf("concurrent/create-%t/%d", create, round))
		opts := storage.PutOptions{IfNoneMatch: create}
		if !create {
			initial, err := s.put(ctx, key, []byte("initial"), storage.PutOptions{})
			if err != nil {
				return err
			}
			opts.IfMatch = initial.Version
		}
		start := make(chan struct{})
		results := make(chan writeResult, 4)
		for i := range 4 {
			go func() {
				<-start
				data := fmt.Appendf(nil, "contender-%d", i)
				info, err := s.put(ctx, key, data, opts)
				results <- writeResult{info: info, data: data, err: err}
			}()
		}
		close(start)
		var winner writeResult
		var failures []error
		successes := 0
		for range 4 {
			result := <-results
			if result.err == nil {
				successes++
				winner = result
			} else if !conditionalFailure(result.err) && (!create || !errors.Is(result.err, storage.ErrAlreadyExists)) {
				failures = append(failures, result.err)
			}
		}
		if successes != 1 || len(failures) != 0 {
			return errors.Join(
				fmt.Errorf("round %d: expected one successful writer, got %d", round+1, successes),
				errors.Join(failures...),
			)
		}
		if err := s.verify(ctx, key, winner.data, winner.info.Version); err != nil {
			return err
		}
	}
	return nil
}

func conditionalFailure(err error) bool {
	return errors.Is(err, storage.ErrPreconditionFailed) || errors.Is(err, storage.ErrConditionalConflict)
}

func (s *suite) competingDelete(ctx context.Context) error {
	for round := range 8 {
		key := s.key(fmt.Sprintf("concurrent/delete/%d", round))
		initial, err := s.put(ctx, key, []byte("initial"), storage.PutOptions{})
		if err != nil {
			return err
		}
		data := []byte("replacement")
		start := make(chan struct{})
		updated := make(chan writeResult, 1)
		deleted := make(chan error, 1)
		go func() {
			<-start
			info, err := s.put(ctx, key, data, storage.PutOptions{IfMatch: initial.Version})
			updated <- writeResult{info: info, err: err}
		}()
		go func() {
			<-start
			deleted <- s.store.Delete(ctx, key, initial.Version)
		}()
		close(start)
		put, deleteErr := <-updated, <-deleted
		if (put.err == nil) == (deleteErr == nil) {
			if put.err == nil {
				return fmt.Errorf("round %d: update and delete both succeeded", round+1)
			}
			return fmt.Errorf("round %d: update and delete both failed: update=%w delete=%w", round+1, put.err, deleteErr)
		}
		if put.err == nil {
			if !conditionalFailure(deleteErr) {
				return fmt.Errorf("losing delete failed unexpectedly: %w", deleteErr)
			}
			if err := s.verify(ctx, key, data, put.info.Version); err != nil {
				return err
			}
		} else {
			if !conditionalFailure(put.err) && !errors.Is(put.err, storage.ErrNotFound) {
				return fmt.Errorf("losing update failed unexpectedly: %w", put.err)
			}
			if err := s.missing(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// unexpectedProbeResult preserves unexpected provider errors while giving an
// explicit failure when an operation that should have been rejected succeeds.
func unexpectedProbeResult(operation string, err error) error {
	if err == nil {
		return fmt.Errorf("%s; operation unexpectedly succeeded", operation)
	}
	return fmt.Errorf("%s; unexpected error: %w", operation, err)
}
