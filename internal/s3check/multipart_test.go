package s3check

import (
	"bytes"
	"crypto/md5" // #nosec G501 -- Test verifies the S3 transport checksum.
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
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

func TestMultipartComplete(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		ignoreRetry bool
		wrongETag   bool
		badRange    bool
		wantError   bool
	}{
		{name: "complete and replace part"},
		{name: "provider ignores replacement", ignoreRetry: true, wantError: true},
		{name: "read ETag disagrees", wrongETag: true, wantError: true},
		{name: "range is corrupt", badRange: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := &multipartTestBackend{ignoreRetry: test.ignoreRetry, wrongETag: test.wrongETag, badRange: test.badRange}
			s := newMultipartTestSuite(t, backend)
			err := s.multipartComplete(t.Context())
			if (err != nil) != test.wantError {
				t.Fatalf("multipartComplete() = %v, want error %v", err, test.wantError)
			}
		})
	}
}

func TestMultipartChecksum(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		code      string
		accept    bool
		wantError bool
	}{
		{name: "bad digest", code: "BadDigest"},
		{name: "invalid digest", code: "InvalidDigest"},
		{name: "unrelated failure", code: "AccessDenied", wantError: true},
		{name: "checksum ignored", accept: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := &multipartTestBackend{checksumCode: test.code, ignoreChecksum: test.accept}
			s := newMultipartTestSuite(t, backend)
			err := s.multipartChecksum(t.Context())
			if (err != nil) != test.wantError {
				t.Fatalf("multipartChecksum() = %v, want error %v", err, test.wantError)
			}
		})
	}
}

func TestMultipartAbort(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		ignoreAbort bool
		missingCode string
		wantError   bool
	}{
		{name: "abort invalidates upload", missingCode: "NoSuchUpload"},
		{name: "unrelated not found", missingCode: "NoSuchKey", wantError: true},
		{name: "provider ignores abort", ignoreAbort: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := &multipartTestBackend{ignoreAbort: test.ignoreAbort, missingCode: test.missingCode}
			s := newMultipartTestSuite(t, backend)
			err := s.multipartAbort(t.Context())
			if (err != nil) != test.wantError {
				t.Fatalf("multipartAbort() = %v, want error %v", err, test.wantError)
			}
		})
	}
}

func TestCompareMultipartBody(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		body      string
		wantError bool
	}{
		{name: "exact", body: "abcdef"},
		{name: "short", body: "abcde", wantError: true},
		{name: "long", body: "abcdefg", wantError: true},
		{name: "corrupt", body: "abXdef", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := &multipartTestBody{Reader: strings.NewReader(test.body)}
			err := compareMultipartBody(body, []byte("abc"), []byte("def"))
			if (err != nil) != test.wantError || !body.closed {
				t.Fatalf("compareMultipartBody() = %v, closed %v, want error %v", err, body.closed, test.wantError)
			}
		})
	}
}

type multipartTestBody struct {
	*strings.Reader
	closed bool
}

func (b *multipartTestBody) Close() error {
	b.closed = true
	return nil
}

func newMultipartTestSuite(t *testing.T, backend *multipartTestBackend) *suite {
	t.Helper()
	backend.t = t
	backend.parts = make(map[int][]byte)
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(server.URL), UsePathStyle: true,
		Credentials: aws.AnonymousCredentials{}, HTTPClient: server.Client(),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	store, err := s3store.New(client, "gitone-check-test")
	if err != nil {
		t.Fatal(err)
	}
	return &suite{client: client, store: store, bucket: "gitone-check-test", prefix: "checks/test/"}
}

type multipartTestBackend struct {
	mu             sync.Mutex
	t              *testing.T
	parts          map[int][]byte
	created        bool
	aborted        bool
	object         []byte
	ignoreRetry    bool
	wrongETag      bool
	badRange       bool
	ignoreChecksum bool
	checksumCode   string
	ignoreAbort    bool
	missingCode    string
}

func (b *multipartTestBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	query := r.URL.Query()
	if r.Method == http.MethodPost && query.Has("uploads") {
		b.created = true
		b.write(w, "<InitiateMultipartUploadResult><UploadId>test-upload</UploadId></InitiateMultipartUploadResult>")
		return
	}
	if query.Get("uploadId") != "" {
		if query.Get("uploadId") != "test-upload" || !b.created || b.aborted {
			code := b.missingCode
			if code == "" {
				code = "NoSuchUpload"
			}
			b.fail(w, http.StatusNotFound, code)
			return
		}
		switch r.Method {
		case http.MethodPut:
			b.uploadPart(w, r)
		case http.MethodPost:
			b.complete(w, r)
		case http.MethodDelete:
			b.aborted = !b.ignoreAbort
			w.WriteHeader(http.StatusNoContent)
		default:
			b.fail(w, http.StatusBadRequest, "InvalidRequest")
		}
		return
	}
	if b.object == nil {
		b.fail(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	b.read(w, r)
}

func (b *multipartTestBackend) uploadPart(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, (5<<20)+1))
	if err != nil {
		b.t.Errorf("read test part: %v", err)
		b.fail(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	// #nosec G401 -- Test verifies Content-MD5, not cryptographic identity.
	digest := md5.Sum(body)
	if checksum := r.Header.Get("Content-MD5"); checksum != "" && checksum != base64.StdEncoding.EncodeToString(digest[:]) && !b.ignoreChecksum {
		code := b.checksumCode
		if code == "" {
			code = "BadDigest"
		}
		b.fail(w, http.StatusBadRequest, code)
		return
	}
	number, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil {
		b.t.Errorf("invalid part number: %v", err)
		b.fail(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	if b.parts[number] == nil || !b.ignoreRetry {
		b.parts[number] = body
	}
	w.Header().Set("ETag", multipartTestETag(body))
	w.WriteHeader(http.StatusOK)
}

func (b *multipartTestBackend) complete(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Parts []struct {
			Number int    `xml:"PartNumber"`
			ETag   string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&input); err != nil {
		b.t.Errorf("decode completion: %v", err)
		b.fail(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	b.object = []byte{}
	for _, part := range input.Parts {
		body := b.parts[part.Number]
		if body == nil || (!b.ignoreRetry && part.ETag != multipartTestETag(body)) {
			b.fail(w, http.StatusBadRequest, "InvalidPart")
			return
		}
		b.object = append(b.object, body...)
	}
	b.write(w, "<CompleteMultipartUploadResult><ETag>"+multipartTestETag(b.object)+"</ETag></CompleteMultipartUploadResult>")
}

func (b *multipartTestBackend) read(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodHead && r.Method != http.MethodGet {
		b.fail(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	etag := multipartTestETag(b.object)
	if b.wrongETag {
		etag = `"wrong"`
	}
	w.Header().Set("ETag", etag)
	body := b.object
	status := http.StatusOK
	if requested := r.Header.Get("Range"); requested != "" {
		var start, end int
		if _, err := fmt.Sscanf(requested, "bytes=%d-%d", &start, &end); err != nil || start < 0 || end >= len(body) || start > end {
			b.fail(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		body = body[start : end+1]
		if b.badRange {
			body = bytes.Repeat([]byte{'x'}, len(body))
		}
		status = http.StatusPartialContent
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		if _, err := w.Write(body); err != nil {
			// A probe closes corrupt responses immediately, so this larger
			// response may still be writing when the client disconnects.
			return
		}
	}
}

func (b *multipartTestBackend) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	b.write(w, "<Error><Code>"+code+"</Code><Message>test response</Message></Error>")
}

func (b *multipartTestBackend) write(w http.ResponseWriter, body string) {
	if _, err := io.WriteString(w, body); err != nil {
		b.t.Errorf("write test XML: %v", err)
	}
}

func multipartTestETag(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf(`"%x"`, digest)
}
