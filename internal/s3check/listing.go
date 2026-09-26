package s3check

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/define42/GitOneS3/internal/storage"
)

func (s *suite) listing(ctx context.Context, mode string) {
	var expected []storage.ObjectInfo
	var setupErr error
	s.check(ctx, "listing_setup", func(ctx context.Context) error {
		expected, setupErr = s.seedListing(ctx)
		return setupErr
	})
	if setupErr != nil {
		return
	}
	if mode == "v1" || mode == "both" {
		s.check(ctx, "list_v1_marker", func(ctx context.Context) error {
			return s.listAll(ctx, s.prefix+"listing/", expected, "v1", "", 1000)
		})
		s.check(ctx, "list_v1_visibility", func(ctx context.Context) error {
			return s.listVisibility(ctx, "v1")
		})
	}
	if mode == "v2" || mode == "both" {
		s.check(ctx, "list_v2_continuation", func(ctx context.Context) error {
			return s.listAll(ctx, s.prefix+"listing/", expected, "v2", "", 1000)
		})
		s.check(ctx, "list_v2_start_after", func(ctx context.Context) error {
			return s.listAll(ctx, s.prefix+"listing/", expected, "v2-start-after", "", 1000)
		})
		s.check(ctx, "list_v2_visibility", func(ctx context.Context) error {
			return s.listVisibility(ctx, "v2-start-after")
		})
	}
}

func (s *suite) seedListing(ctx context.Context) ([]storage.ObjectInfo, error) {
	keys := make([]string, s.objects)
	for i := range keys {
		keys[i] = s.key(fmt.Sprintf("listing/%06d", i))
	}
	expected := make([]storage.ObjectInfo, len(keys))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	var firstError error
	var errorOnce sync.Once
	for worker := range 8 {
		workers.Go(func() {
			for i := worker; i < len(keys); i += 8 {
				if ctx.Err() != nil {
					return
				}
				info, err := s.put(ctx, keys[i], []byte("listing probe\n"), storage.PutOptions{})
				if err != nil {
					errorOnce.Do(func() {
						firstError = fmt.Errorf("create listing object %d: %w", i, err)
						cancel()
					})
					return
				}
				expected[i] = info
			}
		})
	}
	workers.Wait()
	if firstError != nil {
		return nil, firstError
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return expected, nil
}

type listingPage struct {
	objects   []types.Object
	truncated *bool
	token     string
}

// listAll compares every returned entry with known writes. A superficially
// successful first page must not conceal missing objects or ignored cursors.
func (s *suite) listAll(
	ctx context.Context,
	prefix string,
	expected []storage.ObjectInfo,
	mode, after string,
	limit int32,
) error {
	var token string
	seenTokens := make(map[string]bool)
	found := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := s.readListingPage(ctx, prefix, mode, after, token, limit)
		if err != nil {
			return fmt.Errorf("request page after %d objects: %w", found, err)
		}
		if page.truncated == nil {
			return errors.New("listing response is missing IsTruncated")
		}
		if int64(len(page.objects)) > int64(limit) {
			return fmt.Errorf("listing returned %d objects for MaxKeys=%d", len(page.objects), limit)
		}
		if len(page.objects) == 0 && *page.truncated {
			return errors.New("listing returned an empty truncated page")
		}
		for _, object := range page.objects {
			key := aws.ToString(object.Key)
			if !strings.HasPrefix(key, prefix) || key <= after {
				return fmt.Errorf("listing key %q is outside prefix or does not advance past %q", key, after)
			}
			if found >= len(expected) {
				return fmt.Errorf("listing returned unexpected extra key %q", key)
			}
			want := expected[found]
			if key != want.Key {
				return fmt.Errorf("listing object %d: got key %q, want %q", found, key, want.Key)
			}
			if object.Size == nil || *object.Size != want.Size ||
				aws.ToString(object.ETag) != string(want.Version) ||
				object.LastModified == nil || object.LastModified.IsZero() {
				return fmt.Errorf("listing key %q has missing or stale size, ETag, or LastModified", key)
			}
			found++
			after = key
		}
		if !*page.truncated {
			if found != len(expected) {
				return fmt.Errorf("listing ended after %d objects, want %d", found, len(expected))
			}
			return nil
		}
		if found >= len(expected) {
			return fmt.Errorf("listing remains truncated after all %d expected objects", found)
		}
		if mode == "v2" {
			if page.token == "" || seenTokens[page.token] {
				return errors.New("listing has missing or repeated continuation token")
			}
			seenTokens[page.token] = true
			token = page.token
		}
	}
}

func (s *suite) readListingPage(
	ctx context.Context,
	prefix, mode, after, token string,
	limit int32,
) (listingPage, error) {
	if mode == "v1" {
		input := &s3.ListObjectsInput{
			Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(limit),
		}
		if after != "" {
			input.Marker = aws.String(after)
		}
		output, err := s.client.ListObjects(ctx, input)
		if err != nil {
			return listingPage{}, err
		}
		if len(output.CommonPrefixes) != 0 {
			return listingPage{}, errors.New("listing unexpectedly returned CommonPrefixes without a delimiter")
		}
		// Without a delimiter, NextMarker may be absent. The last returned key
		// is the exclusive marker for the next request.
		return listingPage{objects: output.Contents, truncated: output.IsTruncated}, nil
	}
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(limit),
	}
	if mode == "v2" && token != "" {
		input.ContinuationToken = aws.String(token)
	}
	if mode == "v2-start-after" && after != "" {
		input.StartAfter = aws.String(after)
	}
	output, err := s.client.ListObjectsV2(ctx, input)
	if err != nil {
		return listingPage{}, err
	}
	if len(output.CommonPrefixes) != 0 {
		return listingPage{}, errors.New("listing unexpectedly returned CommonPrefixes without a delimiter")
	}
	if output.KeyCount != nil && int64(*output.KeyCount) != int64(len(output.Contents)) {
		return listingPage{}, errors.New("listing KeyCount does not match returned objects")
	}
	return listingPage{
		objects: output.Contents, truncated: output.IsTruncated,
		token: aws.ToString(output.NextContinuationToken),
	}, nil
}

func (s *suite) listVisibility(ctx context.Context, mode string) error {
	prefix := s.prefix + "visibility/" + mode + "/"
	expected := make([]storage.ObjectInfo, 3)
	for i := range expected {
		key := s.key(fmt.Sprintf("visibility/%s/%d", mode, i))
		info, err := s.put(ctx, key, []byte("before"), storage.PutOptions{})
		if err != nil {
			return err
		}
		expected[i] = info
	}
	if err := s.listAll(ctx, prefix, expected, mode, "", 1); err != nil {
		return fmt.Errorf("list immediately after create: %w", err)
	}
	updated, err := s.put(ctx, expected[1].Key, []byte("updated and longer"), storage.PutOptions{})
	if err != nil {
		return err
	}
	expected[1] = updated
	deletedKey := expected[0].Key
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(deletedKey),
	}); err != nil {
		return fmt.Errorf("delete visibility object: %w", err)
	}
	if err := s.listAll(ctx, prefix, expected[1:], mode, "", 1); err != nil {
		return fmt.Errorf("list immediately after overwrite and delete: %w", err)
	}
	if err := s.listAll(ctx, prefix, expected[1:], mode, deletedKey, 1); err != nil {
		return fmt.Errorf("resume listing after deleted marker: %w", err)
	}
	return nil
}
