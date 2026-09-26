package storage_test

import (
	"errors"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestMultipartSize(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		parts []storage.MultipartPart
		size  int64
		valid bool
	}{
		{name: "empty object", parts: []storage.MultipartPart{{Number: 1, ETag: "a"}}, valid: true},
		{name: "small final", parts: []storage.MultipartPart{{Number: 1, ETag: "a", Size: 5}}, size: 5, valid: true},
		{name: "two parts", parts: []storage.MultipartPart{{Number: 1, ETag: "a", Size: storage.MinMultipartPartSize}, {Number: 2, ETag: "b", Size: 1}}, size: storage.MinMultipartPartSize + 1, valid: true},
		{name: "no parts"},
		{name: "too many", parts: make([]storage.MultipartPart, storage.MaxMultipartParts+1)},
		{name: "missing ETag", parts: []storage.MultipartPart{{Number: 1, Size: 1}}},
		{name: "missing number", parts: []storage.MultipartPart{{ETag: "a", Size: 1}}},
		{name: "negative", parts: []storage.MultipartPart{{Number: 1, ETag: "a", Size: -1}}},
		{name: "oversized", parts: []storage.MultipartPart{{Number: 1, ETag: "a", Size: storage.MaxMultipartPartSize + 1}}},
		{name: "gap", parts: []storage.MultipartPart{{Number: 1, ETag: "a", Size: storage.MinMultipartPartSize}, {Number: 3, ETag: "b", Size: 1}}},
		{name: "duplicate", parts: []storage.MultipartPart{{Number: 1, ETag: "a", Size: storage.MinMultipartPartSize}, {Number: 1, ETag: "b", Size: 1}}},
		{name: "small non-final", parts: []storage.MultipartPart{{Number: 1, ETag: "a", Size: 1}, {Number: 2, ETag: "b", Size: 1}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			size, err := storage.MultipartSize(test.parts)
			if test.valid {
				if err != nil || size != test.size {
					t.Fatalf("MultipartSize = %d, %v", size, err)
				}
			} else if !errors.Is(err, storage.ErrInvalidMultipart) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
