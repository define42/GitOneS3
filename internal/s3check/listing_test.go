package s3check

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/define42/GitOneS3/internal/storage"
)

type listingResponse struct {
	XMLName   xml.Name         `xml:"ListBucketResult"`
	Truncated *bool            `xml:"IsTruncated,omitempty"`
	NextToken string           `xml:"NextContinuationToken,omitempty"`
	Objects   []listingContent `xml:"Contents"`
}

type listingContent struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	ETag         string `xml:"ETag"`
	LastModified string `xml:"LastModified"`
}

func TestListAllPagination(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"v1", "v2", "v2-start-after"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, expected, requests := listingFixture(t, mode, nil)
			if err := s.listAll(t.Context(), "probe/listing/", expected, mode, "", 2); err != nil {
				t.Fatal(err)
			}
			if got := requests.Load(); got != 3 {
				t.Fatalf("requests = %d, want 3", got)
			}
		})
	}
}

func TestListAllRejectsIncompleteListing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mode   string
		mutate func(int, *listingResponse)
		want   string
	}{
		{
			name: "missing key", mode: "v1", want: "want \"probe/listing/000000\"",
			mutate: func(page int, response *listingResponse) {
				if page == 0 {
					response.Objects = response.Objects[1:]
				}
			},
		},
		{
			name: "ignored marker", mode: "v1", want: "does not advance",
			mutate: func(page int, response *listingResponse) {
				if page == 1 {
					response.Objects[0].Key = "probe/listing/000000"
				}
			},
		},
		{
			name: "duplicate key", mode: "v2-start-after", want: "does not advance",
			mutate: func(_ int, response *listingResponse) {
				response.Objects[1] = response.Objects[0]
			},
		},
		{
			name: "false end", mode: "v1", want: "ended after 2 objects, want 5",
			mutate: func(_ int, response *listingResponse) {
				response.Truncated = aws.Bool(false)
			},
		},
		{
			name: "missing truncation", mode: "v1", want: "missing IsTruncated",
			mutate: func(_ int, response *listingResponse) {
				response.Truncated = nil
			},
		},
		{
			name: "empty truncated", mode: "v2", want: "empty truncated page",
			mutate: func(_ int, response *listingResponse) {
				response.Objects = nil
			},
		},
		{
			name: "missing token", mode: "v2", want: "missing or repeated continuation token",
			mutate: func(_ int, response *listingResponse) {
				response.NextToken = ""
			},
		},
		{
			name: "repeated token", mode: "v2", want: "missing or repeated continuation token",
			mutate: func(_ int, response *listingResponse) {
				response.NextToken = "cursor:2"
			},
		},
		{
			name: "stale etag", mode: "v1", want: "missing or stale",
			mutate: func(_ int, response *listingResponse) {
				response.Objects[0].ETag = `"old"`
			},
		},
		{
			name: "stale size", mode: "v1", want: "missing or stale",
			mutate: func(_ int, response *listingResponse) {
				response.Objects[0].Size++
			},
		},
		{
			name: "missing modification time", mode: "v1", want: "missing or stale",
			mutate: func(_ int, response *listingResponse) {
				response.Objects[0].LastModified = "0001-01-01T00:00:00Z"
			},
		},
		{
			name: "wrong prefix", mode: "v1", want: "outside prefix",
			mutate: func(_ int, response *listingResponse) {
				response.Objects[0].Key = "outside/object"
			},
		},
		{
			name: "oversized page", mode: "v1", want: "for MaxKeys=2",
			mutate: func(_ int, response *listingResponse) {
				response.Objects = append(response.Objects, response.Objects[0])
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s, expected, _ := listingFixture(t, test.mode, test.mutate)
			err := s.listAll(t.Context(), "probe/listing/", expected, test.mode, "", 2)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestListAllDeletedMarker(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"v1", "v2-start-after"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, expected, _ := listingFixture(t, mode, nil)
			if err := s.listAll(t.Context(), "probe/listing/", expected[2:], mode, expected[1].Key, 2); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func listingFixture(
	t *testing.T,
	mode string,
	mutate func(int, *listingResponse),
) (*suite, []storage.ObjectInfo, *atomic.Int32) {
	t.Helper()
	expected := make([]storage.ObjectInfo, 5)
	for i := range expected {
		expected[i] = storage.ObjectInfo{
			Key: fmt.Sprintf("probe/listing/%06d", i), Size: 7, Version: `"etag"`,
		}
	}
	requests := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := int(requests.Add(1)) - 1
		query := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/test-bucket" ||
			query.Get("prefix") != "probe/listing/" || query.Get("max-keys") != "2" ||
			query.Has("delimiter") {
			t.Errorf("unexpected list request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		cursor := ""
		switch mode {
		case "v1":
			if query.Has("list-type") || query.Has("start-after") || query.Has("continuation-token") {
				t.Errorf("unexpected v2 parameters: %s", r.URL)
			}
			cursor = query.Get("marker")
		case "v2":
			if query.Get("list-type") != "2" || query.Has("start-after") || query.Has("marker") {
				t.Errorf("incorrect v2 continuation request: %s", r.URL)
			}
			cursor = query.Get("continuation-token")
		case "v2-start-after":
			if query.Get("list-type") != "2" || query.Has("continuation-token") || query.Has("marker") {
				t.Errorf("incorrect v2 start-after request: %s", r.URL)
			}
			cursor = query.Get("start-after")
		}
		start := 0
		if cursor != "" {
			if mode == "v2" {
				var err error
				start, err = strconv.Atoi(strings.TrimPrefix(cursor, "cursor:"))
				if err != nil {
					t.Errorf("invalid continuation token %q", cursor)
				}
			} else {
				for start < len(expected) && expected[start].Key <= cursor {
					start++
				}
			}
		}
		end := min(start+2, len(expected))
		response := listingResponse{Truncated: aws.Bool(end < len(expected))}
		if end < len(expected) && mode == "v2" {
			response.NextToken = fmt.Sprintf("cursor:%d", end)
		}
		for _, object := range expected[start:end] {
			response.Objects = append(response.Objects, listingContent{
				Key: object.Key, Size: object.Size, ETag: string(object.Version),
				LastModified: "2026-09-26T10:00:00Z",
			})
		}
		if mutate != nil {
			mutate(page, &response)
		}
		w.Header().Set("Content-Type", "application/xml")
		if err := xml.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode listing: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(server.URL), UsePathStyle: true,
		Credentials: aws.AnonymousCredentials{}, RetryMaxAttempts: 1,
	})
	return &suite{client: client, bucket: "test-bucket", prefix: "probe/"}, expected, requests
}
