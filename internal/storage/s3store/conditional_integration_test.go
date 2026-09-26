//go:build integration

package s3store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/define42/GitOneS3/internal/storage"
)

// TestS3ConditionalDelete exercises the provider's atomic object preconditions
// in a newly created bucket. In particular, a stale delete must never remove a
// replacement object or a new owner's repository maintenance lock.
func TestS3ConditionalDelete(t *testing.T) {
	endpoint := os.Getenv("GITONE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GITONE_TEST_S3_ENDPOINT and normal AWS credentials")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("test endpoint must be HTTP(S) without credentials, query, or fragment")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "gitone-test-cas-" + hex.EncodeToString(random[:])
	create := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if region != "us-east-1" {
		create.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	if _, err := client.CreateBucket(ctx, create); err != nil {
		t.Fatalf("create isolated test bucket: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cleanupCancel()
		// This random bucket belongs exclusively to this test, including the
		// capability probe's random key if its own cleanup encountered an error.
		objects, err := client.ListObjectsV2(cleanupCtx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket), MaxKeys: aws.Int32(1000),
		})
		if err != nil {
			t.Errorf("list isolated test bucket for cleanup: %v", err)
			return
		}
		if aws.ToBool(objects.IsTruncated) {
			t.Error("isolated test bucket unexpectedly contains over 1000 objects")
			return
		}
		for _, object := range objects.Contents {
			if _, err := client.DeleteObject(cleanupCtx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key}); err != nil {
				t.Errorf("clean up test object: %v", err)
			}
		}
		if _, err := client.DeleteBucket(cleanupCtx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Errorf("clean up isolated test bucket: %v", err)
		}
	})
	store, err := New(client, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Check(ctx); err != nil {
		t.Fatalf("provider capability check: %v", err)
	}

	t.Run("stale_delete_preserves_replacement", func(t *testing.T) {
		key := "conditional/replacement"
		oldBody := []byte("original owner")
		old, err := store.Put(ctx, key, bytes.NewReader(oldBody), int64(len(oldBody)), storage.PutOptions{IfNoneMatch: true})
		if err != nil {
			t.Fatal(err)
		}
		newBody := []byte("replacement owner")
		current, err := store.Put(ctx, key, bytes.NewReader(newBody), int64(len(newBody)), storage.PutOptions{IfMatch: old.Version})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(current.Version), `"`) || !strings.HasSuffix(string(current.Version), `"`) {
			t.Fatalf("provider returned an unquoted ETag: %q", current.Version)
		}
		for _, version := range []storage.Version{`"wrong-etag"`, old.Version} {
			if err := store.Delete(ctx, key, version); !errors.Is(err, storage.ErrPreconditionFailed) {
				t.Fatalf("delete with %q = %v, want precondition failure", version, err)
			}
			assertS3ConditionalObject(t, ctx, store, key, newBody, current.Version)
		}
		if err := store.Delete(ctx, key, current.Version); err != nil {
			t.Fatalf("delete with current ETag: %v", err)
		}
		assertS3ConditionalObject(t, ctx, store, key, nil, "")
	})

	for i := range 8 {
		t.Run(fmt.Sprintf("update_delete_race_%d", i), func(t *testing.T) {
			key := fmt.Sprintf("conditional/race-%d", i)
			oldBody := []byte("original value")
			old, err := store.Put(ctx, key, bytes.NewReader(oldBody), int64(len(oldBody)), storage.PutOptions{IfNoneMatch: true})
			if err != nil {
				t.Fatal(err)
			}
			newBody := []byte("replacement value")
			type putResult struct {
				info storage.ObjectInfo
				err  error
			}
			start := make(chan struct{})
			updated := make(chan putResult, 1)
			deleted := make(chan error, 1)
			go func() {
				<-start
				info, err := store.Put(ctx, key, bytes.NewReader(newBody), int64(len(newBody)), storage.PutOptions{IfMatch: old.Version})
				updated <- putResult{info: info, err: err}
			}()
			go func() {
				<-start
				deleted <- store.Delete(ctx, key, old.Version)
			}()
			close(start)
			put, deleteErr := <-updated, <-deleted
			if (put.err == nil) == (deleteErr == nil) {
				t.Fatalf("exactly one operation must succeed: update=%v delete=%v", put.err, deleteErr)
			}
			if put.err == nil {
				if !errors.Is(deleteErr, storage.ErrPreconditionFailed) && !errors.Is(deleteErr, storage.ErrConditionalConflict) {
					t.Fatalf("losing delete = %v, want conditional failure", deleteErr)
				}
				assertS3ConditionalObject(t, ctx, store, key, newBody, put.info.Version)
				return
			}
			if !errors.Is(put.err, storage.ErrPreconditionFailed) && !errors.Is(put.err, storage.ErrConditionalConflict) && !errors.Is(put.err, storage.ErrNotFound) {
				t.Fatalf("losing update = %v, want conditional failure or missing object", put.err)
			}
			assertS3ConditionalObject(t, ctx, store, key, nil, "")
		})
	}
}

func assertS3ConditionalObject(t *testing.T, ctx context.Context, store *Store, key string, expected []byte, version storage.Version) {
	t.Helper()
	body, info, err := store.Get(ctx, key)
	if expected == nil {
		if body != nil {
			if closeErr := body.Close(); closeErr != nil {
				t.Errorf("close unexpected object: %v", closeErr)
			}
		}
		if !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("deleted object GET = %v, want missing", err)
		}
		if _, err := store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("deleted object HEAD = %v, want missing", err)
		}
	} else {
		if err != nil {
			t.Fatalf("read surviving object: %v", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(body, int64(len(expected))+1))
		if err := errors.Join(readErr, body.Close()); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, expected) || info.Version != version || info.Size != int64(len(expected)) {
			t.Fatalf("surviving object = (%q, %q, %d), want (%q, %q, %d)", data, info.Version, info.Size, expected, version, len(expected))
		}
	}
	page, err := store.ListPage(ctx, key, "", 1)
	if err != nil {
		t.Fatalf("list object after conditional operations: %v", err)
	}
	if expected == nil {
		if len(page.Objects) != 0 || page.NextAfter != "" {
			t.Fatalf("deleted object remains listed: %+v", page)
		}
		return
	}
	if len(page.Objects) != 1 || page.Objects[0].Key != key || page.Objects[0].Version != version || page.NextAfter != "" {
		t.Fatalf("listing disagrees with surviving object: %+v", page)
	}
}
