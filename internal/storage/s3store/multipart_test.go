package s3store

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- Test verifies the S3 Content-MD5 transport checksum.
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestMultipartLifecycle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const key = "repos/example/lfs/data/random-key"
	var createCalls, partCalls, completeCalls, abortCalls int
	client := &multipartClient{
		create: func(got context.Context, input *s3.CreateMultipartUploadInput) (*s3.CreateMultipartUploadOutput, error) {
			createCalls++
			if got != ctx || aws.ToString(input.Bucket) != "gitone-test-shard-000" || aws.ToString(input.Key) != key {
				t.Fatal("incorrect bucket, key, or context")
			}
			return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload")}, nil
		},
		part: func(got context.Context, input *s3.UploadPartInput) (*s3.UploadPartOutput, error) {
			partCalls++
			if got != ctx || aws.ToString(input.Bucket) != "gitone-test-shard-000" || aws.ToString(input.Key) != key || aws.ToString(input.UploadId) != "upload" || aws.ToInt32(input.PartNumber) != 1 || aws.ToInt64(input.ContentLength) != 7 {
				t.Fatal("incorrect part request")
			}
			// #nosec G401 -- Test verifies the S3 transport checksum, not an identity.
			digest := md5.Sum([]byte("payload"))
			if aws.ToString(input.ContentMD5) != base64.StdEncoding.EncodeToString(digest[:]) {
				t.Fatal("missing or incorrect transport checksum")
			}
			seeker, ok := input.Body.(io.ReadSeeker)
			if !ok {
				t.Fatal("SDK body cannot retry")
			}
			for range 2 {
				if _, err := seeker.Seek(0, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(seeker)
				if err != nil || string(data) != "payload" {
					t.Fatalf("part retry: %q, %v", data, err)
				}
			}
			return &s3.UploadPartOutput{ETag: aws.String("part-etag")}, nil
		},
		complete: func(got context.Context, input *s3.CompleteMultipartUploadInput) (*s3.CompleteMultipartUploadOutput, error) {
			completeCalls++
			if got != ctx || aws.ToString(input.Bucket) != "gitone-test-shard-000" || aws.ToString(input.Key) != key || aws.ToString(input.UploadId) != "upload" {
				t.Fatal("incorrect complete request")
			}
			if input.MultipartUpload == nil || len(input.MultipartUpload.Parts) != 1 || aws.ToInt32(input.MultipartUpload.Parts[0].PartNumber) != 1 || aws.ToString(input.MultipartUpload.Parts[0].ETag) != "part-etag" {
				t.Fatal("incorrect completion manifest")
			}
			return &s3.CompleteMultipartUploadOutput{ETag: aws.String("complete-etag")}, nil
		},
		abort: func(got context.Context, input *s3.AbortMultipartUploadInput) (*s3.AbortMultipartUploadOutput, error) {
			abortCalls++
			if got != ctx || aws.ToString(input.Bucket) != "gitone-test-shard-000" || aws.ToString(input.Key) != key || aws.ToString(input.UploadId) != "upload" {
				t.Fatal("incorrect abort request")
			}
			return nil, &smithy.GenericAPIError{Code: "NoSuchUpload"}
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateMultipart(ctx, key)
	if err != nil || id != "upload" {
		t.Fatalf("create: %q, %v", id, err)
	}
	part, err := store.UploadPart(ctx, key, id, 1, strings.NewReader("payload"), 7)
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.CompleteMultipart(ctx, key, id, []storage.MultipartPart{part})
	if err != nil || info.Key != key || info.Size != 7 || info.Version != "complete-etag" || info.LastModified.IsZero() {
		t.Fatalf("complete: %+v, %v", info, err)
	}
	if err := store.AbortMultipart(ctx, key, id); err != nil {
		t.Fatal(err)
	}
	if createCalls != 1 || partCalls != 1 || completeCalls != 1 || abortCalls != 1 {
		t.Fatal("unexpected provider call count")
	}
}

func TestMultipartValidationAndCancellation(t *testing.T) {
	t.Parallel()
	store, err := New(&multipartClient{}, "gitone-test-shard-000")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		number int
		size   int64
		data   string
	}{
		{name: "truncated", number: 1, size: 2, data: "x"},
		{name: "excess", number: 1, size: 1, data: "xx"},
		{name: "negative", number: 1, size: -1},
		{name: "oversized", number: 1, size: storage.MaxMultipartPartSize + 1},
		{name: "invalid number", number: 0, size: 1, data: "x"},
		{name: "too many parts", number: storage.MaxMultipartParts + 1, size: 1, data: "x"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.UploadPart(t.Context(), "key", "upload", test.number, strings.NewReader(test.data), test.size); !errors.Is(err, storage.ErrInvalidMultipart) {
				t.Fatalf("error: %v", err)
			}
		})
	}
	if _, err := store.UploadPart(t.Context(), "key", "upload", 1, nil, 0); !errors.Is(err, storage.ErrInvalidMultipart) {
		t.Fatalf("nil body: %v", err)
	}
	if _, err := store.CompleteMultipart(t.Context(), "key", "upload", nil); !errors.Is(err, storage.ErrInvalidMultipart) {
		t.Fatalf("empty complete: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.CreateMultipart(ctx, "key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.UploadPart(ctx, "key", "upload", 1, strings.NewReader("x"), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("upload: %v", err)
	}
	if _, err := store.CompleteMultipart(ctx, "key", "upload", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("complete: %v", err)
	}
	if err := store.AbortMultipart(ctx, "key", "upload"); !errors.Is(err, context.Canceled) {
		t.Fatalf("abort: %v", err)
	}
}

func TestMultipartProviderErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, code string
		want       error
	}{
		{name: "missing upload", code: "NoSuchUpload", want: storage.ErrNotFound},
		{name: "bad checksum", code: "BadDigest", want: storage.ErrInvalidMultipart},
		{name: "invalid part", code: "InvalidPart", want: storage.ErrInvalidMultipart},
		{name: "invalid order", code: "InvalidPartOrder", want: storage.ErrInvalidMultipart},
		{name: "too small", code: "EntityTooSmall", want: storage.ErrInvalidMultipart},
		{name: "conflict", code: "ConditionalRequestConflict", want: storage.ErrConditionalConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &multipartClient{part: func(context.Context, *s3.UploadPartInput) (*s3.UploadPartOutput, error) {
				return nil, &smithy.GenericAPIError{Code: test.code}
			}}
			store, err := New(client, "gitone-test-shard-000")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.UploadPart(t.Context(), "key", "upload", 1, strings.NewReader("x"), 1); !errors.Is(err, test.want) {
				t.Fatalf("error: %v", err)
			}
		})
	}
	store, err := New(&fakeClient{}, "gitone-test-shard-000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMultipart(t.Context(), "key"); !errors.Is(err, storage.ErrMultipartUnsupported) {
		t.Fatalf("unsupported: %v", err)
	}
}

func TestMultipartRejectsEmptyResponses(t *testing.T) {
	t.Parallel()
	client := &multipartClient{
		create: func(context.Context, *s3.CreateMultipartUploadInput) (*s3.CreateMultipartUploadOutput, error) {
			return &s3.CreateMultipartUploadOutput{}, nil
		},
		part: func(context.Context, *s3.UploadPartInput) (*s3.UploadPartOutput, error) { return nil, nil },
		complete: func(context.Context, *s3.CompleteMultipartUploadInput) (*s3.CompleteMultipartUploadOutput, error) {
			return &s3.CompleteMultipartUploadOutput{}, nil
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMultipart(t.Context(), "key"); err == nil {
		t.Fatal("missing upload ID accepted")
	}
	if _, err := store.UploadPart(t.Context(), "key", "id", 1, bytes.NewReader(nil), 0); err == nil {
		t.Fatal("missing part ETag accepted")
	}
	if _, err := store.CompleteMultipart(t.Context(), "key", "id", []storage.MultipartPart{{Number: 1, ETag: "part"}}); err == nil {
		t.Fatal("missing final ETag accepted")
	}
}

type multipartClient struct {
	fakeClient
	create   func(context.Context, *s3.CreateMultipartUploadInput) (*s3.CreateMultipartUploadOutput, error)
	part     func(context.Context, *s3.UploadPartInput) (*s3.UploadPartOutput, error)
	complete func(context.Context, *s3.CompleteMultipartUploadInput) (*s3.CompleteMultipartUploadOutput, error)
	abort    func(context.Context, *s3.AbortMultipartUploadInput) (*s3.AbortMultipartUploadOutput, error)
}

func (c *multipartClient) CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	return c.create(ctx, in)
}
func (c *multipartClient) UploadPart(ctx context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	return c.part(ctx, in)
}
func (c *multipartClient) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	return c.complete(ctx, in)
}
func (c *multipartClient) AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	return c.abort(ctx, in)
}
