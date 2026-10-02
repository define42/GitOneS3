package repository

import (
	"context"
	"errors"
	"strings"
)

// BrowseResult describes one repository page from a single published generation.
type BrowseResult struct {
	Metadata Metadata
	Branches []Branch
	Tree     *Tree
	Blob     *Blob
	Readme   *Blob
	Commits  []Commit
}

// Browse reads one fresh snapshot for metadata, branches, and the selected page.
// The manifest is validated once and remains local to this request; neither
// repository identity nor immutable content is cached across requests.
func (s *Store) Browse(ctx context.Context, namespace, name, ref, path string, history bool) (BrowseResult, error) {
	if !validPath(path) {
		return BrowseResult{}, ErrInvalid
	}
	snap, err := s.load(ctx, namespace, name)
	if err != nil {
		return BrowseResult{}, err
	}
	ref, commit, err := resolveRef(snap, ref)
	if err != nil {
		return BrowseResult{}, err
	}
	result := BrowseResult{Metadata: snap.metadata, Branches: snapshotBranches(snap)}
	if history {
		result.Commits, err = s.snapshotCommits(ctx, snap, ref)
		return result, err
	}
	if path != "" {
		if commit == "" {
			return BrowseResult{}, ErrNotFound
		}
		entry, err := s.entryAt(ctx, snap, commit, path)
		if err != nil {
			return BrowseResult{}, err
		}
		if entry.kind == "blob" {
			blob, err := s.snapshotBlob(ctx, snap, ref, path)
			if err != nil {
				return BrowseResult{}, err
			}
			result.Blob = &blob
			return result, nil
		}
	}
	tree, err := s.snapshotTree(ctx, snap, ref, path)
	if err != nil {
		return BrowseResult{}, err
	}
	result.Tree = &tree
	for _, entry := range tree.Entries {
		name := strings.ToLower(entry.Name)
		if entry.Type != "file" || (name != "readme" && name != "readme.md" && name != "readme.txt") {
			continue
		}
		readme, err := s.snapshotBlob(ctx, snap, ref, entry.Path)
		if err != nil {
			// An unavailable preview should not hide an otherwise valid directory.
			// Corrupt data and cancelled requests still fail explicitly.
			if !errors.Is(err, ErrCorrupt) && (errors.Is(err, ErrNotFound) || errors.Is(err, ErrLimit)) {
				break
			}
			return BrowseResult{}, err
		}
		result.Readme = &readme
		break
	}
	return result, nil
}
