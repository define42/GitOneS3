package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

const (
	// DefaultGCGracePeriod leaves recently uploaded orphan artifacts available
	// for inspection. A repository lock supplies concurrency safety; age alone
	// is never treated as evidence that an artifact is safe to delete.
	DefaultGCGracePeriod    = 24 * time.Hour
	maxMaintenanceArtifacts = 1_000_000
)

var (
	stateSnapshotPattern = regexp.MustCompile(`^states/[0-9]{20}-state-[a-f0-9]{64}\.json$`)
	dataSnapshotPattern  = regexp.MustCompile(`^states/[0-9]{20}-(refs|manifest)-[a-f0-9]{64}\.json$`)
	packKeyPattern       = regexp.MustCompile(`^packs/[a-f0-9]{64}\.pack$`)
	looseKeyPattern      = regexp.MustCompile(`^objects/[a-f0-9]{2}/[a-f0-9]{38}$`)
)

// IntegrityReport describes one fully verified repository generation.
type IntegrityReport struct {
	RepositoryID string `json:"repositoryId"`
	Generation   uint64 `json:"generation"`
	References   int    `json:"references"`
	Objects      int    `json:"objects"`
	Bytes        int64  `json:"bytes"`
	Packs        int    `json:"packs"`
}

// Generation identifies an immutable, recoverable state snapshot. A snapshot
// may be an abandoned publication proposal; only Current proves publication.
// Operators choose an exact Snapshot key to disambiguate competing proposals.
type Generation struct {
	Generation    uint64    `json:"generation"`
	Snapshot      string    `json:"snapshot"`
	Current       bool      `json:"current"`
	CreatedAt     time.Time `json:"createdAt"`
	DefaultBranch string    `json:"defaultBranch"`
}

// GCOptions controls orphan collection. The zero value performs a dry run
// with DefaultGCGracePeriod. Apply must explicitly enable deletion.
type GCOptions struct {
	Apply       bool
	GracePeriod time.Duration
}

// GCReport reports the planned and completed work, including partial progress
// when a conditional delete or storage operation fails.
type GCReport struct {
	RepositoryID        string        `json:"repositoryId"`
	DryRun              bool          `json:"dryRun"`
	GracePeriod         time.Duration `json:"gracePeriod"`
	RetainedGenerations int           `json:"retainedGenerations"`
	RetainedArtifacts   int           `json:"retainedArtifacts"`
	Candidates          int           `json:"candidates"`
	CandidateBytes      int64         `json:"candidateBytes"`
	Deleted             int           `json:"deleted"`
	DeletedBytes        int64         `json:"deletedBytes"`
	RecentArtifacts     int           `json:"recentArtifacts"`
}

// RestoreReport describes a restore published as a new generation.
type RestoreReport struct {
	RepositoryID     string `json:"repositoryId"`
	SourceSnapshot   string `json:"sourceSnapshot"`
	SourceGeneration uint64 `json:"sourceGeneration"`
	Generation       uint64 `json:"generation"`
}

// CheckIntegrity verifies snapshot digests, every object's Git SHA-1 and
// SHA-256, and typed graph connectivity. It reads one object body at a time.
func (s *Store) CheckIntegrity(ctx context.Context, namespace, name string) (IntegrityReport, error) {
	snap, err := s.load(ctx, namespace, name)
	if err != nil {
		return IntegrityReport{}, err
	}
	key, err := maintenanceStateKey(snap.state)
	if err != nil {
		return IntegrityReport{}, err
	}
	verified, err := s.readMaintenanceSnapshot(ctx, snap.metadata, key)
	if err != nil {
		return IntegrityReport{}, err
	}
	return s.verifyMaintenanceSnapshot(ctx, verified)
}

// ListGenerations lists retained state snapshots. All are retained by GC so
// an already pinned reader remains valid for any duration. This includes
// abandoned proposals written before a failed compare-and-swap publication.
func (s *Store) ListGenerations(ctx context.Context, namespace, name string) ([]Generation, error) {
	snap, err := s.maintenanceBase(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	artifacts, err := s.maintenanceArtifacts(ctx, snap.metadata.ID)
	if err != nil {
		return nil, err
	}
	current, err := maintenanceStateKey(snap.state)
	if err != nil {
		return nil, err
	}
	result := []Generation{}
	prefix := "repos/" + snap.metadata.ID + "/"
	foundCurrent := false
	for _, artifact := range artifacts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := strings.TrimPrefix(artifact.Key, prefix)
		if !isStateArtifact(key) {
			continue
		}
		state, err := s.readMaintenanceState(ctx, snap.metadata.ID, key)
		if err != nil {
			return nil, err
		}
		foundCurrent = foundCurrent || key == current
		result = append(result, Generation{Generation: state.Generation, Snapshot: key, Current: key == current, CreatedAt: artifact.LastModified, DefaultBranch: state.DefaultBranch})
	}
	if !foundCurrent {
		return nil, fmt.Errorf("current immutable state snapshot is missing: %w", ErrCorrupt)
	}
	slices.SortFunc(result, func(a, b Generation) int { return strings.Compare(a.Snapshot, b.Snapshot) })
	return result, nil
}

// RestoreGeneration verifies a retained snapshot before atomically publishing
// its refs and manifest as a new generation. Existing history remains intact.
func (s *Store) RestoreGeneration(ctx context.Context, namespace, name, source string) (report RestoreReport, err error) {
	if !stateSnapshotPattern.MatchString(source) {
		return report, ErrInvalid
	}
	base, err := s.maintenanceBase(ctx, namespace, name)
	if err != nil {
		return report, err
	}
	unlock, err := s.lockRepository(ctx, base.metadata.ID)
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	base, err = s.maintenanceBase(ctx, namespace, name)
	if err != nil {
		return report, err
	}
	target, err := s.readMaintenanceSnapshot(ctx, base.metadata, source)
	if err != nil {
		return report, err
	}
	if _, err := s.verifyMaintenanceSnapshot(ctx, target); err != nil {
		return report, err
	}
	if base.state.Generation == ^uint64(0) {
		return report, ErrLimit
	}
	next := target.state
	next.Generation = base.state.Generation + 1
	if err := s.repositories.CompareAndSwapState(ctx, base.metadata.ID, base.version, next); err != nil {
		if errors.Is(err, storage.ErrPreconditionFailed) || errors.Is(err, storage.ErrConditionalConflict) {
			return report, ErrConflict
		}
		return report, fmt.Errorf("publish restored repository state: %w", err)
	}
	return RestoreReport{RepositoryID: base.metadata.ID, SourceSnapshot: source, SourceGeneration: target.state.Generation, Generation: next.Generation}, nil
}

// GarbageCollect removes only orphan artifacts, while retaining every durable
// state snapshot and everything referenced by those snapshots. It holds the
// same durable lock as writers and restore throughout marking and deletion.
// All retained graphs must pass integrity checks before any deletion begins.
func (s *Store) GarbageCollect(ctx context.Context, namespace, name string, options GCOptions) (report GCReport, err error) {
	if options.GracePeriod < 0 {
		return report, ErrInvalid
	}
	if options.GracePeriod == 0 {
		options.GracePeriod = DefaultGCGracePeriod
	}
	snap, err := s.load(ctx, namespace, name)
	if err != nil {
		return report, err
	}
	report = GCReport{RepositoryID: snap.metadata.ID, DryRun: !options.Apply, GracePeriod: options.GracePeriod}
	unlock, err := s.lockRepository(ctx, snap.metadata.ID)
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	// Re-read after acquiring the lock: a writer could have completed between
	// the initial lookup and successful lock creation.
	snap, err = s.load(ctx, namespace, name)
	if err != nil {
		return report, err
	}
	artifacts, err := s.maintenanceArtifacts(ctx, snap.metadata.ID)
	if err != nil {
		return report, err
	}
	current, err := maintenanceStateKey(snap.state)
	if err != nil {
		return report, err
	}
	prefix := "repos/" + snap.metadata.ID + "/"
	marked := map[string]bool{"state": true, "maintenance-lock": true}
	for _, artifact := range artifacts {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		key := strings.TrimPrefix(artifact.Key, prefix)
		if !isStateArtifact(key) {
			continue
		}
		retained, err := s.readMaintenanceSnapshot(ctx, snap.metadata, key)
		if err != nil {
			return report, err
		}
		if _, err := s.verifyMaintenanceSnapshot(ctx, retained); err != nil {
			return report, fmt.Errorf("verify retained snapshot %s: %w", key, err)
		}
		marked[key] = true
		marked[retained.state.RefsSnapshot] = true
		marked[retained.state.PackManifest] = true
		for id, info := range retained.manifest.Objects {
			if info.PackKey != "" {
				marked[info.PackKey] = true
			} else {
				marked[strings.TrimPrefix(objectKey(snap.metadata.ID, id), prefix)] = true
			}
		}
		report.RetainedGenerations++
	}
	if !marked[current] {
		return report, fmt.Errorf("current immutable state snapshot is missing: %w", ErrCorrupt)
	}
	report.RetainedArtifacts = len(marked) - 1 // The temporary lock is not retained data.
	cutoff := time.Now().Add(-options.GracePeriod)
	candidates := make([]storage.ObjectInfo, 0)
	for _, artifact := range artifacts {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		key := strings.TrimPrefix(artifact.Key, prefix)
		if marked[key] || !collectibleArtifact(key) {
			continue
		}
		if artifact.LastModified.IsZero() || !artifact.LastModified.Before(cutoff) {
			report.RecentArtifacts++
			continue
		}
		if artifact.Version == "" || artifact.Size < 0 {
			return report, fmt.Errorf("artifact has no conditional delete version or valid size: %w", ErrCorrupt)
		}
		if artifact.Size > math.MaxInt64-report.CandidateBytes {
			return report, ErrLimit
		}
		candidates = append(candidates, artifact)
		report.Candidates++
		report.CandidateBytes += artifact.Size
	}
	if !options.Apply {
		return report, nil
	}
	for _, artifact := range candidates {
		if err := s.objects.Delete(ctx, artifact.Key, artifact.Version); err != nil {
			return report, fmt.Errorf("delete orphan artifact %s: %w", artifact.Key, err)
		}
		report.Deleted++
		report.DeletedBytes += artifact.Size
	}
	return report, nil
}

func (s *Store) maintenanceArtifacts(ctx context.Context, repositoryID string) ([]storage.ObjectInfo, error) {
	prefix := "repos/" + repositoryID + "/"
	result := []storage.ObjectInfo{}
	after := ""
	for {
		page, err := s.objects.ListPage(ctx, prefix, after, storage.MaxListPageSize)
		if err != nil {
			return nil, fmt.Errorf("list repository artifacts: %w", err)
		}
		if len(result)+len(page.Objects) > maxMaintenanceArtifacts {
			return nil, ErrLimit
		}
		for _, artifact := range page.Objects {
			if !strings.HasPrefix(artifact.Key, prefix) || artifact.Key <= after {
				return nil, ErrCorrupt
			}
			after = artifact.Key
			result = append(result, artifact)
		}
		if page.NextAfter == "" {
			return result, nil
		}
		if len(page.Objects) == 0 || page.NextAfter != after {
			return nil, ErrCorrupt
		}
	}
}

func isStateArtifact(key string) bool {
	return strings.HasPrefix(key, "states/") && strings.Contains(key, "-state-")
}

func collectibleArtifact(key string) bool {
	return looseKeyPattern.MatchString(key) || packKeyPattern.MatchString(key) || dataSnapshotPattern.MatchString(key)
}

func maintenanceStateKey(state storage.RepositoryState) (string, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("encode retained repository state: %w", err)
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("states/%020d-state-%s.json", state.Generation, hex.EncodeToString(digest[:])), nil
}

func (s *Store) readMaintenanceState(ctx context.Context, repositoryID, key string) (storage.RepositoryState, error) {
	var state storage.RepositoryState
	if !stateSnapshotPattern.MatchString(key) {
		return state, ErrCorrupt
	}
	if err := s.readSnapshot(ctx, repositoryID, key, &state); err != nil {
		return state, err
	}
	expectedKey, err := maintenanceStateKey(state)
	if err != nil {
		return state, err
	}
	if expectedKey != key || state.SchemaVersion != 1 || state.Generation == 0 ||
		!strings.HasPrefix(state.DefaultBranch, "refs/heads/") || !ValidRef(state.DefaultBranch) ||
		!dataSnapshotPattern.MatchString(state.RefsSnapshot) || !strings.Contains(state.RefsSnapshot, "-refs-") ||
		!dataSnapshotPattern.MatchString(state.PackManifest) || !strings.Contains(state.PackManifest, "-manifest-") {
		return state, ErrCorrupt
	}
	return state, nil
}

func (s *Store) readMaintenanceSnapshot(ctx context.Context, metadata Metadata, key string) (snapshot, error) {
	state, err := s.readMaintenanceState(ctx, metadata.ID, key)
	if err != nil {
		return snapshot{}, err
	}
	result := snapshot{metadata: metadata, state: state}
	if err := s.readSnapshot(ctx, metadata.ID, state.RefsSnapshot, &result.refs); err != nil {
		return snapshot{}, err
	}
	if err := s.readManifest(ctx, metadata.ID, state.PackManifest, &result.manifest); err != nil {
		return snapshot{}, err
	}
	if result.refs.SchemaVersion != 1 || result.refs.Refs == nil || len(result.refs.Refs) > maxObjects ||
		(result.manifest.SchemaVersion != 1 && result.manifest.SchemaVersion != 2) || result.manifest.ObjectFormat != "sha1" || result.manifest.Objects == nil || len(result.manifest.Objects) > maxObjects {
		return snapshot{}, ErrCorrupt
	}
	var total int64
	for id, info := range result.manifest.Objects {
		if !validObjectInfo(id, info) || (result.manifest.SchemaVersion == 1 && info.PackKey != "") {
			return snapshot{}, ErrCorrupt
		}
		total += info.Size
		if total > MaxGitBytes {
			return snapshot{}, ErrLimit
		}
	}
	for ref, id := range result.refs.Refs {
		info, ok := result.manifest.Objects[id]
		if !ValidRef(ref) || !ok || (strings.HasPrefix(ref, "refs/heads/") && info.Type != "commit") {
			return snapshot{}, ErrCorrupt
		}
		parts := strings.Split(ref, "/")
		for i := 2; i < len(parts); i++ {
			if _, exists := result.refs.Refs[strings.Join(parts[:i], "/")]; exists {
				return snapshot{}, ErrCorrupt
			}
		}
	}
	return result, nil
}

func (s *Store) verifyMaintenanceSnapshot(ctx context.Context, snap snapshot) (report IntegrityReport, err error) {
	report = IntegrityReport{RepositoryID: snap.metadata.ID, Generation: snap.state.Generation, References: len(snap.refs.Refs), Objects: len(snap.manifest.Objects)}
	reader, err := s.openGitSnapshot(snap)
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	packs := map[string]bool{}
	for id, info := range snap.manifest.Objects {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		object, err := reader.Get(ctx, id)
		if err != nil {
			return report, fmt.Errorf("verify object %s: %w", id, err)
		}
		links, err := gitLinks(GitObject{Type: object.Type, Data: object.Data})
		if err != nil {
			return report, fmt.Errorf("verify graph at object %s: %w", id, errors.Join(ErrCorrupt, err))
		}
		for _, link := range links {
			target, ok := snap.manifest.Objects[link.id]
			if !ok || target.Type != link.kind {
				return report, fmt.Errorf("object %s has missing or mistyped target %s: %w", id, link.id, ErrCorrupt)
			}
		}
		report.Bytes += info.Size
		if info.PackKey != "" && !packs[info.PackKey] {
			if err := s.verifyMaintenancePack(ctx, snap.metadata.ID, info.PackKey, reader.cached[info.PackKey]); err != nil {
				return report, err
			}
			packs[info.PackKey] = true
		}
	}
	report.Packs = len(packs)
	return report, nil
}

// maintenanceMetadata intentionally does not load current refs or objects: an
// operator must still be able to restore a good generation when those artifacts
// in the current generation are damaged.
func (s *Store) maintenanceMetadata(ctx context.Context, namespace, name string) (Metadata, error) {
	if !s.validNamespace(namespace) || !ValidName(name) {
		return Metadata{}, ErrInvalid
	}
	data, err := s.read(ctx, metadataKey(namespace, name), maxJSONBytes)
	if errors.Is(err, storage.ErrNotFound) {
		return Metadata{}, ErrNotFound
	}
	if err != nil {
		return Metadata{}, err
	}
	var record metadataRecord
	if err := decodeJSON(data, &record); err != nil {
		return Metadata{}, err
	}
	m := record.Metadata
	if record.SchemaVersion != 1 || m.Namespace != namespace || m.Name != name || !idPattern.MatchString(m.ID) ||
		m.Visibility != "private" || !validBranch(m.DefaultBranch) || m.CreatedAt.IsZero() ||
		!validDescription(m.Description) || m.CreatedBy == "" || !validText(m.CreatedBy, 512) {
		return Metadata{}, ErrCorrupt
	}
	return m, nil
}

func (s *Store) maintenanceBase(ctx context.Context, namespace, name string) (snapshot, error) {
	metadata, err := s.maintenanceMetadata(ctx, namespace, name)
	if err != nil {
		return snapshot{}, err
	}
	state, version, err := s.repositories.LoadState(ctx, metadata.ID)
	if err != nil {
		return snapshot{}, fmt.Errorf("load maintenance publication point: %w", errors.Join(ErrCorrupt, err))
	}
	return snapshot{metadata: metadata, state: state, version: version}, nil
}
