package repository

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1" // Git's established object format, not a security credential.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (s *Store) initializeReadme(ctx context.Context, metadata Metadata, input CreateInput, manifest *objectManifest) (string, error) {
	content := "# " + metadata.Name + "\n"
	if metadata.Description != "" {
		content += "\n" + metadata.Description + "\n"
	}
	blob, err := s.putObject(ctx, metadata.ID, "blob", []byte(content), manifest)
	if err != nil {
		return "", err
	}
	blobID, err := hex.DecodeString(blob)
	if err != nil {
		return "", fmt.Errorf("decode blob id: %w", err)
	}
	treeData := append([]byte("100644 README.md\x00"), blobID...)
	tree, err := s.putObject(ctx, metadata.ID, "tree", treeData, manifest)
	if err != nil {
		return "", err
	}
	signature := fmt.Sprintf("%s <%s> %d +0000", input.AuthorName, input.AuthorEmail, metadata.CreatedAt.Unix())
	commit := "tree " + tree + "\nauthor " + signature + "\ncommitter " + signature + "\n\nInitial commit\n"
	return s.putObject(ctx, metadata.ID, "commit", []byte(commit), manifest)
}

func (s *Store) putObject(ctx context.Context, repositoryID, kind string, content []byte, manifest *objectManifest) (string, error) {
	raw := append([]byte(fmt.Sprintf("%s %d\x00", kind, len(content))), content...)
	// SHA-1 provides Git compatibility. SHA-256 in the immutable manifest also
	// verifies content on reads, avoiding reliance on SHA-1 collision resistance.
	gitDigest := sha1.Sum(raw)
	strongDigest := sha256.Sum256(raw)
	id := hex.EncodeToString(gitDigest[:])
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		return "", fmt.Errorf("compress git object: %w", errors.Join(err, writer.Close()))
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close git compressor: %w", err)
	}
	if err := s.repositories.PutImmutable(ctx, objectKey(repositoryID, id), bytes.NewReader(compressed.Bytes()), int64(compressed.Len())); err != nil {
		return "", fmt.Errorf("store git object: %w", err)
	}
	manifest.Objects[id] = objectInfo{Type: kind, Size: int64(len(content)), SHA256: hex.EncodeToString(strongDigest[:])}
	return id, nil
}

func (s *Store) putSnapshot(ctx context.Context, repositoryID, kind string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode %s snapshot: %w", kind, err)
	}
	digest := sha256.Sum256(data)
	relative := "states/00000000000000000001-" + kind + "-" + hex.EncodeToString(digest[:]) + ".json"
	if err := s.repositories.PutImmutable(ctx, "repos/"+repositoryID+"/"+relative, bytes.NewReader(data), int64(len(data))); err != nil {
		return "", fmt.Errorf("store %s snapshot: %w", kind, err)
	}
	return relative, nil
}

func (s *Store) readSnapshot(ctx context.Context, repositoryID, relative string, target any) error {
	data, err := s.read(ctx, "repos/"+repositoryID+"/"+relative, maxJSONBytes)
	if err != nil {
		return fmt.Errorf("read repository snapshot: %w", errors.Join(ErrCorrupt, err))
	}
	digest := sha256.Sum256(data)
	if !strings.HasSuffix(relative, "-"+hex.EncodeToString(digest[:])+".json") {
		return ErrCorrupt
	}
	return decodeJSON(data, target)
}

func objectKey(repositoryID, id string) string {
	return "repos/" + repositoryID + "/objects/" + id[:2] + "/" + id[2:]
}

func (s *Store) object(ctx context.Context, snap snapshot, id, kind string) ([]byte, error) {
	info, ok := snap.manifest.Objects[id]
	if !ok || !objectIDPattern.MatchString(id) || info.Type != kind {
		return nil, ErrCorrupt
	}
	compressed, err := s.read(ctx, objectKey(snap.metadata.ID, id), maxObjectBytes+4096)
	if err != nil {
		return nil, fmt.Errorf("load git object: %w", errors.Join(ErrCorrupt, err))
	}
	reader, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid compressed git object", ErrCorrupt)
	}
	raw, readErr := io.ReadAll(io.LimitReader(reader, maxObjectBytes+128))
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("%w: invalid git object compression: %v", ErrCorrupt, err)
	}
	header := []byte(fmt.Sprintf("%s %d\x00", kind, info.Size))
	if int64(len(raw)) != int64(len(header))+info.Size || !bytes.HasPrefix(raw, header) {
		return nil, ErrCorrupt
	}
	gitDigest := sha1.Sum(raw)
	strongDigest := sha256.Sum256(raw)
	if hex.EncodeToString(gitDigest[:]) != id || hex.EncodeToString(strongDigest[:]) != info.SHA256 {
		return nil, ErrCorrupt
	}
	return raw[len(header):], nil
}

func (s *Store) Branches(ctx context.Context, namespace, name string) ([]Branch, error) {
	snap, err := s.load(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	branches := make([]Branch, 0, len(snap.refs.Refs))
	for ref, commit := range snap.refs.Refs {
		branches = append(branches, Branch{Name: strings.TrimPrefix(ref, "refs/heads/"), Commit: commit})
	}
	slices.SortFunc(branches, func(a, b Branch) int { return strings.Compare(a.Name, b.Name) })
	return branches, nil
}

func resolveRef(snap snapshot, ref string) (string, string, error) {
	if ref == "" {
		ref = snap.metadata.DefaultBranch
	}
	if !validBranch(ref) {
		return "", "", ErrInvalid
	}
	id, ok := snap.refs.Refs["refs/heads/"+ref]
	if !ok && (len(snap.refs.Refs) != 0 || ref != snap.metadata.DefaultBranch) {
		return "", "", ErrNotFound
	}
	return ref, id, nil
}

func validPath(path string) bool {
	if path == "" {
		return true
	}
	if len(path) > 4096 || !utf8.ValidString(path) || strings.Contains(path, "\\") || strings.ContainsFunc(path, unicode.IsControl) {
		return false
	}
	components := strings.Split(path, "/")
	if len(components) > 32 {
		return false
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." || strings.EqualFold(component, ".git") || len(component) > 255 {
			return false
		}
	}
	return true
}

type treeEntry struct{ name, id, kind string }

func (s *Store) treeEntries(ctx context.Context, snap snapshot, id string) ([]treeEntry, error) {
	data, err := s.object(ctx, snap, id, "tree")
	if err != nil {
		return nil, err
	}
	entries := []treeEntry{}
	seen := map[string]bool{}
	for len(data) > 0 {
		if len(entries) >= maxTreeEntries {
			return nil, ErrLimit
		}
		zero := bytes.IndexByte(data, 0)
		if zero < 0 || len(data) < zero+21 {
			return nil, ErrCorrupt
		}
		mode, name, ok := strings.Cut(string(data[:zero]), " ")
		if !ok || !validPath(name) || name == "" || strings.Contains(name, "/") || seen[name] {
			return nil, ErrCorrupt
		}
		kind := "blob"
		switch mode {
		case "40000", "040000":
			kind = "tree"
		case "100644", "100755", "120000":
		default:
			return nil, ErrCorrupt
		}
		id := hex.EncodeToString(data[zero+1 : zero+21])
		if snap.manifest.Objects[id].Type != kind {
			return nil, ErrCorrupt
		}
		seen[name] = true
		entries = append(entries, treeEntry{name: name, id: id, kind: kind})
		data = data[zero+21:]
	}
	return entries, nil
}

func (s *Store) entryAt(ctx context.Context, snap snapshot, commit, path string) (treeEntry, error) {
	_, tree, err := s.commit(ctx, snap, commit)
	if err != nil {
		return treeEntry{}, err
	}
	entry := treeEntry{id: tree, kind: "tree"}
	if path == "" {
		return entry, nil
	}
	for _, component := range strings.Split(path, "/") {
		if entry.kind != "tree" {
			return treeEntry{}, ErrNotFound
		}
		entries, err := s.treeEntries(ctx, snap, entry.id)
		if err != nil {
			return treeEntry{}, err
		}
		found := false
		for _, candidate := range entries {
			if candidate.name == component {
				entry, found = candidate, true
				break
			}
		}
		if !found {
			return treeEntry{}, ErrNotFound
		}
	}
	return entry, nil
}

func (s *Store) Tree(ctx context.Context, namespace, name, ref, path string) (Tree, error) {
	if !validPath(path) {
		return Tree{}, ErrInvalid
	}
	snap, err := s.load(ctx, namespace, name)
	if err != nil {
		return Tree{}, err
	}
	ref, commit, err := resolveRef(snap, ref)
	if err != nil {
		return Tree{}, err
	}
	result := Tree{Ref: ref, Path: path, Commit: commit, Entries: []Entry{}}
	if commit == "" {
		if path != "" {
			return Tree{}, ErrNotFound
		}
		return result, nil
	}
	entry, err := s.entryAt(ctx, snap, commit, path)
	if err != nil {
		return Tree{}, err
	}
	if entry.kind != "tree" {
		return Tree{}, ErrNotFound
	}
	entries, err := s.treeEntries(ctx, snap, entry.id)
	if err != nil {
		return Tree{}, err
	}
	for _, entry := range entries {
		childPath := entry.name
		if path != "" {
			childPath = path + "/" + entry.name
		}
		kind := "file"
		size := snap.manifest.Objects[entry.id].Size
		if entry.kind == "tree" {
			kind, size = "directory", 0
		}
		result.Entries = append(result.Entries, Entry{Name: entry.name, Path: childPath, Type: kind, Size: size})
	}
	slices.SortFunc(result.Entries, func(a, b Entry) int {
		if a.Type != b.Type {
			return strings.Compare(a.Type, b.Type)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return result, nil
}

func (s *Store) Blob(ctx context.Context, namespace, name, ref, path string) (Blob, error) {
	if !validPath(path) || path == "" {
		return Blob{}, ErrInvalid
	}
	snap, err := s.load(ctx, namespace, name)
	if err != nil {
		return Blob{}, err
	}
	ref, commit, err := resolveRef(snap, ref)
	if err != nil {
		return Blob{}, err
	}
	if commit == "" {
		return Blob{}, ErrNotFound
	}
	entry, err := s.entryAt(ctx, snap, commit, path)
	if err != nil {
		return Blob{}, err
	}
	if entry.kind != "blob" {
		return Blob{}, ErrNotFound
	}
	content, err := s.object(ctx, snap, entry.id, "blob")
	if err != nil {
		return Blob{}, err
	}
	result := Blob{Ref: ref, Path: path, Commit: commit, Size: int64(len(content)), IsBinary: !utf8.Valid(content) || bytes.ContainsRune(content, 0)}
	if !result.IsBinary {
		result.Content = string(content)
	}
	return result, nil
}

// Commits returns up to 100 commits along the selected branch's first-parent
// history. The parent IDs preserve merge information without unbounded traversal.
func (s *Store) Commits(ctx context.Context, namespace, name, ref string) ([]Commit, error) {
	snap, err := s.load(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	_, id, err := resolveRef(snap, ref)
	if err != nil {
		return nil, err
	}
	commits := []Commit{}
	seen := map[string]bool{}
	for id != "" && len(commits) < maxCommits {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if seen[id] {
			return nil, ErrCorrupt
		}
		seen[id] = true
		commit, _, err := s.commit(ctx, snap, id)
		if err != nil {
			return nil, err
		}
		commits = append(commits, commit)
		id = ""
		if len(commit.Parents) > 0 {
			id = commit.Parents[0]
		}
	}
	return commits, nil
}

func (s *Store) commit(ctx context.Context, snap snapshot, id string) (Commit, string, error) {
	data, err := s.object(ctx, snap, id, "commit")
	if err != nil {
		return Commit{}, "", err
	}
	header, message, ok := strings.Cut(string(data), "\n\n")
	if !ok || !utf8.Valid(data) {
		return Commit{}, "", ErrCorrupt
	}
	commit := Commit{ID: id, Message: strings.TrimSuffix(message, "\n"), Parents: []string{}}
	var tree string
	var hasAuthor bool
	for _, line := range strings.Split(header, "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			return Commit{}, "", ErrCorrupt
		}
		switch key {
		case "tree":
			if tree != "" || !objectIDPattern.MatchString(value) || snap.manifest.Objects[value].Type != "tree" {
				return Commit{}, "", ErrCorrupt
			}
			tree = value
		case "parent":
			if len(commit.Parents) >= 64 || !objectIDPattern.MatchString(value) || snap.manifest.Objects[value].Type != "commit" {
				return Commit{}, "", ErrCorrupt
			}
			commit.Parents = append(commit.Parents, value)
		case "author":
			if hasAuthor {
				return Commit{}, "", ErrCorrupt
			}
			hasAuthor = true
			name, rest, ok := strings.Cut(value, " <")
			if !ok || !validIdentityPart(name) {
				return Commit{}, "", ErrCorrupt
			}
			_, timestamp, ok := strings.Cut(rest, "> ")
			fields := strings.Fields(timestamp)
			if !ok || len(fields) != 2 {
				return Commit{}, "", ErrCorrupt
			}
			seconds, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				return Commit{}, "", ErrCorrupt
			}
			commit.AuthorName, commit.CreatedAt = name, time.Unix(seconds, 0).UTC()
		}
	}
	if tree == "" || !hasAuthor {
		return Commit{}, "", ErrCorrupt
	}
	return commit, tree, nil
}
