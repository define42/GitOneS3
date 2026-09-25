// Package s3store adapts an S3 bucket to storage.ObjectStore.
package s3store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/define42/GitOneS3/internal/storage"
)

var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

type s3API interface {
	PutObject(
		context.Context,
		*s3.PutObjectInput,
		...func(*s3.Options),
	) (*s3.PutObjectOutput, error)
	GetObject(
		context.Context,
		*s3.GetObjectInput,
		...func(*s3.Options),
	) (*s3.GetObjectOutput, error)
	HeadObject(
		context.Context,
		*s3.HeadObjectInput,
		...func(*s3.Options),
	) (*s3.HeadObjectOutput, error)
	HeadBucket(
		context.Context,
		*s3.HeadBucketInput,
		...func(*s3.Options),
	) (*s3.HeadBucketOutput, error)
	DeleteObject(
		context.Context,
		*s3.DeleteObjectInput,
		...func(*s3.Options),
	) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(
		context.Context,
		*s3.ListObjectsV2Input,
		...func(*s3.Options),
	) (*s3.ListObjectsV2Output, error)
}

// Store is permanently scoped to one shard bucket.
type Store struct {
	client             s3API
	bucket             string
	capabilityMu       sync.Mutex
	capabilityVerified bool
}

// New constructs a fixed-bucket S3 object store.
func New(client s3API, bucket string) (*Store, error) {
	if client == nil {
		return nil, errors.New("S3 client is required")
	}
	if !bucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") {
		return nil, fmt.Errorf("invalid S3 bucket name %q", bucket)
	}

	return &Store{client: client, bucket: bucket}, nil
}

// Check verifies that the fixed shard bucket is reachable with current
// credentials. It is suitable for readiness checks, not per-request use.
func (s *Store) Check(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	}); err != nil {
		return classifyError("head bucket", s.bucket, err)
	}

	s.capabilityMu.Lock()
	defer s.capabilityMu.Unlock()
	if s.capabilityVerified {
		return nil
	}
	if err := storage.VerifyConditionalOperations(
		ctx,
		s,
		"maintenance/capabilities/",
	); err != nil {
		return fmt.Errorf("verify bucket conditional operations: %w", err)
	}
	s.capabilityVerified = true

	return nil
}

// Put applies S3 If-Match or If-None-Match atomically.
func (s *Store) Put(
	ctx context.Context,
	key string,
	body io.Reader,
	size int64,
	opts storage.PutOptions,
) (storage.ObjectInfo, error) {
	if body == nil {
		return storage.ObjectInfo{}, fmt.Errorf("put %q: body is required", key)
	}
	if err := storage.ValidateKey(key); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("put %q: %w", key, err)
	}
	if size < 0 {
		return storage.ObjectInfo{}, fmt.Errorf("put %q: negative size", key)
	}
	if opts.IfMatch != "" && opts.IfNoneMatch {
		return storage.ObjectInfo{}, fmt.Errorf("put %q: conflicting preconditions", key)
	}

	input := &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
	}
	if opts.IfMatch != "" {
		input.IfMatch = aws.String(string(opts.IfMatch))
	}
	if opts.IfNoneMatch {
		input.IfNoneMatch = aws.String("*")
	}

	output, err := s.client.PutObject(ctx, input)
	if err != nil {
		if isAlreadyExists(err) && opts.IfNoneMatch {
			return storage.ObjectInfo{}, fmt.Errorf("put %q: %w", key, storage.ErrAlreadyExists)
		}
		return storage.ObjectInfo{}, classifyError("put", key, err)
	}
	if output == nil {
		return storage.ObjectInfo{}, fmt.Errorf("put %q: S3 response is empty", key)
	}
	if aws.ToString(output.ETag) == "" {
		return storage.ObjectInfo{}, fmt.Errorf("put %q: S3 response has no ETag", key)
	}

	return storage.ObjectInfo{
		Key:          key,
		Size:         size,
		Version:      storage.Version(aws.ToString(output.ETag)),
		LastModified: time.Now().UTC(),
	}, nil
}

// Get streams a complete S3 object.
func (s *Store) Get(
	ctx context.Context,
	key string,
) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return nil, storage.ObjectInfo{}, fmt.Errorf("get %q: %w", key, err)
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, storage.ObjectInfo{}, classifyError("get", key, err)
	}
	if output == nil || output.Body == nil {
		return nil, storage.ObjectInfo{}, fmt.Errorf("get %q: S3 response has no body", key)
	}
	version := storage.Version(aws.ToString(output.ETag))
	if version == "" {
		closeErr := output.Body.Close()
		return nil, storage.ObjectInfo{}, errors.Join(
			fmt.Errorf("get %q: S3 response has no ETag", key),
			closeErr,
		)
	}

	return output.Body, storage.ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(output.ContentLength),
		Version:      version,
		LastModified: aws.ToTime(output.LastModified),
	}, nil
}

// GetRange streams an inclusive S3 byte range derived from offset and length.
func (s *Store) GetRange(
	ctx context.Context,
	key string,
	offset, length int64,
) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return nil, storage.ObjectInfo{}, fmt.Errorf("get range %q: %w", key, err)
	}
	if offset < 0 || length <= 0 || offset > math.MaxInt64-length+1 {
		return nil, storage.ObjectInfo{}, fmt.Errorf("get range %q: %w", key, storage.ErrInvalidRange)
	}
	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		classified := classifyError("get range", key, err)
		if statusCode(err) == 416 {
			classified = fmt.Errorf("get range %q: %w", key, storage.ErrInvalidRange)
		}
		return nil, storage.ObjectInfo{}, classified
	}
	if output == nil || output.Body == nil {
		return nil, storage.ObjectInfo{}, fmt.Errorf("get range %q: S3 response has no body", key)
	}
	version := storage.Version(aws.ToString(output.ETag))
	if version == "" {
		closeErr := output.Body.Close()
		return nil, storage.ObjectInfo{}, errors.Join(
			fmt.Errorf("get range %q: S3 response has no ETag", key),
			closeErr,
		)
	}
	start, end, total, err := parseContentRange(aws.ToString(output.ContentRange))
	if err != nil {
		closeErr := output.Body.Close()
		return nil, storage.ObjectInfo{}, errors.Join(
			fmt.Errorf("get range %q: invalid S3 Content-Range: %w", key, err),
			closeErr,
		)
	}
	expectedEnd := offset + length - 1
	if expectedEnd >= total {
		expectedEnd = total - 1
	}
	expectedLength := end - start + 1
	if start != offset || end != expectedEnd || aws.ToInt64(output.ContentLength) != expectedLength {
		closeErr := output.Body.Close()
		return nil, storage.ObjectInfo{}, errors.Join(
			fmt.Errorf(
				"get range %q: S3 response range %d-%d/%d with length %d does not match request %s",
				key,
				start,
				end,
				total,
				aws.ToInt64(output.ContentLength),
				rangeHeader,
			),
			closeErr,
		)
	}

	return output.Body, storage.ObjectInfo{
		Key:          key,
		Size:         total,
		Version:      version,
		LastModified: aws.ToTime(output.LastModified),
	}, nil
}

// Head returns S3 object metadata.
func (s *Store) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("head %q: %w", key, err)
	}
	output, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return storage.ObjectInfo{}, classifyError("head", key, err)
	}
	if output == nil {
		return storage.ObjectInfo{}, fmt.Errorf("head %q: S3 response is empty", key)
	}
	version := storage.Version(aws.ToString(output.ETag))
	if version == "" {
		return storage.ObjectInfo{}, fmt.Errorf("head %q: S3 response has no ETag", key)
	}

	return storage.ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(output.ContentLength),
		Version:      version,
		LastModified: aws.ToTime(output.LastModified),
	}, nil
}

// Delete conditionally removes an S3 object.
func (s *Store) Delete(ctx context.Context, key string, ifMatch storage.Version) error {
	if err := storage.ValidateKey(key); err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	input := &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	if ifMatch != "" {
		input.IfMatch = aws.String(string(ifMatch))
	}
	if _, err := s.client.DeleteObject(ctx, input); err != nil {
		return classifyError("delete", key, err)
	}

	return nil
}

// List returns all object metadata under prefix without loading object bodies.
func (s *Store) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	if err := storage.ValidatePrefix(prefix); err != nil {
		return nil, fmt.Errorf("list %q: %w", prefix, err)
	}
	var objects []storage.ObjectInfo
	var continuationToken *string
	seenContinuationTokens := make(map[string]struct{})
	for {
		output, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, classifyError("list", prefix, err)
		}
		if output == nil {
			return nil, fmt.Errorf("list %q: S3 response is empty", prefix)
		}
		for _, object := range output.Contents {
			objects = append(objects, storage.ObjectInfo{
				Key:          aws.ToString(object.Key),
				Size:         aws.ToInt64(object.Size),
				Version:      storage.Version(aws.ToString(object.ETag)),
				LastModified: aws.ToTime(object.LastModified),
			})
		}
		if !aws.ToBool(output.IsTruncated) {
			break
		}
		nextToken := aws.ToString(output.NextContinuationToken)
		if nextToken == "" {
			return nil, errors.New("list response is truncated without a continuation token")
		}
		if _, exists := seenContinuationTokens[nextToken]; exists {
			return nil, errors.New("list response repeats a continuation token")
		}
		seenContinuationTokens[nextToken] = struct{}{}
		continuationToken = aws.String(nextToken)
	}

	return objects, nil
}

func classifyError(operation, key string, err error) error {
	switch statusCode(err) {
	case 404:
		return fmt.Errorf("%s %q: %w", operation, key, storage.ErrNotFound)
	case 409:
		return fmt.Errorf("%s %q: %w", operation, key, storage.ErrConditionalConflict)
	case 412:
		return fmt.Errorf("%s %q: %w", operation, key, storage.ErrPreconditionFailed)
	}

	if apiError, ok := errors.AsType[smithy.APIError](err); ok {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return fmt.Errorf("%s %q: %w", operation, key, storage.ErrNotFound)
		case "PreconditionFailed":
			return fmt.Errorf("%s %q: %w", operation, key, storage.ErrPreconditionFailed)
		case "ConditionalRequestConflict":
			return fmt.Errorf("%s %q: %w", operation, key, storage.ErrConditionalConflict)
		case "InvalidRange", "RequestedRangeNotSatisfiable":
			return fmt.Errorf("%s %q: %w", operation, key, storage.ErrInvalidRange)
		}
	}

	return fmt.Errorf("%s %q: %w", operation, key, err)
}

func parseContentRange(input string) (start, end, total int64, err error) {
	unit, value, ok := strings.Cut(strings.TrimSpace(input), " ")
	if !ok || unit != "bytes" {
		return 0, 0, 0, fmt.Errorf("expected bytes range")
	}
	rangeValue, totalValue, ok := strings.Cut(value, "/")
	if !ok || totalValue == "*" {
		return 0, 0, 0, fmt.Errorf("missing total size")
	}
	startValue, endValue, ok := strings.Cut(rangeValue, "-")
	if !ok {
		return 0, 0, 0, fmt.Errorf("missing byte bounds")
	}
	start, startErr := strconv.ParseInt(startValue, 10, 64)
	end, endErr := strconv.ParseInt(endValue, 10, 64)
	total, totalErr := strconv.ParseInt(totalValue, 10, 64)
	if startErr != nil || endErr != nil || totalErr != nil {
		return 0, 0, 0, fmt.Errorf("non-numeric byte bounds")
	}
	if start < 0 || end < start || total <= end {
		return 0, 0, 0, fmt.Errorf("inconsistent byte bounds")
	}
	return start, end, total, nil
}

func isAlreadyExists(err error) bool {
	if statusCode(err) == 412 {
		return true
	}
	var apiError smithy.APIError
	return errors.As(err, &apiError) && apiError.ErrorCode() == "PreconditionFailed"
}

func statusCode(err error) int {
	if responseError, ok := errors.AsType[*awshttp.ResponseError](err); ok {
		return responseError.HTTPStatusCode()
	}

	return 0
}

var _ storage.ObjectStore = (*Store)(nil)
