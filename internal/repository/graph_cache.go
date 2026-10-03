package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
)

// Graph edges are derived only from verified published objects. The index
// eliminates repeated decompression and storage reads during negotiation. Its
// independent bound protects requests with unusually dense trees; those requests
// retain the existing bounded object walker as a fallback.
const maxGraphIndexBytes = 64 << 20

type validatedGraph struct {
	objects map[string]objectInfo
	links   map[string][]objectLink
}

func (r *GitReader) validateCachedGraph(ctx context.Context) error {
	if r.closed || !maps.Equal(r.base.References, r.base.original.refs.Refs) {
		return ErrInvalid
	}
	value, err := r.store.cache.LoadMemory(ctx, r.CacheKey("graph-v1"), func(ctx context.Context) (any, int64, error) {
		if err := r.prefetch(ctx, func(_ string, info objectInfo) bool { return info.Type != "blob" }); err != nil {
			return nil, 0, err
		}
		graph := &validatedGraph{links: make(map[string][]objectLink)}
		size := int64(128)
		limit := min(int64(maxGraphIndexBytes), r.store.cache.MemoryLimit())
		record := func(id string, links []objectLink) {
			if graph.links == nil {
				return
			}
			charge := int64(128 + len(id))
			for _, link := range links {
				charge += int64(64 + len(link.id) + len(link.kind))
			}
			if charge > limit-size {
				graph.links = nil
				size = 128
				return
			}
			// Link parsers can return substrings of a large commit message/tree.
			// Own short strings so an index edge cannot retain an entire body.
			owned := make([]objectLink, len(links))
			for i, link := range links {
				owned[i] = objectLink{id: strings.Clone(link.id), kind: strings.Clone(link.kind)}
			}
			graph.links[id] = owned
			size += charge
		}
		objects, err := walkObjectsIndexed(ctx, r.base.References, r.base.original.manifest.Objects, r.object, record)
		if err != nil {
			return nil, 0, errors.Join(ErrCorrupt, err)
		}
		graph.objects = objects
		// Object-info strings remain reachable through this map even when its
		// source manifest is evicted; account for them here too.
		size += manifestMemoryBytes(objectManifest{Objects: objects})
		return graph, size, nil
	})
	if err != nil {
		return err
	}
	r.graph = value.(*validatedGraph)
	r.base.available = r.graph.objects
	return nil
}

func (r *GitReader) cachedReachable(ctx context.Context, refs map[string]string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := sha256.New()
	for _, ref := range slices.Sorted(maps.Keys(refs)) {
		_, _ = digest.Write([]byte(ref))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(refs[ref]))
		_, _ = digest.Write([]byte{0})
	}
	key := r.CacheKey("reachable-v1:" + hex.EncodeToString(digest.Sum(nil)))
	value, err := r.store.cache.LoadMemory(ctx, key, func(ctx context.Context) (any, int64, error) {
		ids, err := r.graph.reachable(ctx, refs)
		return ids, int64(256 + len(ids)*80), err
	})
	if err != nil {
		return nil, err
	}
	// Upload negotiation filters this slice in place when subtracting haves.
	return slices.Clone(value.([]string)), nil
}

func (g *validatedGraph) reachable(ctx context.Context, refs map[string]string) ([]string, error) {
	seen := make(map[string]bool)
	queue := make([]string, 0, len(refs))
	add := func(id, kind string) error {
		info, ok := g.objects[id]
		if !ok || (kind != "" && kind != info.Type) {
			return ErrInvalid
		}
		if !seen[id] {
			seen[id] = true
			queue = append(queue, id)
		}
		return nil
	}
	for ref, id := range refs {
		kind := ""
		if strings.HasPrefix(ref, "refs/heads/") {
			kind = "commit"
		}
		if err := add(id, kind); err != nil {
			return nil, err
		}
	}
	for len(queue) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, link := range g.links[id] {
			if err := add(link.id, link.kind); err != nil {
				return nil, err
			}
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}
