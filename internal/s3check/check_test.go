package s3check

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/define42/GitOneS3/internal/storage/s3store"
)

func TestConditionalProbes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		probe func(*suite, context.Context) error
		fault string
	}{
		{name: "create", probe: (*suite).conditionalCreate, fault: "create"},
		{name: "update", probe: (*suite).conditionalUpdate, fault: "update"},
		{name: "delete", probe: (*suite).conditionalDelete, fault: "delete"},
		{name: "concurrent create", probe: func(s *suite, ctx context.Context) error { return s.competingWrites(ctx, true) }, fault: "create"},
		{name: "concurrent update", probe: func(s *suite, ctx context.Context) error { return s.competingWrites(ctx, false) }, fault: "update"},
		{name: "concurrent update delete", probe: (*suite).competingDelete, fault: "all"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, fault := range []string{"", test.fault, "denied"} {
				t.Run("fault="+fault, func(t *testing.T) {
					t.Parallel()
					backend := &conditionalBackend{fault: fault}
					s := newConditionalFixture(t, backend)
					err := test.probe(s, t.Context())
					if (err != nil) != (fault != "") {
						t.Fatalf("fault=%q: probe error=%v", fault, err)
					}
				})
			}
		})
	}
}

func TestRunRefusesVersionedBucketBeforeWriting(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"Enabled", "Suspended"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			backend := &conditionalBackend{versioning: status}
			s := newConditionalFixture(t, backend)
			report, err := Run(t.Context(), s.client, checkOptions())
			if err == nil || report.Passed || len(report.Results) != 2 || report.Results[1].Status != "fail" {
				t.Fatalf("versioned bucket accepted: report=%+v error=%v", report, err)
			}
			if backend.writes != 0 {
				t.Fatalf("wrote %d objects to a versioned bucket", backend.writes)
			}
		})
	}
}

func TestRunCleansUpAfterCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend := &conditionalBackend{onPut: cancel}
	s := newConditionalFixture(t, backend)
	backend.objects["unrelated/user-data"] = []byte("must survive")
	report, err := Run(ctx, s.client, checkOptions())
	if err == nil || report.Passed || !strings.HasPrefix(report.Prefix, "tests/") {
		t.Fatalf("canceled report=%+v error=%v", report, err)
	}
	last := report.Results[len(report.Results)-1]
	if last.Name != "cleanup" || last.Status != "pass" {
		t.Fatalf("cleanup did not survive canceled context: %+v", last)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.objects) != 1 || string(backend.objects["unrelated/user-data"]) != "must survive" {
		t.Fatalf("cleanup touched unrelated data or left probe data: keys=%v", backend.objects)
	}
}

func TestCleanupReportsSurvivingObjects(t *testing.T) {
	t.Parallel()
	backend := &conditionalBackend{fault: "retain"}
	s := newConditionalFixture(t, backend)
	key := s.key("left-behind")
	backend.objects[key] = []byte("data")
	if err := s.cleanup(t.Context()); err == nil || !strings.Contains(err.Error(), s.prefix) {
		t.Fatalf("cleanup failure did not identify test prefix: %v", err)
	}
}

func TestBasicReadRangeAndDelete(t *testing.T) {
	t.Parallel()
	s := newConditionalFixture(t, &conditionalBackend{})
	if err := s.basic(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRunRejectsInvalidOptionsWithoutRequests(t *testing.T) {
	t.Parallel()
	backend := &conditionalBackend{}
	s := newConditionalFixture(t, backend)
	opts := checkOptions()
	opts.Prefix = "../user-data"
	if _, err := Run(t.Context(), s.client, opts); err == nil {
		t.Fatal("invalid prefix accepted")
	}
	if backend.requests != 0 {
		t.Fatal("invalid options reached the endpoint")
	}
}

func checkOptions() Options {
	return Options{Bucket: "test-bucket", Prefix: "tests/", ListAPI: "both", Objects: 1005}
}

func newConditionalFixture(t *testing.T, backend *conditionalBackend) *suite {
	t.Helper()
	backend.objects = make(map[string][]byte)
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	client := s3.NewFromConfig(aws.Config{
		Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, RetryMaxAttempts: 1,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	}, func(opts *s3.Options) {
		opts.BaseEndpoint = aws.String(server.URL)
		opts.UsePathStyle = true
	})
	store, err := s3store.New(client, "test-bucket")
	if err != nil {
		t.Fatal(err)
	}
	return &suite{client: client, store: store, bucket: "test-bucket", prefix: "tests/unit/"}
}

type conditionalBackend struct {
	mu         sync.Mutex
	objects    map[string][]byte
	fault      string
	versioning string
	onPut      func()
	writes     int
	requests   int
}

func (b *conditionalBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests++
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
	if r.URL.Path == "/test-bucket" || r.URL.Path == "/test-bucket/" {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Query().Has("versioning") {
			b.write(w, "<VersioningConfiguration><Status>"+b.versioning+"</Status></VersioningConfiguration>")
			return
		}
	}
	data, exists := b.objects[key]
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256(data))
	if b.fault == "denied" {
		b.fail(w, http.StatusForbidden, "AccessDenied")
		return
	}
	switch r.Method {
	case http.MethodPut:
		if r.Header.Get("If-None-Match") == "*" && exists && b.fault != "create" && b.fault != "all" {
			b.fail(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if match := r.Header.Get("If-Match"); match != "" && (!exists || match != etag) && b.fault != "update" && b.fault != "all" {
			b.fail(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		if err != nil {
			b.fail(w, http.StatusBadRequest, "InvalidRequest")
			return
		}
		b.objects[key] = body
		b.writes++
		w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(body)))
		if b.onPut != nil {
			b.onPut()
		}
	case http.MethodDelete:
		if match := r.Header.Get("If-Match"); match != "" && (!exists || match != etag) && b.fault != "delete" && b.fault != "all" {
			b.fail(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if b.fault != "retain" {
			delete(b.objects, key)
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodHead, http.MethodGet:
		if !exists {
			b.fail(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", etag)
		if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
			var start, end int
			if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil || start < 0 || end >= len(data) || start > end {
				b.fail(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			data = data[start : end+1]
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		}
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, bytes.NewReader(data))
		}
	default:
		b.fail(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func (b *conditionalBackend) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	b.write(w, "<Error><Code>"+code+"</Code></Error>")
}

func (b *conditionalBackend) write(w http.ResponseWriter, value string) {
	_, _ = io.WriteString(w, value)
}

func TestCheckRecordsSkippedChecks(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s := &suite{}
	s.check(ctx, "canceled", func(context.Context) error {
		t.Fatal("canceled check executed")
		return errors.New("unreachable")
	})
	report, err := s.finish()
	if err == nil || report.Passed || report.Results[0].Status != "skip" {
		t.Fatalf("skipped check was reported as passed: %+v %v", report, err)
	}
}
