package s3store

import (
	"context"
	"crypto/md5" // #nosec G501 -- S3 Content-MD5 detects transfer corruption; LFS identity uses SHA-256.
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/define42/GitOneS3/internal/storage"
)

type multipartAPI interface {
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// CreateMultipart begins a private upload within this shard's fixed bucket.
func (s *Store) CreateMultipart(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := storage.ValidateKey(key); err != nil {
		return "", fmt.Errorf("create multipart: %w: %w", storage.ErrInvalidMultipart, err)
	}
	client, ok := s.client.(multipartAPI)
	if !ok {
		return "", storage.ErrMultipartUnsupported
	}
	output, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return "", multipartError("create multipart", key, err)
	}
	if output == nil || aws.ToString(output.UploadId) == "" {
		return "", errors.New("create multipart: S3 response has no upload identifier")
	}
	if err := storage.ValidateMultipart(key, *output.UploadId); err != nil {
		return "", err
	}
	return *output.UploadId, nil
}

// UploadPart sends one bounded, seekable part with a transport checksum. The SDK
// can retry the part by rewinding the caller's buffer; no local file is created.
func (s *Store) UploadPart(ctx context.Context, key, uploadID string, number int, body io.ReadSeeker, size int64) (storage.MultipartPart, error) {
	if err := ctx.Err(); err != nil {
		return storage.MultipartPart{}, err
	}
	if err := storage.ValidateMultipart(key, uploadID); err != nil {
		return storage.MultipartPart{}, err
	}
	if err := storage.ValidateMultipartPart(number, size); err != nil {
		return storage.MultipartPart{}, err
	}
	if body == nil {
		return storage.MultipartPart{}, fmt.Errorf("%w: body is required", storage.ErrInvalidMultipart)
	}
	client, ok := s.client.(multipartAPI)
	if !ok {
		return storage.MultipartPart{}, storage.ErrMultipartUnsupported
	}
	checksum, err := partChecksum(ctx, body, size)
	if err != nil {
		return storage.MultipartPart{}, fmt.Errorf("upload part checksum: %w", err)
	}
	output, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		// #nosec G115 -- ValidateMultipartPart bounds the number to 1..10,000.
		PartNumber: aws.Int32(int32(number)), ContentLength: aws.Int64(size),
		Body: &multipartReader{ReadSeeker: body, ctx: ctx}, ContentMD5: aws.String(checksum),
	})
	if err != nil {
		return storage.MultipartPart{}, multipartError("upload part", key, err)
	}
	if output == nil || aws.ToString(output.ETag) == "" {
		return storage.MultipartPart{}, errors.New("upload part: S3 response has no ETag")
	}
	return storage.MultipartPart{Number: number, ETag: *output.ETag, Size: size}, nil
}

// CompleteMultipart publishes the ordered parts. Callers must verify the overall
// payload hash, length, and permissions before invoking this method.
func (s *Store) CompleteMultipart(ctx context.Context, key, uploadID string, parts []storage.MultipartPart) (storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := storage.ValidateMultipart(key, uploadID); err != nil {
		return storage.ObjectInfo{}, err
	}
	size, err := storage.MultipartSize(parts)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	client, ok := s.client.(multipartAPI)
	if !ok {
		return storage.ObjectInfo{}, storage.ErrMultipartUnsupported
	}
	completed := make([]types.CompletedPart, len(parts))
	for i, part := range parts {
		completed[i] = types.CompletedPart{
			ETag: aws.String(part.ETag),
			// #nosec G115 -- MultipartSize bounds the number to 1..10,000.
			PartNumber: aws.Int32(int32(part.Number)),
		}
	}
	output, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return storage.ObjectInfo{}, multipartError("complete multipart", key, err)
	}
	if output == nil || aws.ToString(output.ETag) == "" {
		return storage.ObjectInfo{}, errors.New("complete multipart: S3 response has no ETag")
	}
	return storage.ObjectInfo{Key: key, Size: size, Version: storage.Version(*output.ETag), LastModified: time.Now().UTC()}, nil
}

// AbortMultipart discards pending parts. Repeating an abort is harmless.
func (s *Store) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := storage.ValidateMultipart(key, uploadID); err != nil {
		return err
	}
	client, ok := s.client.(multipartAPI)
	if !ok {
		return storage.ErrMultipartUnsupported
	}
	_, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	if err != nil {
		classified := multipartError("abort multipart", key, err)
		if errors.Is(classified, storage.ErrNotFound) {
			return nil
		}
		return classified
	}
	return nil
}

func partChecksum(ctx context.Context, body io.ReadSeeker, size int64) (string, error) {
	start, err := body.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", err
	}
	// #nosec G401 -- Required S3 transport checksum, not a cryptographic identity.
	digest := md5.New()
	count, readErr := io.Copy(digest, io.LimitReader(&multipartReader{ReadSeeker: body, ctx: ctx}, size+1))
	_, seekErr := body.Seek(start, io.SeekStart)
	if err := errors.Join(readErr, seekErr); err != nil {
		return "", err
	}
	if count != size {
		return "", fmt.Errorf("%w: body size is %d, expected %d", storage.ErrInvalidMultipart, count, size)
	}
	return base64.StdEncoding.EncodeToString(digest.Sum(nil)), nil
}

type multipartReader struct {
	io.ReadSeeker
	ctx context.Context
}

func (r *multipartReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadSeeker.Read(p)
}

func multipartError(operation, key string, err error) error {
	if apiError, ok := errors.AsType[smithy.APIError](err); ok {
		switch apiError.ErrorCode() {
		case "NoSuchUpload":
			return fmt.Errorf("%s %q: %w", operation, key, storage.ErrNotFound)
		case "InvalidPart", "InvalidPartOrder", "EntityTooSmall", "BadDigest", "InvalidDigest":
			return fmt.Errorf("%s %q: %w: %w", operation, key, storage.ErrInvalidMultipart, err)
		}
	}
	return classifyError(operation, key, err)
}

var _ storage.MultipartStore = (*Store)(nil)
