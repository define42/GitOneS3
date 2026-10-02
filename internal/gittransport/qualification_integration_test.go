//go:build integration

package gittransport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/define42/GitOneS3/internal/metrics"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
	"github.com/define42/GitOneS3/internal/storage/s3store"
)

// TestS3GitQualification is an opt-in deployment experiment. It always creates
// and removes a randomly named bucket; no existing shard bucket can be selected.
// Latency includes the actual S3 adapter and production HTTP handler. The local
// client bypasses authentication, ingress and SSH, which have separate native
// integration coverage and must also be tested in the deployment.
func TestS3GitQualification(t *testing.T) {
	if os.Getenv("GITONE_QUALIFY_S3") != "1" {
		t.Skip("set GITONE_QUALIFY_S3=1 and GITONE_TEST_S3_ENDPOINT for isolated S3 qualification")
	}
	clients := qualificationInt(t, "GITONE_QUALIFY_CLIENTS", 1, 1, 32)
	active := qualificationInt(t, "GITONE_GIT_MAX_CONCURRENT_OPERATIONS", 1, 1, 32)
	queued := qualificationInt(t, "GITONE_GIT_MAX_QUEUED_OPERATIONS", 4, 0, 1024)
	rounds := qualificationInt(t, "GITONE_QUALIFY_ROUNDS", 3, 1, 100)
	size := qualificationInt(t, "GITONE_QUALIFY_MIB", 63, 1, 63)
	maxSeconds := qualificationInt(t, "GITONE_QUALIFY_MAX_SECONDS", 90, 1, 90)
	queueTimeout := 5 * time.Second
	if value := os.Getenv("GITONE_GIT_QUEUE_TIMEOUT"); value != "" {
		var err error
		queueTimeout, err = time.ParseDuration(value)
		if err != nil {
			t.Fatal("invalid GITONE_GIT_QUEUE_TIMEOUT")
		}
	}
	objects, bucket := qualificationS3(t)
	measured, err := metrics.NewStore(objects)
	if err != nil {
		t.Fatal(err)
	}
	store, err := repository.New(measured)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(store, Options{
		MaxConcurrentOperations: active, MaxQueuedOperations: queued, QueueTimeout: queueTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(WithWriteAuthorization(r.Context(), func(context.Context) error { return nil })))
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 95 * time.Second

	// Fixture creation and encoding are outside the measured request windows.
	snapshot, old, head := benchmarkSnapshot(size)
	pack, err := encodePack(t.Context(), snapshot.Objects)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	request := append([]byte(pkt(zeroID+" "+head+" refs/heads/main\x00report-status\n")+"0000"), pack...)
	writeProfileFile(t, filepath.Join(root, "push.bin"), request)
	runtime.GC()
	fixture := memoryFixture{Old: old, Head: head, Bytes: int64(size) << 20}
	for index := range clients {
		if _, err := store.Create(t.Context(), "perf", repository.CreateInput{
			Name: fmt.Sprintf("repo-%d", index), CreatedBy: "qualification",
		}); err != nil {
			t.Fatal(err)
		}
	}

	var samples []qualificationSample
	run := func(operation string, round int) {
		t.Helper()
		batch := make([]qualificationSample, clients)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for index := range clients {
			workers.Go(func() {
				<-start
				began := time.Now()
				count, err := profileHTTPRequest(t.Context(), client, server.URL, root, operation, index, fixture)
				batch[index] = qualificationSample{Operation: operation, Round: round, Client: index, Seconds: time.Since(began).Seconds(), ResponseBytes: count}
				if err != nil {
					batch[index].Error = err.Error()
				}
			})
		}
		close(start)
		workers.Wait()
		samples = append(samples, batch...)
		for _, sample := range batch {
			if sample.Error != "" {
				t.Errorf("%s client %d round %d: %s", operation, sample.Client, round, sample.Error)
			}
			if sample.Seconds > float64(maxSeconds) {
				t.Errorf("%s client %d exceeded %ds latency budget: %.3fs", operation, sample.Client, maxSeconds, sample.Seconds)
			}
		}
	}
	artifact := t.ArtifactDir()
	defer func() {
		// Persist evidence even when a load or recovery assertion fails.
		report := map[string]any{
			"go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "bucket": bucket,
			"fixture_mib": size, "clients": clients, "active_limit": active, "queued_limit": queued,
			"queue_timeout_seconds": queueTimeout.Seconds(), "max_seconds": maxSeconds, "samples": samples,
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		writeProfileFile(t, filepath.Join(artifact, "qualification.json"), data)
		w := httptest.NewRecorder()
		measured.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/system/metrics", nil))
		writeProfileFile(t, filepath.Join(artifact, "storage.prom"), w.Body.Bytes())
		t.Logf("qualification artifacts: %s", artifact)
	}()

	run("push", 0)
	if t.Failed() {
		return
	}
	for round := range rounds {
		run("clone", round)
		run("incremental", round)
	}
	for _, operation := range []string{"push", "clone", "incremental"} {
		var durations []float64
		for _, sample := range samples {
			if sample.Operation == operation {
				durations = append(durations, sample.Seconds)
			}
		}
		slices.Sort(durations)
		middle := len(durations) / 2
		median := durations[middle]
		if len(durations)%2 == 0 {
			median = (durations[middle-1] + median) / 2
		}
		t.Logf("%s: %d requests, min %.3fs, median %.3fs, max %.3fs",
			operation, len(durations), durations[0], median, durations[len(durations)-1])
	}
	// Closing the only serving handler makes the lock-recovery acknowledgement
	// true in this isolated fixture. There is no automatic unlock on a live shard.
	server.Close()
	qualificationRecovery(t, objects, store, head)
	if mode := os.Getenv("GITONE_QUALIFY_CAPACITY"); mode != "" {
		if mode != "bytes" && mode != "objects" && mode != "both" {
			t.Fatal("GITONE_QUALIFY_CAPACITY must be bytes, objects or both")
		}
		for _, candidate := range []string{"bytes", "objects"} {
			if mode == candidate || mode == "both" {
				t.Run("capacity-"+candidate, func(t *testing.T) { runNativeGitCapacity(t, objects, candidate) })
			}
		}
	}
}

type qualificationSample struct {
	Operation     string
	Round         int
	Client        int
	Seconds       float64
	ResponseBytes int64
	Error         string
}

func qualificationInt(t *testing.T, name string, fallback, minimum, maximum int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		t.Fatalf("%s must be %d..%d", name, minimum, maximum)
	}
	return value
}

func qualificationRecovery(t *testing.T, objects storage.ObjectStore, original *repository.Store, head string) {
	t.Helper()
	generations, err := original.ListGenerations(t.Context(), "perf", "repo-0")
	if err != nil || len(generations) == 0 {
		t.Fatalf("list recovery snapshots: %v", err)
	}
	source := generations[len(generations)-1].Snapshot
	metadata, err := original.Get(t.Context(), "perf", "repo-0")
	if err != nil {
		t.Fatal(err)
	}
	// Persist the same record a writer leaves if the process dies before unlock.
	token := strings.Repeat("a", 32)
	lock, err := json.Marshal(repository.MaintenanceLock{Token: token, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Put(t.Context(), "repos/"+metadata.ID+"/maintenance-lock", bytes.NewReader(lock), int64(len(lock)), storage.PutOptions{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	restarted, err := repository.New(objects)
	if err != nil {
		t.Fatal(err)
	}
	current, err := restarted.ReadGitReferences(t.Context(), "perf", "repo-0")
	if err != nil || current.References["refs/heads/main"] != head {
		t.Fatalf("restart lost published state: %v", err)
	}
	update := []repository.RefUpdate{{Name: "refs/tags/recovered", New: head}}
	authorize := func(context.Context) error { return nil }
	if err := restarted.PublishGit(t.Context(), current, update, nil, authorize); !errors.Is(err, repository.ErrMaintenanceBusy) {
		t.Fatalf("stale lock failed to fence writers: %v", err)
	}
	if err := restarted.UnlockMaintenance(t.Context(), "perf", "repo-0", token, false); !errors.Is(err, repository.ErrInvalid) {
		t.Fatalf("recovery accepted a live writer acknowledgement: %v", err)
	}
	if err := restarted.UnlockMaintenance(t.Context(), "perf", "repo-0", token, true); err != nil {
		t.Fatal(err)
	}
	if err := restarted.PublishGit(t.Context(), current, update, nil, authorize); err != nil {
		t.Fatalf("writer did not recover: %v", err)
	}
	if _, err := restarted.RestoreGeneration(t.Context(), "perf", "repo-0", source); err != nil {
		t.Fatalf("retained generation restore: %v", err)
	}
	restored, err := restarted.ReadGitReferences(t.Context(), "perf", "repo-0")
	if err != nil || restored.References["refs/heads/main"] != head || restored.References["refs/tags/recovered"] != "" {
		t.Fatalf("restore did not publish the chosen snapshot: %v", err)
	}
	if _, err := restarted.CheckIntegrity(t.Context(), "perf", "repo-0"); err != nil {
		t.Fatalf("restored repository integrity: %v", err)
	}
	t.Log("S3 restart, write fencing, offline lock recovery, generation restore and integrity passed")
}

func qualificationS3(t *testing.T) (*s3store.Store, string) {
	t.Helper()
	endpoint := os.Getenv("GITONE_TEST_S3_ENDPOINT")
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		t.Fatal("GITONE_TEST_S3_ENDPOINT must be an HTTP(S) origin without credentials, query or fragment")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(), awsconfig.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
	})
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "gitone-qualify-" + hex.EncodeToString(random[:])
	input := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	if _, err := client.CreateBucket(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	t.Logf("isolated qualification bucket: %s", bucket)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				t.Errorf("cleanup isolated bucket %s: %v", bucket, err)
				return
			}
			for _, object := range page.Contents {
				if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key}); err != nil {
					t.Errorf("cleanup isolated bucket %s: %v", bucket, err)
					return
				}
			}
		}
		if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Errorf("remove isolated bucket %s: %v", bucket, err)
		}
	})
	objects, err := s3store.New(client, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.Check(t.Context()); err != nil {
		t.Fatalf("S3 conditional/read/list capability probe: %v", err)
	}
	return objects, bucket
}
