package auth

import (
	"context"
	"errors"
	"io"

	"github.com/define42/GitOneS3/internal/storage"
)

var errUsernameTaken = errors.New("username is bound to a different Google account")

// bindUser claims a new name atomically or verifies its existing subject binding.
// It never reassigns ownership based on email or on the requested username.
func (s *Service) bindUser(ctx context.Context, username string, identity Identity) error {
	err := s.writeNamespace(ctx, username, namespaceRecord{SchemaVersion: 1, Type: userNamespace, Identity: identity}, "")
	if err == nil {
		return nil
	}
	if !isNamespaceConflict(err) {
		return err
	}
	existing, _, err := s.loadNamespace(ctx, username)
	if err != nil {
		return err
	}
	if existing.Type != userNamespace || existing.Subject != identity.Subject {
		return errUsernameTaken
	}
	return nil
}

func (s *Service) readObject(ctx context.Context, key string) ([]byte, storage.Version, error) {
	body, info, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(body, 8193))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return nil, "", err
	}
	if len(data) > 8192 {
		return nil, "", errors.New("authentication record too large")
	}
	return data, info.Version, nil
}
