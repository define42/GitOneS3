// Package storage defines the durable object-store contracts used by a shard.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNotFound reports that an object or repository does not exist.
	ErrNotFound = errors.New("object not found")
	// ErrAlreadyExists reports an attempted overwrite of an immutable object.
	ErrAlreadyExists = errors.New("object already exists")
	// ErrPreconditionFailed reports a failed optimistic-concurrency check.
	ErrPreconditionFailed = errors.New("object precondition failed")
	// ErrConditionalConflict reports a retryable conflicting conditional S3 operation.
	ErrConditionalConflict = errors.New("conditional object request conflict")
	// ErrConditionalUnsupported reports an object store that does not enforce
	// the conditional operations required for repository publication.
	ErrConditionalUnsupported = errors.New("conditional object operations are unsupported")
	// ErrConsistencyUnsupported reports stale object listings observed by the
	// startup capability probe. Strong read and listing consistency is required.
	ErrConsistencyUnsupported = errors.New("strong object store consistency is unsupported")
	// ErrInvalidRange reports an invalid or unsatisfiable byte range.
	ErrInvalidRange = errors.New("invalid object range")
)

// Version is an opaque object version suitable for a subsequent If-Match write.
type Version string

// ObjectInfo describes a stored object without exposing provider-specific types.
type ObjectInfo struct {
	Key string
	// Size is the complete object size, including for a ranged read.
	Size         int64
	Version      Version
	LastModified time.Time
}

// MaxListPageSize bounds one object listing request.
const MaxListPageSize = 1000

// ObjectPage contains a lexicographically ordered page of object metadata.
// NextAfter is the last returned key when more objects exist, otherwise empty.
// Pages are not a snapshot across requests; concurrent changes may affect later pages.
type ObjectPage struct {
	Objects   []ObjectInfo
	NextAfter string
}

// PutOptions controls conditional object publication.
type PutOptions struct {
	IfMatch     Version
	IfNoneMatch bool
}

// ObjectStore is the shard-scoped durable object storage boundary.
//
// Implementations must apply PutOptions and conditional Delete atomically, and
// provide strongly consistent reads and prefix listings after writes and deletes.
// GC depends on complete listings of immutable state snapshots while writers are
// fenced. Pagination need not provide a snapshot across concurrent mutations.
// A deployment must reject S3-compatible providers without these guarantees;
// capability probes detect violations but cannot prove a provider's guarantees.
type ObjectStore interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) (ObjectInfo, error)
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ObjectInfo, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string, ifMatch Version) error
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// ListPage returns at most limit objects strictly after the full key after.
	// An empty after starts the listing; otherwise it must be a valid key within
	// prefix. The limit must be between 1 and MaxListPageSize, inclusive.
	ListPage(ctx context.Context, prefix, after string, limit int) (ObjectPage, error)
}
