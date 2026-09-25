package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

const tokenRecordLimit = 32 << 10

var errInvalidToken = errors.New("invalid or expired access token")

type tokenView struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Username        string     `json:"username"`
	Permission      string     `json:"permission" enum:"read,write"`
	AllRepositories bool       `json:"allRepositories,omitempty"`
	Repositories    []string   `json:"repositories"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	RevokedAt       *time.Time `json:"revokedAt,omitempty"`
}

type tokenRecord struct {
	SchemaVersion int       `json:"schemaVersion"`
	Metadata      tokenView `json:"metadata"`
	Identity      Identity  `json:"identity"`
	Digest        string    `json:"digest"`
}

// The secret is uniformly random: SHA-256 is a verifier, not a password KDF.
// Its public username/ID prefix only locates the authoritative shard record.
func splitToken(raw string) (username, id string, ok bool) {
	if len(raw) > 160 {
		return "", "", false
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 4 || parts[0] != "gitone_pat_v1" || !validTokenID(parts[2]) {
		return "", "", false
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != parts[3] {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func validTokenID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}

func tokenKey(username, id string) string { return "auth/tokens/" + username + "/" + id + ".json" }

func sortedScopes(scopes []string) []string {
	scopes = slices.Clone(scopes)
	slices.Sort(scopes)
	return scopes
}

func (s *Service) validTokenMetadata(view tokenView) bool {
	owner, err := s.router.Owner(view.Username)
	if err != nil || owner != s.local || view.Username == "auth" || !validTokenID(view.ID) ||
		strings.TrimSpace(view.Name) == "" || len(view.Name) > 100 || strings.ContainsFunc(view.Name, unicode.IsControl) ||
		(view.Permission != "read" && view.Permission != "write") ||
		view.CreatedAt.IsZero() || !view.ExpiresAt.After(view.CreatedAt) || view.ExpiresAt.Sub(view.CreatedAt) > 90*24*time.Hour ||
		(view.RevokedAt != nil && view.RevokedAt.Before(view.CreatedAt)) {
		return false
	}
	return s.validTokenScopes(view.AllRepositories, view.Repositories)
}

// All-repository scope is explicit, not inferred from an empty selection.
// Older records without the flag therefore remain restricted to their list.
func (s *Service) validTokenScopes(all bool, scopes []string) bool {
	if all {
		return len(scopes) == 0
	}
	if len(scopes) == 0 || len(scopes) > 100 {
		return false
	}
	for i, scope := range scopes {
		namespace, repo, found := strings.Cut(scope, "/")
		if _, err := s.router.Owner(namespace); !found || err != nil || namespace == "auth" || !repository.ValidName(repo) {
			return false
		}
		if i > 0 && scopes[i-1] >= scope {
			return false
		}
	}
	return true
}

func (s *Service) createToken(ctx context.Context, username string, identity Identity, name, permission string, scopes []string, allRepositories bool, days int) (string, tokenView, error) {
	idBytes := make([]byte, 16)
	secret := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return "", tokenView{}, err
	}
	if _, err := rand.Read(secret); err != nil {
		return "", tokenView{}, err
	}
	scopes = sortedScopes(scopes)
	if scopes == nil {
		scopes = []string{}
	}
	now := time.Now().UTC()
	view := tokenView{ID: hex.EncodeToString(idBytes), Name: strings.TrimSpace(name), Username: username,
		Permission: permission, AllRepositories: allRepositories, Repositories: scopes, CreatedAt: now, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}
	if days < 1 || days > 90 || !validIdentity(identity) || !s.validTokenMetadata(view) {
		return "", tokenView{}, errors.New("invalid token settings")
	}
	raw := "gitone_pat_v1." + username + "." + view.ID + "." + base64.RawURLEncoding.EncodeToString(secret)
	digest := sha256.Sum256([]byte(raw))
	record := tokenRecord{SchemaVersion: 1, Metadata: view, Identity: Identity{Issuer: identityIssuer(identity), Subject: identity.Subject}, Digest: hex.EncodeToString(digest[:])}
	data, err := json.Marshal(record)
	if err != nil {
		return "", tokenView{}, err
	}
	_, err = s.store.Put(ctx, tokenKey(username, view.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfNoneMatch: true})
	if err != nil {
		return "", tokenView{}, err
	}
	return raw, view, nil
}

func (s *Service) loadToken(ctx context.Context, username, id string) (tokenRecord, storage.Version, error) {
	var record tokenRecord
	owner, err := s.router.Owner(username)
	if err != nil || username == "auth" || owner != s.local || !validTokenID(id) {
		return record, "", errInvalidToken
	}
	body, info, err := s.store.Get(ctx, tokenKey(username, id))
	if err != nil {
		return record, "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(body, tokenRecordLimit+1))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return record, "", err
	}
	if len(data) > tokenRecordLimit || json.Unmarshal(data, &record) != nil || record.SchemaVersion != 1 ||
		record.Metadata.Username != username || record.Metadata.ID != id || !s.validTokenMetadata(record.Metadata) || !validIdentity(record.Identity) || info.Version == "" {
		return tokenRecord{}, "", errors.New("invalid stored token record")
	}
	digest, err := hex.DecodeString(record.Digest)
	if err != nil || len(digest) != sha256.Size {
		return tokenRecord{}, "", errors.New("invalid stored token digest")
	}
	return record, info.Version, nil
}

func (s *Service) verifyLocalToken(ctx context.Context, username, raw string) (tokenRecord, error) {
	routeName, id, ok := splitToken(raw)
	if !ok || routeName != username {
		return tokenRecord{}, errInvalidToken
	}
	record, _, err := s.loadToken(ctx, username, id)
	if errors.Is(err, storage.ErrNotFound) {
		return tokenRecord{}, errInvalidToken
	}
	if err != nil {
		return tokenRecord{}, err
	}
	digest := sha256.Sum256([]byte(raw))
	expected, _ := hex.DecodeString(record.Digest)
	if subtle.ConstantTimeCompare(digest[:], expected) != 1 || record.Metadata.RevokedAt != nil || !record.Metadata.ExpiresAt.After(time.Now()) || identityIssuer(record.Identity) != s.issuer {
		return tokenRecord{}, errInvalidToken
	}
	namespace, _, err := s.loadNamespace(ctx, username)
	if errors.Is(err, storage.ErrNotFound) {
		return tokenRecord{}, errInvalidToken
	}
	if err != nil {
		return tokenRecord{}, err
	}
	if namespace.Type != userNamespace || userID(namespace.Identity) != userID(record.Identity) {
		return tokenRecord{}, errInvalidToken
	}
	return record, nil
}

func (s *Service) listTokens(ctx context.Context, username string) ([]tokenView, error) {
	views, _, err := s.listTokenPage(ctx, username, "")
	return views, err
}

func (s *Service) listTokenPage(ctx context.Context, username, after string) ([]tokenView, string, error) {
	prefix := "auth/tokens/" + username + "/"
	items, err := s.store.List(ctx, prefix)
	if err != nil {
		return nil, "", err
	}
	slices.SortFunc(items, func(a, b storage.ObjectInfo) int { return strings.Compare(a.Key, b.Key) })
	views := make([]tokenView, 0, min(len(items), 1000))
	for _, item := range items {
		id := strings.TrimSuffix(strings.TrimPrefix(item.Key, prefix), ".json")
		if item.Key != tokenKey(username, id) || !validTokenID(id) {
			return nil, "", errors.New("invalid token record key")
		}
		if id <= after {
			continue
		}
		if len(views) == 1000 {
			return views, views[len(views)-1].ID, nil
		}
		record, _, err := s.loadToken(ctx, username, id)
		if err != nil {
			return nil, "", err
		}
		views = append(views, record.Metadata)
	}
	return views, "", nil
}

func (s *Service) revokeToken(ctx context.Context, username, id string) error {
	for range 8 {
		record, version, err := s.loadToken(ctx, username, id)
		if err != nil {
			return err
		}
		if record.Metadata.RevokedAt != nil {
			return nil
		}
		now := time.Now().UTC()
		record.Metadata.RevokedAt = &now
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		_, err = s.store.Put(ctx, tokenKey(username, id), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version})
		if !isNamespaceConflict(err) {
			return err
		}
	}
	return storage.ErrConditionalConflict
}
