package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
)

const (
	// MinMultipartPartSize is the minimum size of every non-final part.
	MinMultipartPartSize int64 = 5 << 20
	// MaxMultipartPartSize bounds a single replayable transfer buffer.
	MaxMultipartPartSize int64 = 64 << 20
	// MaxMultipartParts bounds upload metadata and follows the S3 part limit.
	MaxMultipartParts = 10000
)

var (
	// ErrMultipartUnsupported reports a provider without multipart uploads.
	ErrMultipartUnsupported = errors.New("multipart uploads are unsupported")
	// ErrInvalidMultipart reports invalid part metadata or upload parameters.
	ErrInvalidMultipart = errors.New("invalid multipart upload")
)

// MultipartPart identifies a successfully uploaded part. Retrying the same number
// replaces that part; completion must use its most recently returned ETag.
type MultipartPart struct {
	Number int
	ETag   string
	Size   int64
}

// MultipartStore uploads objects without local disk or whole-object buffering.
// Callers must use unique physical keys because completion can replace an object.
// Parts remain invisible until completion. Failed uploads must be aborted;
// providers need lifecycle cleanup for process crashes. Body contains exactly
// size bytes from its current position, supports seeking for retries, and remains
// owned by the caller. Implementations never close it.
type MultipartStore interface {
	CreateMultipart(ctx context.Context, key string) (string, error)
	UploadPart(ctx context.Context, key, uploadID string, number int, body io.ReadSeeker, size int64) (MultipartPart, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart) (ObjectInfo, error)
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

// ValidateMultipart validates the key and opaque provider upload identifier.
func ValidateMultipart(key, uploadID string) error {
	if err := ValidateKey(key); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMultipart, err)
	}
	if uploadID == "" || len(uploadID) > 4096 {
		return fmt.Errorf("%w: missing or oversized upload identifier", ErrInvalidMultipart)
	}
	return nil
}

// ValidateMultipartPart bounds a part before reading its payload.
func ValidateMultipartPart(number int, size int64) error {
	if number < 1 || number > MaxMultipartParts || size < 0 || size > MaxMultipartPartSize {
		return fmt.Errorf("%w: part number or size outside limits", ErrInvalidMultipart)
	}
	return nil
}

// MultipartSize validates an ordered, consecutive completion list and sums its
// size. Only the last part may be smaller than MinMultipartPartSize.
func MultipartSize(parts []MultipartPart) (int64, error) {
	if len(parts) == 0 || len(parts) > MaxMultipartParts {
		return 0, fmt.Errorf("%w: invalid part count", ErrInvalidMultipart)
	}
	var size int64
	for i, part := range parts {
		if err := ValidateMultipartPart(part.Number, part.Size); err != nil {
			return 0, err
		}
		if part.Number != i+1 || part.ETag == "" || len(part.ETag) > 1024 {
			return 0, fmt.Errorf("%w: parts must be consecutive with valid ETags", ErrInvalidMultipart)
		}
		if i < len(parts)-1 && part.Size < MinMultipartPartSize {
			return 0, fmt.Errorf("%w: non-final part is too small", ErrInvalidMultipart)
		}
		size += part.Size // 10,000 parts of 64 MiB cannot overflow int64.
	}
	return size, nil
}
