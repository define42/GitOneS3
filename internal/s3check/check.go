// Package s3check probes the S3 semantics required by GitOne using isolated test objects.
// Successful observations cannot prove a provider's consistency guarantees.
package s3check

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/define42/GitOneS3/internal/storage"
	"github.com/define42/GitOneS3/internal/storage/s3store"
)

// Options selects an existing, never-versioned bucket and the listing APIs to probe.
// Prefix is a parent prefix; every run appends a random, private subdirectory.
type Options struct {
	Bucket  string
	Prefix  string
	ListAPI string
	Objects int
	// OnStart is called with the isolated prefix before the first request.
	OnStart func(prefix string) error
}

var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// Validate rejects invalid options before the tool makes any network requests.
func (o Options) Validate() error {
	if !bucketPattern.MatchString(o.Bucket) || strings.Contains(o.Bucket, "..") {
		return errors.New("a valid existing bucket name is required")
	}
	if err := storage.ValidatePrefix(o.Prefix); err != nil {
		return fmt.Errorf("test prefix: %w", err)
	}
	if len(o.Prefix) > 800 {
		return errors.New("test prefix must be at most 800 bytes")
	}
	if o.ListAPI != "v1" && o.ListAPI != "v2" && o.ListAPI != "both" {
		return errors.New("list api must be v1, v2, or both")
	}
	if o.Objects < 1001 || o.Objects > 10000 {
		return errors.New("objects must be between 1001 and 10000 to exercise pagination")
	}
	return nil
}

// Result records one check. Status is pass, fail, or skip.
type Result struct {
	Name     string        `json:"name"`
	Status   string        `json:"status"`
	Detail   string        `json:"detail,omitempty"`
	Duration time.Duration `json:"durationNs"`
}

// Report describes the selected checks, including cleanup. Passed means all
// selected checks completed successfully, not that provider guarantees are proven.
type Report struct {
	Bucket  string   `json:"bucket"`
	Prefix  string   `json:"prefix"`
	ListAPI string   `json:"listAPI"`
	Results []Result `json:"results"`
	Passed  bool     `json:"passed"`
}

type pendingUpload struct {
	key string
	id  string
}

type suite struct {
	client  *s3.Client
	store   *s3store.Store
	bucket  string
	prefix  string
	objects int
	report  Report
	mu      sync.Mutex
	keys    map[string]struct{}
	uploads []pendingUpload
}

// Run checks an endpoint without reading or changing existing objects. It never
// creates/deletes buckets. Cleanup uses recorded keys, independent of listing
// compatibility, and gets its own bounded context even after cancellation.
func Run(ctx context.Context, client *s3.Client, opts Options) (Report, error) {
	if err := opts.Validate(); err != nil {
		return Report{}, err
	}
	if client == nil {
		return Report{}, errors.New("s3 client is required")
	}
	objectStore, err := s3store.New(client, opts.Bucket)
	if err != nil {
		return Report{}, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return Report{}, fmt.Errorf("create test prefix: %w", err)
	}
	prefix := strings.TrimSuffix(opts.Prefix, "/") + "/" + hex.EncodeToString(token[:]) + "/"
	s := &suite{
		client: client, store: objectStore, bucket: opts.Bucket, prefix: prefix,
		objects: opts.Objects, keys: make(map[string]struct{}),
		report: Report{Bucket: opts.Bucket, Prefix: prefix, ListAPI: opts.ListAPI},
	}
	if opts.OnStart != nil {
		if err := opts.OnStart(prefix); err != nil {
			return s.report, fmt.Errorf("report test prefix: %w", err)
		}
	}
	s.check(ctx, "bucket_access", func(ctx context.Context) error {
		_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(opts.Bucket)})
		return err
	})
	if s.report.Results[0].Status != "pass" {
		return s.finish()
	}
	s.check(ctx, "unversioned_test_bucket", func(ctx context.Context) error {
		output, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(opts.Bucket)})
		if err != nil {
			return fmt.Errorf("check versioning before writing test objects: %w", err)
		}
		if output == nil || output.Status != "" {
			return errors.New("use a test bucket where versioning has never been enabled; no objects were written")
		}
		return nil
	})
	if s.report.Results[1].Status != "pass" {
		return s.finish()
	}
	s.check(ctx, "object_read_write_range", s.basic)
	s.check(ctx, "conditional_create", s.conditionalCreate)
	s.check(ctx, "conditional_update", s.conditionalUpdate)
	s.check(ctx, "conditional_delete", s.conditionalDelete)
	s.check(ctx, "concurrent_create", func(ctx context.Context) error { return s.competingWrites(ctx, true) })
	s.check(ctx, "concurrent_update", func(ctx context.Context) error { return s.competingWrites(ctx, false) })
	s.check(ctx, "concurrent_update_delete", s.competingDelete)
	s.multipart(ctx)
	s.listing(ctx, opts.ListAPI)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	s.check(cleanupCtx, "cleanup", s.cleanup)
	return s.finish()
}

func (s *suite) check(ctx context.Context, name string, probe func(context.Context) error) {
	result := Result{Name: name, Status: "pass"}
	started := time.Now()
	if err := ctx.Err(); err != nil {
		result.Status, result.Detail = "skip", err.Error()
	} else if err := probe(ctx); err != nil {
		result.Status, result.Detail = "fail", err.Error()
	}
	result.Duration = time.Since(started)
	s.report.Results = append(s.report.Results, result)
}

func (s *suite) finish() (Report, error) {
	s.report.Passed = len(s.report.Results) > 0
	for _, result := range s.report.Results {
		if result.Status != "pass" {
			s.report.Passed = false
		}
	}
	if !s.report.Passed {
		return s.report, errors.New("one or more selected checks failed or could not run; see report")
	}
	return s.report, nil
}

func (s *suite) key(suffix string) string {
	key := s.prefix + suffix
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = make(map[string]struct{})
	}
	s.keys[key] = struct{}{}
	return key
}

func (s *suite) put(ctx context.Context, key string, body []byte, opts storage.PutOptions) (storage.ObjectInfo, error) {
	return s.store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), opts)
}

func (s *suite) trackUpload(key, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads = append(s.uploads, pendingUpload{key: key, id: id})
}

func (s *suite) cleanup(ctx context.Context) error {
	var failures []error
	for _, upload := range s.uploads {
		if err := s.store.AbortMultipart(ctx, upload.key, upload.id); err != nil {
			failures = append(failures, fmt.Errorf("abort %q upload %q: %w", upload.key, upload.id, err))
		}
	}
	// Keep cleanup bounded even when every delete fails. Exact keys avoid any
	// dependency on the endpoint's pagination or prefix filtering correctness.
	keys := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for range 8 {
		wg.Go(func() {
			for key := range keys {
				err := s.store.Delete(ctx, key, "")
				if err == nil || errors.Is(err, storage.ErrNotFound) {
					_, err = s.store.Head(ctx, key)
					if errors.Is(err, storage.ErrNotFound) {
						continue
					}
					if err == nil {
						err = errors.New("object is still present after delete")
					}
				}
				mu.Lock()
				if len(failures) < 10 {
					failures = append(failures, fmt.Errorf("clean up %q: %w", key, err))
				}
				mu.Unlock()
			}
		})
	}
	for key := range s.keys {
		if ctx.Err() != nil {
			break
		}
		select {
		case keys <- key:
		case <-ctx.Done():
		}
	}
	close(keys)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		failures = append(failures, err)
	}
	if len(failures) != 0 {
		return fmt.Errorf("test data may remain under %q: %w", s.prefix, errors.Join(failures...))
	}
	return nil
}
