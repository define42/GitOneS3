package repository

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- Git's object format requires SHA-1; stored content is also verified with SHA-256.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"unicode/utf8"
)

// These bounds apply to the complete reachable repository, not just one push.
const MaxGitBytes = 1 << 30
const MaxGitObjectBytes = 16 << 20
const MaxGitObjects = maxObjects
const MaxPackBytes = MaxGitBytes + (8 << 20)

var ErrConflict = errors.New("repository: concurrent reference update")
var ErrForbidden = errors.New("repository: write authorization required")

// GitObject is a decoded canonical Git object, without its loose-object header.
type GitObject struct {
	Type string
	Data []byte
}

// GitSnapshot pins one generation. Transport code may read the public fields;
// publication always checks the private original generation and references.
type GitSnapshot struct {
	DefaultBranch string
	References    map[string]string
	Objects       map[string]GitObject
	original      snapshot
	available     map[string]objectInfo
}

type RefUpdate struct{ Name, Old, New string }

func ValidRef(ref string) bool {
	name, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		name, ok = strings.CutPrefix(ref, "refs/tags/")
	}
	return ok && validBranch(name)
}

func GitObjectID(object GitObject) string {
	hash := sha1.New() // #nosec G401 -- Git object identity only, not a credential or security checksum.
	header := append([]byte(object.Type), ' ')
	header = strconv.AppendInt(header, int64(len(object.Data)), 10)
	header = append(header, 0)
	// hash.Hash.Write always accepts the entire input and never returns an error.
	hash.Write(header)
	hash.Write(object.Data)
	return hex.EncodeToString(hash.Sum(nil))
}

func gitObjectInfo(object GitObject) objectInfo {
	raw := append([]byte(fmt.Sprintf("%s %d\x00", object.Type, len(object.Data))), object.Data...)
	digest := sha256.Sum256(raw)
	return objectInfo{Type: object.Type, Size: int64(len(object.Data)), SHA256: hex.EncodeToString(digest[:])}
}

func (s *Store) existingSnapshot(ctx context.Context, id, kind string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	relative := "states/00000000000000000001-" + kind + "-" + hex.EncodeToString(digest[:]) + ".json"
	limit := maxJSONBytes
	if kind == "manifest" {
		limit = maxManifestBytes
	}
	actual, err := s.read(ctx, "repos/"+id+"/"+relative, int64(limit))
	if err != nil {
		return "", err
	}
	if !bytes.Equal(actual, data) {
		return "", ErrCorrupt
	}
	return relative, nil
}

// ReadGitReferences pins metadata, refs, and the manifest without reading Git
// object bodies. Advertisements only need this bounded metadata. Object graph
// validation is deferred to LoadGitObjects before transferring or publishing.
func (s *Store) ReadGitReferences(ctx context.Context, namespace, name string) (*GitSnapshot, error) {
	snap, err := s.load(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, info := range snap.manifest.Objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		total += info.Size
		if total > MaxGitBytes {
			return nil, ErrLimit
		}
	}
	return &GitSnapshot{DefaultBranch: snap.metadata.DefaultBranch, References: maps.Clone(snap.refs.Refs), original: snap}, nil
}

// LoadGitObjects materializes and validates the already pinned generation. It
// never reloads current refs: a concurrent push must not mix an advertisement's
// refs with a different generation's objects or publication version.
func (s *Store) LoadGitObjects(ctx context.Context, base *GitSnapshot) (*GitSnapshot, error) {
	if base == nil || !idPattern.MatchString(base.original.metadata.ID) || base.original.version == "" {
		return nil, ErrInvalid
	}
	snap := base.original
	// Compatibility helper for small in-process callers. Network transfers use
	// OpenGit and never materialize the complete repository.
	var total int64
	for _, info := range snap.manifest.Objects {
		total += info.Size
		if total > 64<<20 {
			return nil, ErrLimit
		}
	}
	result := &GitSnapshot{DefaultBranch: snap.metadata.DefaultBranch, References: maps.Clone(snap.refs.Refs), Objects: map[string]GitObject{}, original: snap}
	for id, info := range snap.manifest.Objects {
		data, err := s.object(ctx, snap, id, info.Type)
		if err != nil {
			return nil, err
		}
		result.Objects[id] = GitObject{Type: info.Type, Data: data}
	}
	reachable, err := ReachableGit(ctx, result.References, result.Objects)
	if err != nil {
		// Invalid persisted graphs are corruption, not invalid client input.
		return nil, fmt.Errorf("%w: %s", ErrCorrupt, err.Error())
	}
	// Only published, reachable objects may participate in fetch negotiation.
	// An unrelated manifest entry is not evidence of a common client ancestor.
	result.Objects = reachable
	return result, nil
}

func (s *Store) ReadGit(ctx context.Context, namespace, name string) (*GitSnapshot, error) {
	base, err := s.ReadGitReferences(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	return s.LoadGitObjects(ctx, base)
}

// PublishGit durably stores the complete next generation, rechecks the caller's
// current token and membership, then atomically publishes all reference changes.
func (s *Store) PublishGit(ctx context.Context, base *GitSnapshot, updates []RefUpdate, incoming map[string]GitObject, authorize func(context.Context) error) error {
	infos := make(map[string]objectInfo, len(incoming))
	for id, object := range incoming {
		if len(object.Data) > MaxGitObjectBytes || GitObjectID(object) != id {
			return ErrInvalid
		}
		infos[id] = gitObjectInfo(object)
	}
	get := func(_ context.Context, id string) (GitObject, error) {
		object, ok := incoming[id]
		if !ok {
			return GitObject{}, ErrNotFound
		}
		return object, nil
	}
	return s.publishObjects(ctx, base, updates, infos, get, false, authorize)
}

// ReachableGit validates object graph connectivity and returns only reachable
// content. Gitlinks reference another repository and are deliberately not walked.
func ReachableGit(ctx context.Context, refs map[string]string, objects map[string]GitObject) (map[string]GitObject, error) {
	queue := make([]objectLink, 0, len(refs))
	for ref, id := range refs {
		kind := ""
		if strings.HasPrefix(ref, "refs/heads/") {
			kind = "commit"
		}
		queue = append(queue, objectLink{id: id, kind: kind})
	}
	result := map[string]GitObject{}
	total := 0
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		link := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		object, ok := objects[link.id]
		if !ok || (link.kind != "" && object.Type != link.kind) {
			return nil, ErrInvalid
		}
		if _, ok := result[link.id]; ok {
			continue
		}
		if len(object.Data) > MaxGitObjectBytes || GitObjectID(object) != link.id {
			return nil, ErrInvalid
		}
		total += len(object.Data)
		if total > MaxGitBytes || len(result) >= MaxGitObjects {
			return nil, ErrLimit
		}
		links, err := gitLinks(object)
		if err != nil {
			return nil, err
		}
		result[link.id] = object
		queue = append(queue, links...)
		if len(queue) > MaxGitObjects*64 {
			return nil, ErrLimit
		}
	}
	return result, nil
}

type objectLink struct{ id, kind string }

func gitLinks(object GitObject) ([]objectLink, error) {
	links := []objectLink{}
	switch object.Type {
	case "blob":
		return links, nil
	case "tree":
		data := object.Data
		seen := map[string]bool{}
		for len(data) > 0 {
			zero := bytes.IndexByte(data, 0)
			if zero < 0 || len(data) < zero+21 || len(seen) >= maxTreeEntries {
				return nil, ErrInvalid
			}
			mode, name, ok := strings.Cut(string(data[:zero]), " ")
			if !ok || name == "" || !validPath(name) || strings.Contains(name, "/") || seen[name] {
				return nil, ErrInvalid
			}
			seen[name] = true
			kind := "blob"
			switch mode {
			case "40000", "040000":
				kind = "tree"
			case "100644", "100755", "120000":
			case "160000":
				kind = "gitlink"
			default:
				return nil, ErrInvalid
			}
			if kind != "gitlink" {
				links = append(links, objectLink{id: hex.EncodeToString(data[zero+1 : zero+21]), kind: kind})
			}
			data = data[zero+21:]
		}
	case "commit", "tag":
		header, _, ok := strings.Cut(string(object.Data), "\n\n")
		if !ok || strings.Contains(header, "\x00") || !utf8.Valid(object.Data) {
			return nil, ErrInvalid
		}
		var root, kind string
		parents := 0
		identities := map[string]bool{}
		for line := range strings.SplitSeq(header, "\n") {
			key, value, hasValue := strings.Cut(line, " ")
			if !hasValue {
				return nil, ErrInvalid
			}
			if object.Type == "commit" && (key == "author" || key == "committer") {
				if identities[key] || !validGitSignature(value) {
					return nil, ErrInvalid
				}
				identities[key] = true
			}
			if object.Type == "commit" && key == "tree" {
				if root != "" {
					return nil, ErrInvalid
				}
				root, kind = value, "tree"
			}
			if object.Type == "tag" && key == "object" {
				if root != "" {
					return nil, ErrInvalid
				}
				root = value
			}
			if object.Type == "tag" && key == "type" {
				if kind != "" {
					return nil, ErrInvalid
				}
				kind = value
			}
			if object.Type == "commit" && key == "parent" {
				if !objectIDPattern.MatchString(value) || parents >= 64 {
					return nil, ErrInvalid
				}
				links = append(links, objectLink{id: value, kind: "commit"})
				parents++
			}
		}
		if !objectIDPattern.MatchString(root) || (kind != "commit" && kind != "tree" && kind != "blob" && kind != "tag") {
			return nil, ErrInvalid
		}
		if object.Type == "commit" && (!identities["author"] || !identities["committer"]) {
			return nil, ErrInvalid
		}
		links = append(links, objectLink{id: root, kind: kind})
	default:
		return nil, ErrInvalid
	}
	return links, nil
}

func validGitSignature(value string) bool {
	name, rest, ok := strings.Cut(value, " <")
	if !ok || !validIdentityPart(name) {
		return false
	}
	email, timestamp, ok := strings.Cut(rest, "> ")
	if !ok || !validText(email, 254) || strings.ContainsAny(email, "<>") {
		return false
	}
	fields := strings.Fields(timestamp)
	if len(fields) != 2 {
		return false
	}
	seconds, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || seconds < -62135596800 || seconds > 253402300799 {
		return false
	}
	zone := fields[1]
	if len(zone) != 5 || (zone[0] != '+' && zone[0] != '-') {
		return false
	}
	for _, b := range zone[1:] {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}
