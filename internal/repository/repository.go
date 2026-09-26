// Package repository creates and browses private repositories in shard storage.
// Authorization belongs to the caller; no operation reads another shard's bucket.
package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

const (
	maxJSONBytes     = 1 << 20
	maxManifestBytes = 64 << 20
	maxObjectBytes   = 1 << 20
	maxRepositories  = 1000
	maxObjects       = 100000
	maxTreeEntries   = 1000
	maxCommits       = 100
)

var (
	ErrInvalid       = errors.New("repository: invalid input")
	ErrNotFound      = errors.New("repository: not found")
	ErrAlreadyExists = errors.New("repository: already exists")
	ErrCorrupt       = errors.New("repository: corrupt data")
	ErrLimit         = errors.New("repository: limit exceeded")
	namePattern      = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$`)
	idPattern        = regexp.MustCompile(`^[a-f0-9]{32}$`)
	objectIDPattern  = regexp.MustCompile(`^[a-f0-9]{40}$`)
	digestPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Metadata describes a repository; its visibility is inherited from its namespace.
type Metadata struct {
	ID            string    `json:"id"`
	Namespace     string    `json:"namespace"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	DefaultBranch string    `json:"defaultBranch"`
	CreatedAt     time.Time `json:"createdAt"`
	CreatedBy     string    `json:"createdBy"`
	Visibility    string    `json:"visibility"`
	IsEmpty       bool      `json:"empty"`
}

// CreateInput supplies repository settings and a verified creator identity.
type CreateInput struct {
	Name             string
	Description      string
	DefaultBranch    string
	InitializeReadme bool
	CreatedBy        string
	AuthorName       string
	AuthorEmail      string
}

type Branch struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

type Entry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type Tree struct {
	Ref     string  `json:"ref"`
	Path    string  `json:"path"`
	Commit  string  `json:"commit"`
	Entries []Entry `json:"entries"`
}

type Blob struct {
	Ref      string `json:"ref"`
	Path     string `json:"path"`
	Commit   string `json:"commit"`
	Content  string `json:"content"`
	Size     int64  `json:"size"`
	IsBinary bool   `json:"binary"`
}

type Commit struct {
	ID         string    `json:"id"`
	Message    string    `json:"message"`
	AuthorName string    `json:"authorName"`
	CreatedAt  time.Time `json:"createdAt"`
	Parents    []string  `json:"parents"`
}

type metadataRecord struct {
	SchemaVersion int      `json:"schemaVersion"`
	Metadata      Metadata `json:"repository"`
}

type refsSnapshot struct {
	SchemaVersion int               `json:"schemaVersion"`
	Refs          map[string]string `json:"refs"`
}

type objectInfo struct {
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	PackKey string `json:"packKey,omitempty"`
	Offset  int64  `json:"offset,omitempty"`
	Length  int64  `json:"length,omitempty"`
	CRC32   uint32 `json:"crc32,omitempty"`
}

// Schema 1 stores loose objects. Schema 2 also indexes immutable canonical packs.
// Strong object digests are independent of Git's SHA-1 identifiers.
type objectManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	ObjectFormat  string                `json:"objectFormat"`
	Objects       map[string]objectInfo `json:"objects"`
}

type snapshot struct {
	metadata Metadata
	refs     refsSnapshot
	manifest objectManifest
	state    storage.RepositoryState
	version  storage.Version
}

// Store uses durable objects and the existing repository publication contract.
type Store struct {
	objects      storage.ObjectStore
	repositories *storage.Store
	parser       *shard.Parser
}

func New(objects storage.ObjectStore) (*Store, error) {
	repositories, err := storage.NewRepositoryStore(objects)
	if err != nil {
		return nil, err
	}
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		return nil, err
	}
	return &Store{objects: objects, repositories: repositories, parser: parser}, nil
}

// ValidName reports whether a repository name is canonical and route-safe.
func ValidName(name string) bool {
	if !namePattern.MatchString(name) || strings.HasSuffix(name, ".git") || strings.Contains(name, "..") {
		return false
	}
	switch name {
	case "auth", "settings", "members", "invitations":
		return false
	default:
		return true
	}
}

func (s *Store) validNamespace(namespace string) bool {
	if namespace == "auth" || strings.Contains(namespace, "/") {
		return false
	}
	_, err := s.parser.Parse(&http.Request{URL: &url.URL{Path: "/" + namespace}})
	return err == nil
}

// Create initializes durable Git state before atomically claiming its name.
// Failed or losing creations can leave unreachable objects for future garbage
// collection, but never publish a half-created or overwritten repository.
func (s *Store) Create(ctx context.Context, namespace string, input CreateInput) (Metadata, error) {
	if input.DefaultBranch == "" {
		input.DefaultBranch = "main"
	}
	if !s.validNamespace(namespace) || !ValidName(input.Name) || !validBranch(input.DefaultBranch) ||
		!validDescription(input.Description) || !validText(input.CreatedBy, 512) || input.CreatedBy == "" {
		return Metadata{}, ErrInvalid
	}
	if input.InitializeReadme && (!validIdentityPart(input.AuthorName) || !validIdentityPart(input.AuthorEmail)) {
		return Metadata{}, fmt.Errorf("%w: invalid commit identity", ErrInvalid)
	}
	if _, err := s.objects.Head(ctx, metadataKey(namespace, input.Name)); err == nil {
		return Metadata{}, ErrAlreadyExists
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Metadata{}, fmt.Errorf("check repository name: %w", err)
	}

	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return Metadata{}, fmt.Errorf("generate repository id: %w", err)
	}
	metadata := Metadata{
		ID: hex.EncodeToString(randomID[:]), Namespace: namespace, Name: input.Name,
		Description: input.Description, DefaultBranch: input.DefaultBranch,
		CreatedAt: time.Now().UTC().Truncate(time.Second), CreatedBy: input.CreatedBy,
		Visibility: "private", IsEmpty: !input.InitializeReadme,
	}
	refs := refsSnapshot{SchemaVersion: 1, Refs: map[string]string{}}
	manifest := objectManifest{SchemaVersion: 1, ObjectFormat: "sha1", Objects: map[string]objectInfo{}}
	if input.InitializeReadme {
		commit, err := s.initializeReadme(ctx, metadata, input, &manifest)
		if err != nil {
			return Metadata{}, err
		}
		refs.Refs["refs/heads/"+input.DefaultBranch] = commit
	}
	refsKey, err := s.putSnapshot(ctx, metadata.ID, "refs", refs)
	if err != nil {
		return Metadata{}, err
	}
	manifestKey, err := s.putSnapshot(ctx, metadata.ID, "manifest", manifest)
	if err != nil {
		return Metadata{}, err
	}
	state := storage.RepositoryState{SchemaVersion: 1, Generation: 1, DefaultBranch: "refs/heads/" + input.DefaultBranch, RefsSnapshot: refsKey, PackManifest: manifestKey}
	if err := s.repositories.CompareAndSwapState(ctx, metadata.ID, "", state); err != nil {
		return Metadata{}, fmt.Errorf("publish initial repository state: %w", err)
	}
	data, err := json.Marshal(metadataRecord{SchemaVersion: 1, Metadata: metadata})
	if err != nil {
		return Metadata{}, fmt.Errorf("encode repository metadata: %w", err)
	}
	if _, err := s.objects.Put(ctx, metadataKey(namespace, input.Name), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfNoneMatch: true}); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrPreconditionFailed) {
			return Metadata{}, ErrAlreadyExists
		}
		if errors.Is(err, storage.ErrConditionalConflict) {
			if _, headErr := s.objects.Head(ctx, metadataKey(namespace, input.Name)); headErr == nil {
				return Metadata{}, ErrAlreadyExists
			}
		}
		return Metadata{}, fmt.Errorf("claim repository name: %w", err)
	}
	return metadata, nil
}

func (s *Store) List(ctx context.Context, namespace string) ([]Metadata, error) {
	if !s.validNamespace(namespace) {
		return nil, ErrInvalid
	}
	prefix := "repositories/" + namespace + "/"
	objects, err := s.objects.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	if len(objects) > maxRepositories {
		return nil, ErrLimit
	}
	result := make([]Metadata, 0, len(objects))
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(object.Key, prefix), ".json")
		if !ValidName(name) || object.Key != metadataKey(namespace, name) {
			return nil, ErrCorrupt
		}
		metadata, err := s.Get(ctx, namespace, name)
		if err != nil {
			return nil, err
		}
		result = append(result, metadata)
	}
	slices.SortFunc(result, func(a, b Metadata) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

func (s *Store) Get(ctx context.Context, namespace, name string) (Metadata, error) {
	snapshot, err := s.load(ctx, namespace, name)
	if err != nil {
		return Metadata{}, err
	}
	return snapshot.metadata, nil
}

func (s *Store) load(ctx context.Context, namespace, name string) (snapshot, error) {
	if !s.validNamespace(namespace) || !ValidName(name) {
		return snapshot{}, ErrInvalid
	}
	data, err := s.read(ctx, metadataKey(namespace, name), maxJSONBytes)
	if errors.Is(err, storage.ErrNotFound) {
		return snapshot{}, ErrNotFound
	}
	if err != nil {
		return snapshot{}, err
	}
	var record metadataRecord
	if err := decodeJSON(data, &record); err != nil {
		return snapshot{}, err
	}
	m := record.Metadata
	if record.SchemaVersion != 1 || m.Namespace != namespace || m.Name != name || !idPattern.MatchString(m.ID) ||
		m.Visibility != "private" || !validBranch(m.DefaultBranch) || m.CreatedAt.IsZero() ||
		!validDescription(m.Description) || m.CreatedBy == "" || !validText(m.CreatedBy, 512) {
		return snapshot{}, ErrCorrupt
	}
	state, version, err := s.repositories.LoadState(ctx, m.ID)
	if err != nil {
		return snapshot{}, fmt.Errorf("load published repository state: %w", errors.Join(ErrCorrupt, err))
	}
	result := snapshot{metadata: m, state: state, version: version}
	if err := s.readSnapshot(ctx, m.ID, state.RefsSnapshot, &result.refs); err != nil {
		return snapshot{}, err
	}
	if err := s.readManifest(ctx, m.ID, state.PackManifest, &result.manifest); err != nil {
		return snapshot{}, err
	}
	if result.refs.SchemaVersion != 1 || result.refs.Refs == nil || len(result.refs.Refs) > maxObjects ||
		(result.manifest.SchemaVersion != 1 && result.manifest.SchemaVersion != 2) || result.manifest.ObjectFormat != "sha1" || result.manifest.Objects == nil || len(result.manifest.Objects) > maxObjects {
		return snapshot{}, ErrCorrupt
	}
	for id, info := range result.manifest.Objects {
		if !validObjectInfo(id, info) || (result.manifest.SchemaVersion == 1 && info.PackKey != "") {
			return snapshot{}, ErrCorrupt
		}
	}
	for ref, id := range result.refs.Refs {
		if !ValidRef(ref) || !objectIDPattern.MatchString(id) || result.manifest.Objects[id].Type == "" || (strings.HasPrefix(ref, "refs/heads/") && result.manifest.Objects[id].Type != "commit") {
			return snapshot{}, ErrCorrupt
		}
	}
	result.metadata.DefaultBranch = strings.TrimPrefix(state.DefaultBranch, "refs/heads/")
	if !validBranch(result.metadata.DefaultBranch) {
		return snapshot{}, ErrCorrupt
	}
	result.metadata.IsEmpty = len(result.refs.Refs) == 0
	return result, nil
}

func (s *Store) read(ctx context.Context, key string, limit int64) ([]byte, error) {
	body, info, err := s.objects.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read repository object: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(body, limit+1))
	closeErr := body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("read repository object: %w", err)
	}
	if len(data) > int(limit) || int64(len(data)) != info.Size {
		return nil, ErrCorrupt
	}
	return data, nil
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: invalid json: %w", ErrCorrupt, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%w: trailing json", ErrCorrupt)
	}
	return nil
}

func metadataKey(namespace, name string) string {
	return "repositories/" + namespace + "/" + name + ".json"
}

func validText(text string, limit int) bool {
	return len(text) <= limit && utf8.ValidString(text) && !strings.ContainsFunc(text, unicode.IsControl)
}

func validDescription(text string) bool {
	return len(text) <= 2000 && utf8.ValidString(text) && utf8.RuneCountInString(text) <= 500 &&
		!strings.ContainsFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' })
}

func validIdentityPart(text string) bool {
	return text != "" && validText(text, 254) && !strings.ContainsAny(text, "<>")
}

func validBranch(branch string) bool {
	if branch == "" || len(branch) > 128 || !utf8.ValidString(branch) || strings.HasPrefix(branch, "-") ||
		strings.ContainsAny(branch, " ~^:?*[\\") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") ||
		strings.HasSuffix(branch, ".") || strings.ContainsFunc(branch, unicode.IsControl) {
		return false
	}
	for component := range strings.SplitSeq(branch, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}
