//go:build integration

package s3store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	"github.com/aws/smithy-go"

	"github.com/define42/GitOneS3/internal/storage"
)

// TestS3MultipartStreaming owns a newly created random bucket. It deliberately
// tests multipart independently of the application's conditional-write probes.
func TestS3MultipartStreaming(t *testing.T) {
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
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(endpoint); o.UsePathStyle = true })
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "gitone-test-lfs-" + hex.EncodeToString(random[:])
	create := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if region != "us-east-1" {
		create.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	if _, err := client.CreateBucket(ctx, create); err != nil {
		t.Fatalf("create isolated test bucket: %v", err)
	}
	// Cleanup only this newly created bucket and these exact test-owned keys.
	keys := []string{"repos/test/lfs/data/complete", "repos/test/lfs/data/abort", "repos/test/lfs/data/empty"}
	uploads := make(map[string]string)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cleanupCancel()
		for key, id := range uploads {
			_, err := client.AbortMultipartUpload(cleanupCtx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id)})
			if err != nil {
				if api, ok := errors.AsType[smithy.APIError](err); !ok || api.ErrorCode() != "NoSuchUpload" {
					t.Errorf("cleanup multipart: %v", err)
				}
			}
		}
		for _, key := range keys {
			if _, err := client.DeleteObject(cleanupCtx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
				t.Errorf("cleanup object: %v", err)
			}
		}
		if _, err := client.DeleteBucket(cleanupCtx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Errorf("cleanup isolated bucket: %v", err)
		}
	})
	store, err := New(client, bucket)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateMultipart(ctx, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	uploads[keys[0]] = id
	if _, err := store.Head(ctx, keys[0]); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unfinished upload visible: %v", err)
	}
	payload := bytes.Repeat([]byte("lfs-stream"), int(storage.MinMultipartPartSize)/10+1)
	_, err = store.UploadPart(ctx, keys[0], id, 1, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	// Replaying a successful part is safe and must still produce complete bytes.
	first, err := store.UploadPart(ctx, keys[0], id, 1, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.UploadPart(ctx, keys[0], id, 2, strings.NewReader("tail"), 4)
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.CompleteMultipart(ctx, keys[0], id, []storage.MultipartPart{first, second})
	if err != nil {
		t.Fatal(err)
	}
	delete(uploads, keys[0])
	if info.Size != int64(len(payload)+4) || info.Version == "" {
		t.Fatalf("completed metadata: %+v", info)
	}
	body, readInfo, err := store.Get(ctx, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, append(payload, []byte("tail")...)) || readInfo.Size != info.Size || readInfo.Version != info.Version {
		t.Fatal("multipart bytes or metadata differ")
	}
	body, _, err = store.GetRange(ctx, keys[0], int64(len(payload)), 4)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr = io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil || string(got) != "tail" {
		t.Fatalf("range: %q, %v", got, err)
	}
	id, err = store.CreateMultipart(ctx, keys[1])
	if err != nil {
		t.Fatal(err)
	}
	uploads[keys[1]] = id
	_, err = client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(keys[1]), UploadId: aws.String(id), PartNumber: aws.Int32(1),
		Body: strings.NewReader("corrupt-checksum"), ContentLength: aws.Int64(16), ContentMD5: aws.String("AAAAAAAAAAAAAAAAAAAAAA=="),
	})
	if api, ok := errors.AsType[smithy.APIError](err); !ok || api.ErrorCode() != "BadDigest" {
		t.Fatalf("provider accepted corrupt Content-MD5 or returned unexpected error: %v", err)
	}
	if _, err := store.UploadPart(ctx, keys[1], id, 1, strings.NewReader("abandoned"), 9); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortMultipart(ctx, keys[1], id); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortMultipart(ctx, keys[1], id); err != nil {
		t.Fatal(err)
	}
	delete(uploads, keys[1])
	if _, err := store.Head(ctx, keys[1]); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("aborted upload visible: %v", err)
	}
	id, err = store.CreateMultipart(ctx, keys[2])
	if err != nil {
		t.Fatal(err)
	}
	uploads[keys[2]] = id
	emptyPart, err := store.UploadPart(ctx, keys[2], id, 1, bytes.NewReader(nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	emptyInfo, err := store.CompleteMultipart(ctx, keys[2], id, []storage.MultipartPart{emptyPart})
	if err != nil || emptyInfo.Size != 0 {
		t.Fatalf("empty object completion: %+v, %v", emptyInfo, err)
	}
	delete(uploads, keys[2])
	if emptyInfo, err := store.Head(ctx, keys[2]); err != nil || emptyInfo.Size != 0 {
		t.Fatalf("empty object metadata: %+v, %v", emptyInfo, err)
	}
}
