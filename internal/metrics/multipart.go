package metrics

import (
	"context"
	"io"

	"github.com/define42/GitOneS3/internal/storage"
)

// CreateMultipart measures upload initialization or reports a missing capability.
func (s *Store) CreateMultipart(ctx context.Context, key string) (string, error) {
	started := s.start(opCreateMultipart)
	var id string
	err := storage.ErrMultipartUnsupported
	if inner, ok := s.inner.(storage.MultipartStore); ok {
		id, err = inner.CreateMultipart(ctx, key)
	}
	s.finish(opCreateMultipart, started, err)
	return id, err
}

// UploadPart preserves retries and counts every byte read, including checksum
// passes and SDK retries. Keys and provider upload identifiers are never labels.
func (s *Store) UploadPart(ctx context.Context, key, uploadID string, number int, body io.ReadSeeker, size int64) (storage.MultipartPart, error) {
	started := s.start(opUploadPart)
	var part storage.MultipartPart
	err := storage.ErrMultipartUnsupported
	if inner, ok := s.inner.(storage.MultipartStore); ok {
		if body != nil {
			body = struct {
				io.Reader
				io.Seeker
			}{&reader{inner: body, store: s, op: opUploadPart}, body}
		}
		part, err = inner.UploadPart(ctx, key, uploadID, number, body, size)
	}
	s.finish(opUploadPart, started, err)
	return part, err
}

// CompleteMultipart measures atomic publication of an uploaded object.
func (s *Store) CompleteMultipart(ctx context.Context, key, uploadID string, parts []storage.MultipartPart) (storage.ObjectInfo, error) {
	started := s.start(opCompleteMultipart)
	var info storage.ObjectInfo
	err := storage.ErrMultipartUnsupported
	if inner, ok := s.inner.(storage.MultipartStore); ok {
		info, err = inner.CompleteMultipart(ctx, key, uploadID, parts)
	}
	s.finish(opCompleteMultipart, started, err)
	return info, err
}

// AbortMultipart measures discarded multipart uploads.
func (s *Store) AbortMultipart(ctx context.Context, key, uploadID string) error {
	started := s.start(opAbortMultipart)
	err := storage.ErrMultipartUnsupported
	if inner, ok := s.inner.(storage.MultipartStore); ok {
		err = inner.AbortMultipart(ctx, key, uploadID)
	}
	s.finish(opAbortMultipart, started, err)
	return err
}

var _ storage.MultipartStore = (*Store)(nil)
