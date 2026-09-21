package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	repositoryStateSchema   = 1
	maxRepositoryStateBytes = 1 << 20
)

var repositoryIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// RepositoryState is the small publication point for one immutable generation.
// Snapshot keys are relative to repos/<repo-id>/ and must live below states/.
type RepositoryState struct {
	SchemaVersion int    `json:"schema_version"`
	Generation    uint64 `json:"generation"`
	DefaultBranch string `json:"default_branch"`
	RefsSnapshot  string `json:"refs_snapshot"`
	PackManifest  string `json:"pack_manifest"`
}

type stateEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Snapshot      string          `json:"snapshot"`
	State         RepositoryState `json:"state"`
}

// RepositoryStore is the S3-native repository storage contract used by Git
// serving, push, and maintenance components.
type RepositoryStore interface {
	LoadState(context.Context, string) (RepositoryState, Version, error)
	CompareAndSwapState(context.Context, string, Version, RepositoryState) error
	PutImmutable(context.Context, string, io.Reader, int64) error
	Get(context.Context, string) (io.ReadCloser, error)
	GetRange(context.Context, string, int64, int64) (io.ReadCloser, error)
}

// Store publishes immutable repository generations through one conditional
// mutable state object.
type Store struct {
	objects ObjectStore
}

// NewRepositoryStore constructs a repository store over one shard's bucket.
func NewRepositoryStore(objects ObjectStore) (*Store, error) {
	if objects == nil {
		return nil, errors.New("object store is required")
	}

	return &Store{objects: objects}, nil
}

// LoadState loads one pinned repository generation and its CAS version.
func (s *Store) LoadState(
	ctx context.Context,
	repositoryID string,
) (RepositoryState, Version, error) {
	key, err := repositoryKey(repositoryID, "state")
	if err != nil {
		return RepositoryState{}, "", err
	}

	body, info, err := s.objects.Get(ctx, key)
	if err != nil {
		return RepositoryState{}, "", fmt.Errorf("load repository state: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(body, maxRepositoryStateBytes+1))
	closeErr := body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return RepositoryState{}, "", fmt.Errorf("read repository state: %w", err)
	}
	if len(data) > maxRepositoryStateBytes {
		return RepositoryState{}, "", errors.New("repository state exceeds 1 MiB")
	}
	if info.Version == "" {
		return RepositoryState{}, "", errors.New("repository state has no CAS version")
	}

	var envelope stateEnvelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return RepositoryState{}, "", fmt.Errorf("decode repository state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return RepositoryState{}, "", errors.New("repository state contains trailing JSON")
		}
		return RepositoryState{}, "", fmt.Errorf("decode trailing repository state data: %w", err)
	}
	if envelope.SchemaVersion != repositoryStateSchema {
		return RepositoryState{}, "", fmt.Errorf(
			"unsupported repository state schema %d",
			envelope.SchemaVersion,
		)
	}
	if err := validateState(repositoryID, envelope.State); err != nil {
		return RepositoryState{}, "", fmt.Errorf("validate repository state: %w", err)
	}
	_, expectedSnapshot, err := marshalStateSnapshot(envelope.State)
	if err != nil {
		return RepositoryState{}, "", fmt.Errorf("encode repository state: %w", err)
	}
	if envelope.Snapshot != expectedSnapshot {
		return RepositoryState{}, "", errors.New(
			"repository state snapshot does not match embedded state",
		)
	}

	return envelope.State, info.Version, nil
}

// CompareAndSwapState durably records an immutable state snapshot and then
// conditionally publishes it. An empty expected version creates generation 1.
func (s *Store) CompareAndSwapState(
	ctx context.Context,
	repositoryID string,
	expected Version,
	next RepositoryState,
) error {
	if next.SchemaVersion == 0 {
		next.SchemaVersion = repositoryStateSchema
	}
	if err := validateState(repositoryID, next); err != nil {
		return err
	}
	stateJSON, snapshotName, err := marshalStateSnapshot(next)
	if err != nil {
		return fmt.Errorf("encode repository state: %w", err)
	}
	envelopeJSON, err := json.Marshal(stateEnvelope{
		SchemaVersion: repositoryStateSchema,
		Snapshot:      snapshotName,
		State:         next,
	})
	if err != nil {
		return fmt.Errorf("encode state publication point: %w", err)
	}
	if len(envelopeJSON) > maxRepositoryStateBytes {
		return errors.New("repository state exceeds 1 MiB")
	}
	if err := s.validateTransition(ctx, repositoryID, expected, next.Generation); err != nil {
		return err
	}
	if err := s.requireDurableReferences(ctx, repositoryID, next); err != nil {
		return err
	}

	snapshotKey, err := repositoryKey(repositoryID, snapshotName)
	if err != nil {
		return err
	}
	if _, err := s.objects.Put(
		ctx,
		snapshotKey,
		bytes.NewReader(stateJSON),
		int64(len(stateJSON)),
		PutOptions{IfNoneMatch: true},
	); err != nil {
		if !errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("store immutable repository state: %w", err)
		}
		if err := s.verifyImmutable(ctx, snapshotKey, stateJSON); err != nil {
			return fmt.Errorf("verify immutable repository state: %w", err)
		}
	}

	stateKey, err := repositoryKey(repositoryID, "state")
	if err != nil {
		return err
	}
	options := PutOptions{IfMatch: expected, IfNoneMatch: expected == ""}
	if _, err := s.objects.Put(
		ctx,
		stateKey,
		bytes.NewReader(envelopeJSON),
		int64(len(envelopeJSON)),
		options,
	); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("publish repository state: %w", ErrPreconditionFailed)
		}

		return fmt.Errorf("publish repository state: %w", err)
	}

	return nil
}

func (s *Store) verifyImmutable(ctx context.Context, key string, expected []byte) error {
	body, _, err := s.objects.Get(ctx, key)
	if err != nil {
		return err
	}
	actual, readErr := io.ReadAll(io.LimitReader(body, int64(len(expected))+1))
	closeErr := body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return ErrAlreadyExists
	}

	return nil
}

// PutImmutable uploads a new immutable object within the shard bucket.
func (s *Store) PutImmutable(
	ctx context.Context,
	key string,
	body io.Reader,
	size int64,
) error {
	if _, err := s.objects.Put(
		ctx,
		key,
		body,
		size,
		PutOptions{IfNoneMatch: true},
	); err != nil {
		return fmt.Errorf("put immutable object: %w", err)
	}

	return nil
}

// Get reads a complete object.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	body, _, err := s.objects.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}

	return body, nil
}

// GetRange reads a bounded byte range from an immutable object.
func (s *Store) GetRange(
	ctx context.Context,
	key string,
	offset, length int64,
) (io.ReadCloser, error) {
	body, _, err := s.objects.GetRange(ctx, key, offset, length)
	if err != nil {
		return nil, fmt.Errorf("get object range: %w", err)
	}

	return body, nil
}

func (s *Store) validateTransition(
	ctx context.Context,
	repositoryID string,
	expected Version,
	nextGeneration uint64,
) error {
	current, currentVersion, err := s.LoadState(ctx, repositoryID)
	if expected == "" {
		if err == nil {
			return fmt.Errorf("create repository state: %w", ErrPreconditionFailed)
		}
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("load initial repository state: %w", err)
		}
		if nextGeneration != 1 {
			return fmt.Errorf("initial repository generation must be 1")
		}

		return nil
	}
	if err != nil {
		return fmt.Errorf("load expected repository state: %w", err)
	}
	if currentVersion != expected || current.Generation == ^uint64(0) || nextGeneration != current.Generation+1 {
		return fmt.Errorf("advance repository state: %w", ErrPreconditionFailed)
	}

	return nil
}

func (s *Store) requireDurableReferences(
	ctx context.Context,
	repositoryID string,
	state RepositoryState,
) error {
	for _, relativeKey := range []string{state.RefsSnapshot, state.PackManifest} {
		key, err := repositoryKey(repositoryID, relativeKey)
		if err != nil {
			return err
		}
		if _, err := s.objects.Head(ctx, key); err != nil {
			return fmt.Errorf("repository state references %q: %w", relativeKey, err)
		}
	}

	return nil
}

func validateState(repositoryID string, state RepositoryState) error {
	if !repositoryIDPattern.MatchString(repositoryID) {
		return fmt.Errorf("invalid repository ID")
	}
	if state.SchemaVersion != repositoryStateSchema {
		return fmt.Errorf("repository state schema must be %d", repositoryStateSchema)
	}
	if state.Generation == 0 {
		return fmt.Errorf("repository generation must be positive")
	}
	if !strings.HasPrefix(state.DefaultBranch, "refs/heads/") ||
		!validGitRefName(state.DefaultBranch) {
		return fmt.Errorf("default branch must be a valid refs/heads ref")
	}
	for _, reference := range []struct {
		field string
		value string
	}{
		{field: "refs snapshot", value: state.RefsSnapshot},
		{field: "pack manifest", value: state.PackManifest},
	} {
		if !utf8.ValidString(reference.value) || hasASCIIControl(reference.value) ||
			!strings.HasPrefix(reference.value, "states/") ||
			path.Clean(reference.value) != reference.value {
			return fmt.Errorf("%s must be a canonical states/ key", reference.field)
		}
		if _, err := repositoryKey(repositoryID, reference.value); err != nil {
			return fmt.Errorf("%s must be a canonical states/ key", reference.field)
		}
	}

	return nil
}

func marshalStateSnapshot(state RepositoryState) ([]byte, string, error) {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(stateJSON)
	snapshotName := fmt.Sprintf(
		"states/%020d-state-%s.json",
		state.Generation,
		hex.EncodeToString(digest[:]),
	)

	return stateJSON, snapshotName, nil
}

func validGitRefName(name string) bool {
	if name == "" || name == "@" || !utf8.ValidString(name) {
		return false
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".") || strings.Contains(name, "//") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") ||
		strings.ContainsAny(name, " ~^:?*[\\") || hasASCIIControl(name) {
		return false
	}
	for component := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}

	return true
}

func hasASCIIControl(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] == 0x7f {
			return true
		}
	}

	return false
}

func repositoryKey(repositoryID, relativeKey string) (string, error) {
	if !repositoryIDPattern.MatchString(repositoryID) {
		return "", fmt.Errorf("invalid repository ID")
	}
	key := "repos/" + repositoryID + "/" + relativeKey
	if err := validateKey(key); err != nil {
		return "", fmt.Errorf("repository key: %w", err)
	}

	return key, nil
}

var _ RepositoryStore = (*Store)(nil)
