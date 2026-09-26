//go:build integration

package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/define42/GitOneS3/internal/gitpack"
	"github.com/define42/GitOneS3/internal/storage/s3store"
)

// TestS3RepositoryMaintenance creates its own random bucket. It never uses an
// existing shard bucket. Opt in with GITONE_TEST_S3_ENDPOINT and normal AWS SDK
// credentials; the credentials must permit creating and deleting test buckets.
func TestS3RepositoryMaintenance(t *testing.T) {
	endpoint := os.Getenv("GITONE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GITONE_TEST_S3_ENDPOINT to run against an S3-compatible provider")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("GITONE_TEST_S3_ENDPOINT must be an HTTP(S) endpoint without credentials, query, or fragment")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatalf("load test S3 configuration: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
	})
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "gitone-test-" + hex.EncodeToString(random[:])
	create := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if region != "us-east-1" {
		create.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	if _, err := client.CreateBucket(ctx, create); err != nil {
		t.Fatalf("create isolated test bucket: %v", err)
	}
	// Register cleanup only after successful creation. No existing bucket name
	// can be supplied by configuration or discovered from a deployment.
	t.Cleanup(func() { cleanupS3TestBucket(t, client, bucket) })
	objects, err := s3store.New(client, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.Check(ctx); err != nil {
		t.Fatalf("conditional PUT/DELETE capability probe: %v", err)
	}
	store, err := New(objects)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Create(ctx, "alice", CreateInput{
		Name: "demo", CreatedBy: "integration-test", InitializeReadme: true,
		AuthorName: "Alice", AuthorEmail: "alice@example.test",
	})
	if err != nil {
		t.Fatalf("create repository: %v", err)
	}
	generations, err := store.ListGenerations(ctx, "alice", "demo")
	if err != nil || len(generations) != 1 || !generations[0].Current {
		t.Fatalf("initial generations = %+v, error %v", generations, err)
	}
	restoreSource := generations[0].Snapshot
	pinned, err := store.ReadGitReferences(ctx, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	initialHead := pinned.References["refs/heads/main"]
	payload := gitpack.Object{Type: "blob", Data: bytes.Repeat([]byte("S3 packed object\n"), 140000)}
	payloadID := GitObjectID(GitObject{Type: payload.Type, Data: payload.Data})
	incoming := packedWorkspace(t, []string{payloadID}, func(context.Context, string) (gitpack.Object, error) { return payload, nil })
	if err := store.PublishPack(ctx, pinned, []RefUpdate{{Name: "refs/tags/payload", New: payloadID}}, incoming, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("publish packed object: %v", err)
	}
	if err := incoming.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := store.CheckIntegrity(ctx, "alice", "demo")
	if err != nil || report.Generation != 2 || report.Objects != 4 || report.Packs != 1 {
		t.Fatalf("packed integrity = %+v, error %v", report, err)
	}
	packed, err := store.ReadGitReferences(ctx, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	// Direct object access exercises the provider's actual Content-Range and
	// ETag handling, alongside the full-pack download used by integrity checks.
	read, err := store.object(ctx, packed.original, payloadID, "blob")
	if err != nil || !bytes.Equal(read, payload.Data) {
		t.Fatalf("read packed object through S3 range: %v", err)
	}
	restored, err := store.RestoreGeneration(ctx, "alice", "demo", restoreSource)
	if err != nil || restored.SourceGeneration != 1 || restored.Generation != 3 {
		t.Fatalf("restore report = %+v, error %v", restored, err)
	}
	current, err := store.ReadGitReferences(ctx, "alice", "demo")
	if err != nil || current.References["refs/heads/main"] != initialHead || current.References["refs/tags/payload"] != "" {
		t.Fatalf("restored references = %+v, error %v", current, err)
	}
	if err := store.Repack(ctx, "alice", "demo"); err != nil {
		t.Fatalf("repack: %v", err)
	}
	report, err = store.CheckIntegrity(ctx, "alice", "demo")
	if err != nil || report.Generation != 4 || report.Objects != 3 || report.Packs != 1 {
		t.Fatalf("repacked integrity = %+v, error %v", report, err)
	}
	generations, err = store.ListGenerations(ctx, "alice", "demo")
	if err != nil || len(generations) != 4 || !generations[3].Current {
		t.Fatalf("retained generations = %+v, error %v", generations, err)
	}
	current, err = store.ReadGitReferences(ctx, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	packPrefix := "repos/" + metadata.ID + "/packs/"
	before, err := objects.List(ctx, packPrefix)
	if err != nil {
		t.Fatal(err)
	}
	rejected := gitpack.Object{Type: "blob", Data: []byte("S3 upload rejected after authorization was revoked")}
	rejectedID := GitObjectID(GitObject{Type: rejected.Type, Data: rejected.Data})
	incoming = packedWorkspace(t, []string{rejectedID}, func(context.Context, string) (gitpack.Object, error) { return rejected, nil })
	checked := false
	err = store.PublishPack(ctx, current, []RefUpdate{{Name: "refs/tags/rejected", New: rejectedID}}, incoming, func(ctx context.Context) error {
		checked = true
		packs, err := objects.List(ctx, packPrefix)
		if err != nil {
			return err
		}
		if len(packs) != len(before)+1 {
			t.Errorf("authorization ran before the new pack was durable: before %d, after %d", len(before), len(packs))
		}
		return errors.New("authorization revoked")
	})
	if !checked || !errors.Is(err, ErrForbidden) {
		t.Fatalf("rejected publication = %v, authorization checked = %v", err, checked)
	}
	if err := incoming.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadGitReferences(ctx, "alice", "demo")
	if err != nil || after.original.version != current.original.version || after.References["refs/tags/rejected"] != "" {
		t.Fatalf("rejected push changed published references: %v", err)
	}
	preview, err := store.GarbageCollect(ctx, "alice", "demo", GCOptions{GracePeriod: time.Nanosecond})
	if err != nil || !preview.DryRun || preview.Candidates < 3 || preview.Deleted != 0 || preview.RetainedGenerations != 4 {
		t.Fatalf("orphan preview = %+v, error %v", preview, err)
	}
	collected, err := store.GarbageCollect(ctx, "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || collected.DryRun || collected.Deleted != preview.Candidates || collected.DeletedBytes != preview.CandidateBytes {
		t.Fatalf("orphan collection = %+v, error %v", collected, err)
	}
	packs, err := objects.List(ctx, packPrefix)
	if err != nil || len(packs) != len(before) {
		t.Fatalf("packs after collection = %d, want %d, error %v", len(packs), len(before), err)
	}
	if _, err := store.LoadGitObjects(ctx, pinned); err != nil {
		t.Fatalf("collection invalidated the original pinned reader: %v", err)
	}
	read, err = store.object(ctx, packed.original, payloadID, "blob")
	if err != nil || !bytes.Equal(read, payload.Data) {
		t.Fatalf("collection removed a retained packed generation: %v", err)
	}
	report, err = store.CheckIntegrity(ctx, "alice", "demo")
	if err != nil || report.Generation != 4 || report.Objects != 3 {
		t.Fatalf("final integrity = %+v, error %v", report, err)
	}
	remaining, err := store.GarbageCollect(ctx, "alice", "demo", GCOptions{GracePeriod: time.Nanosecond})
	if err != nil || remaining.Candidates != 0 {
		t.Fatalf("remaining orphan candidates = %+v, error %v", remaining, err)
	}
}

func cleanupS3TestBucket(t *testing.T, client *s3.Client, bucket string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Errorf("list isolated test bucket during cleanup: %v", err)
			return
		}
		for _, object := range page.Contents {
			if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key}); err != nil {
				t.Errorf("delete isolated test object: %v", err)
			}
		}
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Error(fmt.Errorf("delete isolated test bucket %s: %w", bucket, err))
	}
}
