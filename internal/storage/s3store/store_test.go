package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestStore_PutImmutableCondition(t *testing.T) {
	t.Parallel()

	var captured *s3.PutObjectInput
	client := &fakeClient{
		putObject: func(input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
			captured = input
			return &s3.PutObjectOutput{ETag: aws.String(`"version-1"`)}, nil
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	info, err := store.Put(
		context.Background(),
		"packs/a.pack",
		bytes.NewBufferString("pack"),
		4,
		storage.PutOptions{IfNoneMatch: true},
	)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if got, want := aws.ToString(captured.Bucket), "gitone-test-shard-000"; got != want {
		t.Fatalf("Put() bucket = %q, want %q", got, want)
	}
	if got, want := aws.ToString(captured.IfNoneMatch), "*"; got != want {
		t.Fatalf("Put() IfNoneMatch = %q, want %q", got, want)
	}
	if got, want := info.Version, storage.Version(`"version-1"`); got != want {
		t.Fatalf("Put() version = %q, want %q", got, want)
	}
}

func TestStore_PutTranslatesImmutableConflict(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		putObject: func(*s3.PutObjectInput) (*s3.PutObjectOutput, error) {
			return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "exists"}
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = store.Put(
		context.Background(),
		"packs/a.pack",
		bytes.NewBufferString("pack"),
		4,
		storage.PutOptions{IfNoneMatch: true},
	)
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("Put() error = %v, want ErrAlreadyExists", err)
	}
}

func TestStore_PutPreservesRetryableConditionalConflict(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		putObject: func(*s3.PutObjectInput) (*s3.PutObjectOutput, error) {
			return nil, &smithy.GenericAPIError{
				Code:    "ConditionalRequestConflict",
				Message: "retry",
			}
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = store.Put(
		context.Background(),
		"packs/a.pack",
		bytes.NewBufferString("pack"),
		4,
		storage.PutOptions{IfNoneMatch: true},
	)
	if !errors.Is(err, storage.ErrConditionalConflict) {
		t.Fatalf("Put() error = %v, want ErrConditionalConflict", err)
	}
}

func TestStore_GetRangeUsesInclusiveEnd(t *testing.T) {
	t.Parallel()

	var captured *s3.GetObjectInput
	client := &fakeClient{
		getObject: func(input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			captured = input
			return &s3.GetObjectOutput{
				Body:          io.NopCloser(bytes.NewBufferString("3456")),
				ContentLength: aws.Int64(4),
				ContentRange:  aws.String("bytes 3-6/10"),
				ETag:          aws.String(`"version-1"`),
			}, nil
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	body, info, err := store.GetRange(context.Background(), "packs/a.pack", 3, 4)
	if err != nil {
		t.Fatalf("GetRange() error = %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got, want := aws.ToString(captured.Range), "bytes=3-6"; got != want {
		t.Fatalf("GetRange() Range = %q, want %q", got, want)
	}
	if info.Size != 10 {
		t.Fatalf("GetRange() object size = %d, want 10", info.Size)
	}
}

func TestStore_GetRangeRejectsIgnoredRangeAndClosesBody(t *testing.T) {
	t.Parallel()

	body := &trackedReadCloser{Reader: bytes.NewBufferString("0123456789")}
	client := &fakeClient{
		getObject: func(*s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{
				Body:          body,
				ContentLength: aws.Int64(10),
				ETag:          aws.String(`"version-1"`),
			}, nil
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, _, err = store.GetRange(context.Background(), "packs/a.pack", 3, 4)
	if err == nil {
		t.Fatal("GetRange() error = nil, want missing Content-Range error")
	}
	if !body.closed {
		t.Fatal("GetRange() did not close rejected response body")
	}
}

func TestStore_GetRangeRejectsMismatchedRange(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		getObject: func(*s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{
				Body:          io.NopCloser(bytes.NewBufferString("0123")),
				ContentLength: aws.Int64(4),
				ContentRange:  aws.String("bytes 0-3/10"),
				ETag:          aws.String(`"version-1"`),
			}, nil
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, _, err := store.GetRange(
		context.Background(),
		"packs/a.pack",
		3,
		4,
	); err == nil {
		t.Fatal("GetRange() error = nil, want mismatched Content-Range error")
	}
}

func TestStore_GetRangeTranslatesProviderInvalidRange(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		getObject: func(*s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return nil, &smithy.GenericAPIError{Code: "InvalidRange", Message: "past eof"}
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, _, err = store.GetRange(context.Background(), "packs/a.pack", 30, 4)
	if !errors.Is(err, storage.ErrInvalidRange) {
		t.Fatalf("GetRange() error = %v, want ErrInvalidRange", err)
	}
}

func TestStore_CheckVerifiesConditionalOperationsOnce(t *testing.T) {
	t.Parallel()

	var data []byte
	var etag string
	var exists bool
	var version int
	var putCalls int
	var storedKey string
	client := &fakeClient{
		headBucket: func(*s3.HeadBucketInput) (*s3.HeadBucketOutput, error) {
			return &s3.HeadBucketOutput{}, nil
		},
		putObject: func(input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
			putCalls++
			if aws.ToString(input.IfNoneMatch) == "*" && exists {
				return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "exists"}
			}
			if match := aws.ToString(input.IfMatch); match != "" && (!exists || match != etag) {
				return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "version"}
			}
			body, err := io.ReadAll(input.Body)
			if err != nil {
				return nil, err
			}
			version++
			storedKey = aws.ToString(input.Key)
			data = bytes.Clone(body)
			etag = fmt.Sprintf(`"v%d"`, version)
			exists = true
			return &s3.PutObjectOutput{ETag: aws.String(etag)}, nil
		},
		getObject: func(input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			if !exists {
				return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}
			}
			return &s3.GetObjectOutput{
				Body:          io.NopCloser(bytes.NewReader(bytes.Clone(data))),
				ContentLength: aws.Int64(int64(len(data))),
				ETag:          aws.String(etag),
			}, nil
		},
		headObject: func(*s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
			if !exists {
				return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}
			}
			return &s3.HeadObjectOutput{
				ContentLength: aws.Int64(int64(len(data))),
				ETag:          aws.String(etag),
			}, nil
		},
		listObjects: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			output := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
			if exists {
				output.Contents = []types.Object{{Key: aws.String(storedKey), ETag: aws.String(etag), Size: aws.Int64(int64(len(data)))}}
			}
			return output, nil
		},
		deleteObject: func(input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
			if match := aws.ToString(input.IfMatch); match != "" && (!exists || match != etag) {
				return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "version"}
			}
			if !exists {
				return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}
			}
			exists = false
			return &s3.DeleteObjectOutput{}, nil
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatalf("first Check() error = %v", err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatalf("second Check() error = %v", err)
	}
	if putCalls != 5 {
		t.Fatalf("conditional probe Put calls = %d, want 5", putCalls)
	}
}

func TestStore_ListRejectsRepeatedContinuationToken(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		listObjects: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			return &s3.ListObjectsV2Output{
				IsTruncated:           aws.Bool(true),
				NextContinuationToken: aws.String("same-token"),
			}, nil
		},
	}
	store, err := New(client, "gitone-test-shard-000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := store.List(context.Background(), "packs/"); err == nil {
		t.Fatal("List() error = nil, want repeated-token error")
	}
}

func TestStore_ListPage(t *testing.T) {
	t.Parallel()

	modified := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		after       string
		limit       int
		keys        []string
		isTruncated bool
		nextAfter   string
	}{
		{name: "first page", limit: 2, keys: []string{"packs/a", "packs/b"}, isTruncated: true, nextAfter: "packs/b"},
		{name: "later page", after: "packs/b", limit: 2, keys: []string{"packs/c", "packs/d"}},
		{name: "short nonterminal page", after: "packs/a", limit: 2, keys: []string{"packs/b"}, isTruncated: true, nextAfter: "packs/b"},
		{name: "empty page", limit: 1, keys: []string{}},
		{name: "end of listing", after: "packs/z", limit: storage.MaxListPageSize, keys: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls int
			client := &fakeClient{
				listObjects: func(input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
					calls++
					if calls != 1 {
						t.Fatal("ListPage() made more than one provider request")
					}
					if aws.ToString(input.Bucket) != "gitone-test-shard-000" || aws.ToString(input.Prefix) != "packs/" {
						t.Fatalf("ListPage() unexpected bucket or prefix: %+v", input)
					}
					if aws.ToString(input.StartAfter) != test.after || int(aws.ToInt32(input.MaxKeys)) != test.limit {
						t.Fatalf("ListPage() cursor or limit differs from request: %+v", input)
					}
					if input.ContinuationToken != nil || input.Delimiter != nil {
						t.Fatal("ListPage() must request full keys without continuation tokens or delimiter")
					}
					objects := make([]types.Object, 0, len(test.keys))
					for _, key := range test.keys {
						objects = append(objects, types.Object{
							Key:          aws.String(key),
							Size:         aws.Int64(17),
							ETag:         aws.String(`"version"`),
							LastModified: aws.Time(modified),
						})
					}
					return &s3.ListObjectsV2Output{
						Contents:    objects,
						IsTruncated: aws.Bool(test.isTruncated),
					}, nil
				},
			}
			store, err := New(client, "gitone-test-shard-000")
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListPage(t.Context(), "packs/", test.after, test.limit)
			if err != nil {
				t.Fatalf("ListPage() error = %v", err)
			}
			keys := make([]string, 0, len(page.Objects))
			for _, object := range page.Objects {
				keys = append(keys, object.Key)
				if object.Size != 17 || object.Version != `"version"` || !object.LastModified.Equal(modified) {
					t.Fatalf("ListPage() lost object metadata: %+v", object)
				}
			}
			if !slices.Equal(keys, test.keys) || page.NextAfter != test.nextAfter || calls != 1 {
				t.Fatalf("ListPage() = %+v, calls = %d, want keys %v and cursor %q", page, calls, test.keys, test.nextAfter)
			}
		})
	}
}

func TestStore_ListPageRejectsMalformedResponse(t *testing.T) {
	t.Parallel()

	object := func(key string) types.Object { return types.Object{Key: aws.String(key)} }
	tests := []struct {
		name   string
		output *s3.ListObjectsV2Output
	}{
		{name: "nil response"},
		{name: "missing pagination metadata", output: &s3.ListObjectsV2Output{}},
		{name: "empty nonterminal page", output: &s3.ListObjectsV2Output{IsTruncated: aws.Bool(true)}},
		{name: "too many objects", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(false),
			Contents:    []types.Object{object("packs/c"), object("packs/d"), object("packs/e")},
		}},
		{name: "common prefixes", output: &s3.ListObjectsV2Output{
			IsTruncated:    aws.Bool(false),
			CommonPrefixes: []types.CommonPrefix{{Prefix: aws.String("packs/c/")}},
		}},
		{name: "inconsistent key count", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(false), KeyCount: aws.Int32(2), Contents: []types.Object{object("packs/c")},
		}},
		{name: "negative key count", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(false), KeyCount: aws.Int32(-1),
		}},
		{name: "unordered keys", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(false), Contents: []types.Object{object("packs/d"), object("packs/c")},
		}},
		{name: "duplicate keys", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(true), Contents: []types.Object{object("packs/c"), object("packs/c")},
		}},
		{name: "foreign prefix", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(false), Contents: []types.Object{object("other/c")},
		}},
		{name: "before cursor", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(false), Contents: []types.Object{object("packs/a")},
		}},
		{name: "cursor repeated", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(true), Contents: []types.Object{object("packs/b")},
		}},
		{name: "invalid key", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(false), Contents: []types.Object{object("packs/c/../d")},
		}},
		{name: "missing key", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(false), Contents: []types.Object{{}},
		}},
		{name: "negative size", output: &s3.ListObjectsV2Output{
			IsTruncated: aws.Bool(false), Contents: []types.Object{{Key: aws.String("packs/c"), Size: aws.Int64(-1)}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls int
			store, err := New(&fakeClient{
				listObjects: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
					calls++
					return test.output, nil
				},
			}, "gitone-test-shard-000")
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListPage(t.Context(), "packs/", "packs/b", 2)
			if err == nil || len(page.Objects) != 0 || page.NextAfter != "" || calls != 1 {
				t.Fatalf("ListPage() = %+v, error %v, calls %d; want failure without partial results", page, err, calls)
			}
		})
	}
}

func TestStore_ListPageRejectsInvalidRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix string
		after  string
		limit  int
	}{
		{name: "invalid prefix", prefix: "packs//", limit: 1},
		{name: "foreign cursor", prefix: "packs/", after: "other/a", limit: 1},
		{name: "invalid cursor", prefix: "packs/", after: "packs/../a", limit: 1},
		{name: "zero limit", prefix: "packs/", limit: 0},
		{name: "excessive limit", prefix: "packs/", limit: storage.MaxListPageSize + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, err := New(&fakeClient{
				listObjects: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
					t.Fatal("ListPage() sent an invalid request to the provider")
					return nil, nil
				},
			}, "gitone-test-shard-000")
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.ListPage(t.Context(), test.prefix, test.after, test.limit)
			if err == nil {
				t.Fatal("ListPage() accepted an invalid request")
			}
		})
	}
}

func TestStore_ListPagePreservesProviderError(t *testing.T) {
	t.Parallel()

	expected := errors.New("provider unavailable")
	store, err := New(&fakeClient{
		listObjects: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			return nil, expected
		},
	}, "gitone-test-shard-000")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ListPage(t.Context(), "packs/", "", 1)
	if !errors.Is(err, expected) {
		t.Fatalf("ListPage() error = %v, want provider error", err)
	}
}

func TestStore_ListPageCanceled(t *testing.T) {
	t.Parallel()

	store, err := New(&fakeClient{
		listObjects: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			t.Fatal("ListPage() sent a canceled request to the provider")
			return nil, nil
		},
	}, "gitone-test-shard-000")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.ListPage(ctx, "packs/", "", 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ListPage() error = %v, want context.Canceled", err)
	}
}

type trackedReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackedReadCloser) Close() error {
	r.closed = true
	return nil
}

type fakeClient struct {
	putObject    func(*s3.PutObjectInput) (*s3.PutObjectOutput, error)
	getObject    func(*s3.GetObjectInput) (*s3.GetObjectOutput, error)
	headObject   func(*s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	headBucket   func(*s3.HeadBucketInput) (*s3.HeadBucketOutput, error)
	deleteObject func(*s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	listObjects  func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
}

func (c *fakeClient) PutObject(
	_ context.Context,
	input *s3.PutObjectInput,
	_ ...func(*s3.Options),
) (*s3.PutObjectOutput, error) {
	return c.putObject(input)
}

func (c *fakeClient) GetObject(
	_ context.Context,
	input *s3.GetObjectInput,
	_ ...func(*s3.Options),
) (*s3.GetObjectOutput, error) {
	return c.getObject(input)
}

func (c *fakeClient) HeadObject(
	_ context.Context,
	input *s3.HeadObjectInput,
	_ ...func(*s3.Options),
) (*s3.HeadObjectOutput, error) {
	return c.headObject(input)
}

func (c *fakeClient) HeadBucket(
	_ context.Context,
	input *s3.HeadBucketInput,
	_ ...func(*s3.Options),
) (*s3.HeadBucketOutput, error) {
	return c.headBucket(input)
}

func (c *fakeClient) DeleteObject(
	_ context.Context,
	input *s3.DeleteObjectInput,
	_ ...func(*s3.Options),
) (*s3.DeleteObjectOutput, error) {
	return c.deleteObject(input)
}

func (c *fakeClient) ListObjectsV2(
	_ context.Context,
	input *s3.ListObjectsV2Input,
	_ ...func(*s3.Options),
) (*s3.ListObjectsV2Output, error) {
	return c.listObjects(input)
}
