package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

const maxLFSPointerBytes = 1024

type lfsIndex struct {
	Version int              `json:"version"`
	Objects map[string]int64 `json:"objects"`
}

func validLFSIndex(index *lfsIndex) bool {
	if index == nil {
		return true // Older generations are derived from their Git objects.
	}
	if index.Version != 1 || index.Objects == nil || len(index.Objects) > maxObjects {
		return false
	}
	for oid, size := range index.Objects {
		if !ValidLFSOID(oid) || size < 0 {
			return false
		}
	}
	return true
}

// parseLFSPointer recognizes the standard small pointer representation. Files
// that declare the LFS version but contain invalid fields fail publication.
func parseLFSPointer(data []byte) (LFSObject, bool, error) {
	if len(data) >= maxLFSPointerBytes {
		return LFSObject{}, false, nil
	}
	// Match the native client's accepted legacy versions and noncanonical
	// whitespace so maintenance never overlooks a pointer the client follows.
	lines := strings.Split(string(bytes.TrimSpace(data)), "\n")
	versionLine := -1
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		switch line {
		case "version https://git-lfs.github.com/spec/v1", "version https://hawser.github.com/spec/v1", "version http://git-media.io/v/2":
			versionLine = i
		default:
			// The native parser also accepts extensions before version.
			if line != "" && !strings.HasPrefix(line, "ext-") {
				return LFSObject{}, false, nil
			}
		}
		if versionLine >= 0 {
			break
		}
	}
	if versionLine < 0 {
		return LFSObject{}, false, nil
	}
	var result LFSObject
	seen := map[string]bool{}
	for i, line := range lines {
		if i == versionLine {
			continue
		}
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok || value == "" || seen[key] {
			return result, true, ErrInvalid
		}
		seen[key] = true
		switch key {
		case "oid":
			result.OID = strings.TrimPrefix(value, "sha256:")
			if value != "sha256:"+result.OID || !ValidLFSOID(result.OID) {
				return result, true, ErrInvalid
			}
		case "size":
			size, err := strconv.ParseInt(value, 10, 64)
			if err != nil || size < 0 {
				return result, true, ErrInvalid
			}
			result.Size = size
		default:
			// Extensions do not alter the identity of the stored content.
			if !strings.HasPrefix(key, "ext-") || strings.ContainsAny(value, "\x00\r") {
				return result, true, ErrInvalid
			}
		}
	}
	if !seen["oid"] || !seen["size"] {
		return result, true, ErrInvalid
	}
	return result, true, nil
}

func addLFSPointer(index map[string]int64, data []byte) error {
	object, pointer, err := parseLFSPointer(data)
	if err != nil || !pointer {
		return err
	}
	if prior, ok := index[object.OID]; ok && prior != object.Size {
		return errors.Join(ErrInvalid, ErrLFSHashMismatch)
	}
	index[object.OID] = object.Size
	return nil
}

func collectLFSPointers(ctx context.Context, objects map[string]objectInfo, get gitGetter) (map[string]int64, error) {
	index := map[string]int64{}
	for id, info := range objects {
		if info.Type != "blob" || info.Size > maxLFSPointerBytes {
			continue
		}
		object, err := get(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := addLFSPointer(index, object.Data); err != nil {
			return nil, fmt.Errorf("invalid LFS pointer in Git blob %s: %w", id, err)
		}
	}
	return index, nil
}

func (s *Store) validateLFSPointers(ctx context.Context, repositoryID string, pointers map[string]int64) error {
	for oid, size := range pointers {
		record, err := s.checkLFSRecord(ctx, repositoryID, oid)
		if err != nil {
			return fmt.Errorf("LFS object %s: %w", oid, err)
		}
		if record.Object.Size != size {
			return fmt.Errorf("LFS pointer size differs for %s: %w", oid, errors.Join(ErrInvalid, ErrLFSHashMismatch))
		}
	}
	return nil
}

func compareLFSIndex(index *lfsIndex, actual map[string]int64) error {
	if !validLFSIndex(index) || (index != nil && !maps.Equal(index.Objects, actual)) {
		return fmt.Errorf("LFS reference index differs from Git objects: %w", ErrCorrupt)
	}
	return nil
}
