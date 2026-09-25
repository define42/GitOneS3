package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
)

func TestMemoryStore_PutConditions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	first, err := store.Put(ctx, "packs/a.pack", bytes.NewBufferString("one"), 3, PutOptions{IfNoneMatch: true})
	if err != nil {
		t.Fatalf("initial Put() error = %v", err)
	}
	if _, err := store.Put(
		ctx,
		"packs/a.pack",
		bytes.NewBufferString("two"),
		3,
		PutOptions{IfNoneMatch: true},
	); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("immutable overwrite error = %v, want ErrAlreadyExists", err)
	}
	if _, err := store.Put(
		ctx,
		"packs/a.pack",
		bytes.NewBufferString("two"),
		3,
		PutOptions{IfMatch: "wrong"},
	); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("wrong version error = %v, want ErrPreconditionFailed", err)
	}
	second, err := store.Put(
		ctx,
		"packs/a.pack",
		bytes.NewBufferString("two"),
		3,
		PutOptions{IfMatch: first.Version},
	)
	if err != nil {
		t.Fatalf("conditional Put() error = %v", err)
	}
	if second.Version == first.Version {
		t.Fatal("conditional Put() did not advance the version")
	}
}

func TestMemoryStore_GetRange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if _, err := store.Put(
		ctx,
		"packs/a.pack",
		bytes.NewBufferString("0123456789"),
		10,
		PutOptions{},
	); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	body, _, err := store.GetRange(ctx, "packs/a.pack", 3, 4)
	if err != nil {
		t.Fatalf("GetRange() error = %v", err)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got, want := string(data), "3456"; got != want {
		t.Fatalf("GetRange() = %q, want %q", got, want)
	}
}

func TestMemoryStore_RejectsSizeMismatch(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	_, err := store.Put(
		context.Background(),
		"packs/a.pack",
		bytes.NewBufferString("short"),
		10,
		PutOptions{},
	)
	if err == nil {
		t.Fatal("Put() error = nil, want size mismatch")
	}
}

func TestMemoryStore_ListPage(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	expected := []ObjectInfo{}
	for index := range 23 {
		key := fmt.Sprintf("packs/%02d.pack", index)
		info, err := store.Put(
			t.Context(),
			key,
			bytes.NewBufferString("pack"),
			4,
			PutOptions{},
		)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, info)
	}
	if _, err := store.Put(
		t.Context(),
		"other/object",
		bytes.NewReader(nil),
		0,
		PutOptions{},
	); err != nil {
		t.Fatal(err)
	}

	for _, limit := range []int{1, 2, 10, 23, MaxListPageSize} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			t.Parallel()
			actual := []ObjectInfo{}
			after := ""
			for {
				page, err := store.ListPage(
					t.Context(),
					"packs/",
					after,
					limit,
				)
				if err != nil {
					t.Fatalf("ListPage() error = %v", err)
				}
				if len(page.Objects) > limit || len(page.Objects) == 0 {
					t.Fatalf("ListPage() returned %d objects for limit %d", len(page.Objects), limit)
				}
				actual = append(actual, page.Objects...)
				if len(actual) > len(expected) {
					t.Fatal("ListPage() returned repeated objects")
				}
				if page.NextAfter == "" {
					break
				}
				last := page.Objects[len(page.Objects)-1].Key
				if page.NextAfter != last || page.NextAfter <= after {
					t.Fatalf("ListPage() cursor = %q, want advancing last key %q", page.NextAfter, last)
				}
				after = page.NextAfter
			}
			if !slices.Equal(actual, expected) {
				t.Fatalf("ListPage() objects = %v, want %v", actual, expected)
			}
		})
	}
}

func TestMemoryStore_ListPageCursorMayBeDeleted(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	for _, key := range []string{"packs/a", "packs/b", "packs/c"} {
		if _, err := store.Put(
			t.Context(),
			key,
			bytes.NewReader(nil),
			0,
			PutOptions{},
		); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ListPage(t.Context(), "packs/", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), first.NextAfter, ""); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListPage(t.Context(), "packs/", first.NextAfter, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 2 || page.Objects[0].Key != "packs/b" || page.Objects[1].Key != "packs/c" {
		t.Fatalf("ListPage() after deleted cursor = %v", page.Objects)
	}
	if page.NextAfter != "" {
		t.Fatalf("ListPage() terminal cursor = %q, want empty", page.NextAfter)
	}
	for _, after := range []string{"packs/c", "packs/z"} {
		page, err := store.ListPage(t.Context(), "packs/", after, 2)
		if err != nil || len(page.Objects) != 0 || page.NextAfter != "" {
			t.Fatalf("ListPage() past end = %+v, error %v", page, err)
		}
	}
}

func TestMemoryStore_ListPageRejectsInvalidRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix string
		after  string
		limit  int
	}{
		{name: "invalid prefix", prefix: "packs//", limit: 1},
		{name: "foreign cursor", prefix: "packs/", after: "other/a", limit: 1},
		{name: "invalid cursor", prefix: "packs/", after: "packs/../a", limit: 1},
		{name: "invalid limit", prefix: "packs/", limit: MaxListPageSize + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewMemoryStore().ListPage(
				t.Context(),
				test.prefix,
				test.after,
				test.limit,
			)
			if err == nil {
				t.Fatal("ListPage() accepted an invalid request")
			}
		})
	}
}

func TestMemoryStore_ListPageCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := NewMemoryStore().ListPage(ctx, "packs/", "", 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ListPage() error = %v, want context.Canceled", err)
	}
}
