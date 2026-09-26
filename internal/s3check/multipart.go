package s3check

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/define42/GitOneS3/internal/storage"
)

func (s *suite) multipart(ctx context.Context) {
	s.check(ctx, "multipart.complete-retry-read-range", s.multipartComplete)
	s.check(ctx, "multipart.content-md5", s.multipartChecksum)
	s.check(ctx, "multipart.abort", s.multipartAbort)
}

func (s *suite) multipartComplete(ctx context.Context) error {
	key := s.key("multipart/complete")
	id, err := s.store.CreateMultipart(ctx, key)
	if err != nil {
		return fmt.Errorf("create multipart upload: %w", err)
	}
	s.trackUpload(key, id)
	first := bytes.Repeat([]byte{'a'}, int(storage.MinMultipartPartSize))
	if _, err := s.store.UploadPart(ctx, key, id, 1, bytes.NewReader(first), int64(len(first))); err != nil {
		return fmt.Errorf("upload first part: %w", err)
	}
	// A retry may replace bytes as well as repeat them. Completion must use the
	// latest ETag and publish the replacement exactly once.
	first[0] = 'b'
	part, err := s.store.UploadPart(ctx, key, id, 1, bytes.NewReader(first), int64(len(first)))
	if err != nil {
		return fmt.Errorf("replace first part: %w", err)
	}
	tail := []byte("tail")
	last, err := s.store.UploadPart(ctx, key, id, 2, bytes.NewReader(tail), int64(len(tail)))
	if err != nil {
		return fmt.Errorf("upload final part: %w", err)
	}
	if _, err := s.store.Head(ctx, key); err == nil {
		return errors.New("unfinished multipart object is visible before completion")
	} else if !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("check unfinished multipart object: %w", err)
	}
	info, err := s.store.CompleteMultipart(ctx, key, id, []storage.MultipartPart{part, last})
	if err != nil {
		return fmt.Errorf("complete multipart upload: %w", err)
	}
	size := int64(len(first) + len(tail))
	if info.Version == "" || info.Size != size {
		return fmt.Errorf("completion returned an empty ETag or incorrect size: %+v", info)
	}
	head, err := s.store.Head(ctx, key)
	if err != nil {
		return fmt.Errorf("head completed multipart object: %w", err)
	}
	if head.Size != size || head.Version != info.Version {
		return errors.New("HEAD metadata disagrees with multipart completion")
	}
	body, got, err := s.store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("read completed multipart object: %w", err)
	}
	readErr := compareMultipartBody(body, first, tail)
	if got.Size != size || got.Version != info.Version {
		readErr = errors.Join(readErr, errors.New("GET metadata disagrees with multipart completion"))
	}
	if readErr != nil {
		return fmt.Errorf("verify completed multipart object: %w", readErr)
	}
	body, got, err = s.store.GetRange(ctx, key, int64(len(first)-2), 6)
	if err != nil {
		return fmt.Errorf("read range across multipart boundary: %w", err)
	}
	readErr = compareMultipartBody(body, first[len(first)-2:], tail)
	if got.Size != size || got.Version != info.Version {
		readErr = errors.Join(readErr, errors.New("range metadata disagrees with multipart completion"))
	}
	if readErr != nil {
		return fmt.Errorf("verify multipart range: %w", readErr)
	}
	return nil
}

func (s *suite) multipartChecksum(ctx context.Context) error {
	key := s.key("multipart/checksum")
	id, err := s.store.CreateMultipart(ctx, key)
	if err != nil {
		return fmt.Errorf("create checksum upload: %w", err)
	}
	s.trackUpload(key, id)
	body := []byte("incorrect checksum must fail")
	_, err = s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(id),
		PartNumber: aws.Int32(1), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
		// This is a correctly encoded 16-byte digest that does not match body.
		ContentMD5: aws.String("AAAAAAAAAAAAAAAAAAAAAA=="),
	})
	if api, ok := errors.AsType[smithy.APIError](err); ok && (api.ErrorCode() == "BadDigest" || api.ErrorCode() == "InvalidDigest") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("incorrect Content-MD5 must fail with BadDigest or InvalidDigest: %w", err)
	}
	return errors.New("provider accepted a part with an incorrect Content-MD5")
}

func (s *suite) multipartAbort(ctx context.Context) error {
	key := s.key("multipart/abort")
	id, err := s.store.CreateMultipart(ctx, key)
	if err != nil {
		return fmt.Errorf("create upload to abort: %w", err)
	}
	s.trackUpload(key, id)
	body := []byte("abort")
	part, err := s.store.UploadPart(ctx, key, id, 1, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fmt.Errorf("upload part before abort: %w", err)
	}
	if err := s.store.AbortMultipart(ctx, key, id); err != nil {
		return fmt.Errorf("abort upload: %w", err)
	}
	// Use the SDK for these negative checks because the production adapter
	// intentionally maps NoSuchUpload to the more general ErrNotFound.
	_, partErr := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(id),
		PartNumber: aws.Int32(1), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
	})
	_, completeErr := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{
			PartNumber: aws.Int32(1), ETag: aws.String(part.ETag),
		}}},
	})
	if err := errors.Join(
		requireNoSuchUpload("upload part after abort", partErr),
		requireNoSuchUpload("complete after abort", completeErr),
	); err != nil {
		return err
	}
	if _, err := s.store.Head(ctx, key); err == nil {
		return errors.New("aborted multipart object is visible")
	} else if !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("check aborted multipart object: %w", err)
	}
	return nil
}

func requireNoSuchUpload(operation string, err error) error {
	if api, ok := errors.AsType[smithy.APIError](err); ok && api.ErrorCode() == "NoSuchUpload" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s must fail with NoSuchUpload: %w", operation, err)
	}
	return fmt.Errorf("%s unexpectedly succeeded", operation)
}

// compareMultipartBody verifies bytes and EOF while retaining only one small
// read buffer. It closes the response even after a short or corrupt transfer.
func compareMultipartBody(body io.ReadCloser, expected ...[]byte) (returnErr error) {
	defer func() { returnErr = errors.Join(returnErr, body.Close()) }()
	buffer := make([]byte, 64<<10)
	for _, segment := range expected {
		for len(segment) != 0 {
			n := min(len(buffer), len(segment))
			if _, err := io.ReadFull(body, buffer[:n]); err != nil {
				return fmt.Errorf("read multipart bytes: %w", err)
			}
			if !bytes.Equal(buffer[:n], segment[:n]) {
				return errors.New("multipart bytes differ from the uploaded parts")
			}
			segment = segment[n:]
		}
	}
	n, err := io.ReadFull(body, buffer[:1])
	if n != 0 {
		return errors.New("multipart response contains excess bytes")
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("read multipart response end: %w", err)
	}
	return nil
}
