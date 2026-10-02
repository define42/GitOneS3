package repository

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

const MaxListPageSize = 50
const listMetadataWorkers = 8

// ListPage is a bounded repository catalog page. NextCursor is the last
// repository name when another page exists. Pages follow storage-key order;
// concurrent creations can become visible on subsequent requests.
type ListPage struct {
	Repositories []Metadata
	NextCursor   string
}

// List collects the catalog for in-process callers. HTTP handlers use
// ListRepositoriesPage so their work stays bounded regardless of catalog size.
func (s *Store) List(ctx context.Context, namespace string) ([]Metadata, error) {
	result := []Metadata{}
	after := ""
	for {
		page, err := s.ListRepositoriesPage(ctx, namespace, after, MaxListPageSize)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Repositories...)
		if page.NextCursor == "" {
			slices.SortFunc(result, func(a, b Metadata) int { return strings.Compare(a.Name, b.Name) })
			return result, nil
		}
		after = page.NextCursor
	}
}

// ListRepositoriesPage reads only repository metadata, the current state, and
// its refs. Listing never downloads or decodes Git object manifests or packs.
func (s *Store) ListRepositoriesPage(ctx context.Context, namespace, after string, limit int) (ListPage, error) {
	if !s.validNamespace(namespace) || (after != "" && !ValidName(after)) || limit < 1 || limit > MaxListPageSize {
		return ListPage{}, ErrInvalid
	}
	prefix := "repositories/" + namespace + "/"
	afterKey := ""
	if after != "" {
		afterKey = metadataKey(namespace, after)
	}
	page, err := s.objects.ListPage(ctx, prefix, afterKey, limit)
	if err != nil {
		return ListPage{}, fmt.Errorf("list repositories: %w", err)
	}
	if len(page.Objects) > limit {
		return ListPage{}, ErrCorrupt
	}
	names := make([]string, len(page.Objects))
	previous := afterKey
	for i, object := range page.Objects {
		name := strings.TrimSuffix(strings.TrimPrefix(object.Key, prefix), ".json")
		if !ValidName(name) || object.Key != metadataKey(namespace, name) || object.Key <= previous {
			return ListPage{}, ErrCorrupt
		}
		names[i], previous = name, object.Key
	}
	result := ListPage{Repositories: make([]Metadata, len(names))}
	if page.NextAfter != "" {
		if len(names) == 0 || page.NextAfter != previous {
			return ListPage{}, ErrCorrupt
		}
		result.NextCursor = names[len(names)-1]
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, len(names))
	for i := range names {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	var once sync.Once
	var readErr error
	for range min(listMetadataWorkers, len(names)) {
		wg.Go(func() {
			for i := range jobs {
				metadata, err := s.listMetadata(ctx, namespace, names[i])
				if err != nil {
					once.Do(func() { readErr = err; cancel() })
					return
				}
				result.Repositories[i] = metadata
			}
		})
	}
	wg.Wait()
	if readErr != nil {
		return ListPage{}, readErr
	}
	if err := ctx.Err(); err != nil {
		return ListPage{}, err
	}
	return result, nil
}

func (s *Store) listMetadata(ctx context.Context, namespace, name string) (Metadata, error) {
	base, err := s.maintenanceBase(ctx, namespace, name)
	if err != nil {
		return Metadata{}, err
	}
	var refs refsSnapshot
	if err := s.readSnapshot(ctx, base.metadata.ID, base.state.RefsSnapshot, &refs); err != nil {
		return Metadata{}, err
	}
	if refs.SchemaVersion != 1 || refs.Refs == nil || len(refs.Refs) > maxObjects {
		return Metadata{}, ErrCorrupt
	}
	for ref, id := range refs.Refs {
		if !ValidRef(ref) || !objectIDPattern.MatchString(id) {
			return Metadata{}, ErrCorrupt
		}
	}
	base.metadata.DefaultBranch = strings.TrimPrefix(base.state.DefaultBranch, "refs/heads/")
	if !validBranch(base.metadata.DefaultBranch) {
		return Metadata{}, ErrCorrupt
	}
	base.metadata.IsEmpty = len(refs.Refs) == 0
	return base.metadata, nil
}
