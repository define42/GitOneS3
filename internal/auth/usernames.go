package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

const usernameLookupPath = "/api/internal/v1/usernames/resolve"
const usernameLookupProofName = "gitone-group-usernames-v1"
const maxGroupUsernameCacheBytes = 1 << 20
const unresolvedUsernameLifetime = 5 * time.Minute

type groupUsernameCache struct {
	SchemaVersion int               `json:"schemaVersion"`
	Usernames     map[string]string `json:"usernames"`
	Unresolved    map[string]int64  `json:"unresolved,omitempty"`
}

func groupUsernameCacheKey(name string) string {
	return "auth/group-usernames/v1/" + name + ".json"
}

type usernameLookupRequest struct {
	UserIDs  []string          `json:"userIds"`
	Expected map[string]string `json:"expected,omitempty"`
}

type usernameLookupResponse struct {
	Usernames map[string]string `json:"usernames"`
}

type usernameLookupProof struct {
	Digest  string `json:"digest"`
	Expires int64  `json:"expires"`
}

// groupView resolves usernames from user namespace records, which remain the
// authority even for groups written before names were included in responses.
// A failed lookup leaves its name absent so the UI cannot misidentify an ID.
func (s *Service) groupView(ctx context.Context, name string, record namespaceRecord, current session) (*groupView, error) {
	view := &groupView{
		Name: name, Type: groupNamespace, CreatorUserID: record.CreatorUserID,
		Role: record.Members[userID(current.Identity)], Members: record.Members,
		MemberUsernames: make(map[string]string), CSRF: current.CSRF,
	}
	if view.Role == "owner" {
		view.Invitations = record.Invitations
		view.InvitationUsernames = make(map[string]string)
	}
	wanted := make(map[string]struct{}, len(record.Members)+len(view.Invitations))
	for id := range record.Members {
		wanted[id] = struct{}{}
	}
	for id := range view.Invitations {
		wanted[id] = struct{}{}
	}
	cache, cacheVersion, err := s.loadGroupUsernameCache(ctx, name)
	if err != nil {
		return nil, err
	}
	// The cache tracks every current participant, including invitations hidden
	// from non-owners. Only visible IDs enter wanted or the response below.
	usernames := make(map[string]string, len(record.Members)+len(record.Invitations))
	unresolved := make(map[string]int64)
	for id, username := range cache.Usernames {
		if record.Members[id] != "" || record.Invitations[id] != "" {
			usernames[id] = username
			delete(wanted, id)
		}
	}
	for id, expires := range cache.Unresolved {
		if (record.Members[id] != "" || record.Invitations[id] != "") && expires > time.Now().Unix() {
			unresolved[id] = expires
			delete(wanted, id)
		}
	}
	// The signed session was issued only after binding this username to the
	// identity, and namespace claims cannot be reassigned to another account.
	if current.Username != "" {
		if record.Members[userID(current.Identity)] != "" && usernames[userID(current.Identity)] == "" {
			usernames[userID(current.Identity)] = current.Username
			delete(unresolved, userID(current.Identity))
			delete(wanted, userID(current.Identity))
		}
	}
	resolved, complete, err := s.resolveUsernames(ctx, wanted)
	if err != nil {
		return nil, err
	}
	maps.Copy(usernames, resolved)
	if complete {
		for id := range wanted {
			unresolved[id] = time.Now().Add(unresolvedUsernameLifetime).Unix()
		}
	}
	for id := range unresolved {
		if record.Members[id] != "" || view.Invitations[id] != "" {
			view.UnresolvedUserIDs = append(view.UnresolvedUserIDs, id)
		}
	}
	for id := range wanted {
		if _, cached := unresolved[id]; !cached {
			view.UnresolvedUserIDs = append(view.UnresolvedUserIDs, id)
		}
	}
	sort.Strings(view.UnresolvedUserIDs)
	for id, username := range usernames {
		if record.Members[id] != "" {
			view.MemberUsernames[id] = username
		} else if view.Invitations[id] != "" {
			view.InvitationUsernames[id] = username
		}
	}
	updated := groupUsernameCache{SchemaVersion: 1, Usernames: usernames, Unresolved: unresolved}
	if !equalUsernameCaches(cache, updated) {
		// A request that loaded an older group must not prune an alias saved
		// after a concurrent invitation. The sidecar CAS closes the remaining
		// window between this roster check and its write.
		latest, _, err := s.loadNamespace(ctx, name)
		if err == nil && latest.Type == record.Type && latest.CreatorUserID == record.CreatorUserID &&
			maps.Equal(latest.Members, record.Members) && maps.Equal(latest.Invitations, record.Invitations) {
			// Cache persistence is an optimization; verified names in this
			// response remain usable if a conditional write is unavailable.
			_ = s.saveGroupUsernameCache(ctx, name, updated, cacheVersion)
		}
	}
	return view, nil
}

// fallbackGroupView is used only after a group mutation has committed. It
// preserves the successful status and explicitly marks names that still need
// verification, so a retry cannot accidentally repeat the mutation.
func fallbackGroupView(name string, record namespaceRecord, current session) *groupView {
	view := &groupView{
		Name: name, Type: groupNamespace, CreatorUserID: record.CreatorUserID,
		Role: record.Members[userID(current.Identity)], Members: record.Members,
		MemberUsernames: make(map[string]string), CSRF: current.CSRF,
		UsernameLookupError: "Member names could not be loaded. Refresh to retry.",
	}
	if view.Role == "owner" {
		view.Invitations = record.Invitations
		view.InvitationUsernames = make(map[string]string)
	}
	for id := range record.Members {
		if id == userID(current.Identity) && current.Username != "" {
			view.MemberUsernames[id] = current.Username
		} else {
			view.UnresolvedUserIDs = append(view.UnresolvedUserIDs, id)
		}
	}
	for id := range view.Invitations {
		view.UnresolvedUserIDs = append(view.UnresolvedUserIDs, id)
	}
	sort.Strings(view.UnresolvedUserIDs)
	return view
}

func markVerifiedInvitation(view *groupView, id, username string) {
	view.InvitationUsernames[id] = username
	view.UnresolvedUserIDs = slices.DeleteFunc(view.UnresolvedUserIDs, func(current string) bool { return current == id })
}

func equalUsernameCaches(a, b groupUsernameCache) bool {
	if len(a.Usernames) != len(b.Usernames) || len(a.Unresolved) != len(b.Unresolved) {
		return false
	}
	for id, name := range a.Usernames {
		if b.Usernames[id] != name {
			return false
		}
	}
	for id, expires := range a.Unresolved {
		if b.Unresolved[id] != expires {
			return false
		}
	}
	return true
}

func (s *Service) loadGroupUsernameCache(ctx context.Context, name string) (groupUsernameCache, storage.Version, error) {
	body, info, err := s.store.Get(ctx, groupUsernameCacheKey(name))
	if errors.Is(err, storage.ErrNotFound) {
		return groupUsernameCache{SchemaVersion: 1, Usernames: make(map[string]string)}, "", nil
	}
	if err != nil {
		return groupUsernameCache{}, "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(body, maxGroupUsernameCacheBytes+1))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return groupUsernameCache{}, "", err
	}
	if len(data) > maxGroupUsernameCacheBytes || info.Version == "" {
		return groupUsernameCache{}, "", errors.New("invalid group username cache")
	}
	var cache groupUsernameCache
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cache); err != nil {
		return groupUsernameCache{}, "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || cache.SchemaVersion != 1 ||
		len(cache.Usernames) > 2*maxGroupEntries || len(cache.Unresolved) > 2*maxGroupEntries {
		return groupUsernameCache{}, "", errors.New("invalid group username cache")
	}
	for id, username := range cache.Usernames {
		if !validUserID(id) || username == "" || username == "auth" {
			return groupUsernameCache{}, "", errors.New("invalid group username cache")
		}
		if _, err := s.router.Owner(username); err != nil {
			return groupUsernameCache{}, "", errors.New("invalid group username cache")
		}
	}
	for id, expires := range cache.Unresolved {
		if !validUserID(id) || expires <= 0 || cache.Usernames[id] != "" {
			return groupUsernameCache{}, "", errors.New("invalid group username cache")
		}
	}
	return cache, info.Version, nil
}

// The cache is separate from the 128 KiB group namespace. It contains only
// verified names and cannot grant or revoke membership. CAS keeps concurrent
// readers from corrupting it; the next read can fill a name lost to a race.
func (s *Service) saveGroupUsernameCache(ctx context.Context, name string, cache groupUsernameCache, version storage.Version) error {
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	if len(data) > maxGroupUsernameCacheBytes {
		return errors.New("group username cache too large")
	}
	_, err = s.store.Put(ctx, groupUsernameCacheKey(name), bytes.NewReader(data), int64(len(data)),
		storage.PutOptions{IfMatch: version, IfNoneMatch: version == ""})
	return err
}

func (s *Service) prepareGroupInvitation(ctx context.Context, group, caller, id, username string) error {
	record, _, err := s.loadNamespace(ctx, group)
	if err != nil {
		return err
	}
	if record.Type != groupNamespace {
		return errNotGroup
	}
	if record.Members[caller] != "owner" {
		return errGroupDenied
	}
	if record.Members[id] != "" {
		return errGroupConflict
	}
	if err := s.verifyUsernameForID(ctx, id, username); err != nil {
		return err
	}
	return nil
}

func (s *Service) cacheVerifiedGroupUsername(ctx context.Context, group, id, username string) error {
	for range 8 {
		cache, version, err := s.loadGroupUsernameCache(ctx, group)
		if err != nil {
			return err
		}
		cache.Usernames[id] = username
		delete(cache.Unresolved, id)
		data, err := json.Marshal(cache)
		if err != nil {
			return err
		}
		if len(data) > maxGroupUsernameCacheBytes {
			return errors.New("group username cache too large")
		}
		_, err = s.store.Put(ctx, groupUsernameCacheKey(group), bytes.NewReader(data), int64(len(data)),
			storage.PutOptions{IfMatch: version, IfNoneMatch: version == ""})
		if !isNamespaceConflict(err) {
			return err
		}
	}
	return fmt.Errorf("save verified group username: %w", storage.ErrConditionalConflict)
}

// verifyUsernameForID checks the exact alias selected by the group owner.
// A user ID can own more than one username, so a reverse scan alone cannot
// preserve which one was entered in the invitation form.
func (s *Service) verifyUsernameForID(ctx context.Context, id, username string) error {
	if !validUserID(id) || username == "" || username == "auth" {
		return errInvalidMember
	}
	owner, err := s.router.Owner(username)
	if err != nil {
		return errInvalidMember
	}
	if owner == s.local {
		record, _, err := s.loadNamespace(ctx, username)
		if errors.Is(err, storage.ErrNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		if record.Type != userNamespace || userID(record.Identity) != id {
			return errUserNotFound
		}
		return nil
	}
	names, err := s.lookupShardUsernames(ctx, owner, map[string]struct{}{id: {}}, map[string]string{id: username})
	if err != nil {
		return err
	}
	if names[id] != username {
		return errUserNotFound
	}
	return nil
}

// resolveUsernames asks each shard for names bound to the requested stable IDs.
// No username is inferred from the ID, email, or OIDC preferred_username.
func (s *Service) resolveUsernames(ctx context.Context, wanted map[string]struct{}) (map[string]string, bool, error) {
	resolved := make(map[string]string)
	if len(wanted) == 0 {
		return resolved, true, nil
	}
	complete := true
	for shardID := uint32(0); shardID < s.ShardCount() && len(wanted) != 0; shardID++ {
		var names map[string]string
		var err error
		if shard.ShardID(shardID) == s.local {
			names, err = s.scanLocalUsernames(ctx, wanted)
		} else {
			if s.tokenResolver == nil {
				complete = false
				continue // A shard-local service can still return verified local names.
			}
			names, err = s.lookupShardUsernames(ctx, shard.ShardID(shardID), wanted, nil)
		}
		if err != nil {
			return resolved, false, err
		}
		for id, name := range names {
			resolved[id] = name
			delete(wanted, id)
		}
	}
	return resolved, complete, nil
}

func (s *Service) scanLocalUsernames(ctx context.Context, wanted map[string]struct{}) (map[string]string, error) {
	result := make(map[string]string)
	if len(wanted) == 0 {
		return result, nil
	}
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		page, err := s.store.ListPage(ctx, "auth/users/", after, spacePageSize)
		if err != nil {
			return result, err
		}
		for _, object := range page.Objects {
			name, err := s.spaceName(object.Key, "auth/users/")
			if err != nil {
				return result, err
			}
			record, _, err := s.loadNamespace(ctx, name)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return result, err
			}
			if record.Type != userNamespace {
				continue
			}
			id := userID(record.Identity)
			if _, ok := wanted[id]; ok {
				if _, found := result[id]; !found {
					result[id] = name // The first lexical alias on this shard wins.
				}
				if len(result) == len(wanted) {
					return result, nil
				}
			}
		}
		if page.NextAfter == "" {
			return result, nil
		}
		if page.NextAfter <= after {
			return result, errors.New("username lookup cursor did not advance")
		}
		after = page.NextAfter
	}
}

func (s *Service) lookupShardUsernames(ctx context.Context, owner shard.ShardID, wanted map[string]struct{}, expected map[string]string) (map[string]string, error) {
	if s.tokenResolver == nil {
		return nil, errors.New("username authority resolver unavailable")
	}
	destination, err := s.tokenResolver.Resolve(owner)
	if err != nil || destination == nil {
		return nil, errors.New("username authority unavailable")
	}
	if (destination.Scheme != "http" && destination.Scheme != "https") || destination.Host == "" || destination.User != nil || destination.Opaque != "" {
		return nil, errors.New("invalid username authority destination")
	}
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	body, err := json.Marshal(usernameLookupRequest{UserIDs: ids, Expected: expected})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	proof, err := s.sessionCodec.Encode(usernameLookupProofName, usernameLookupProof{
		Digest: base64.RawURLEncoding.EncodeToString(digest[:]), Expires: time.Now().Add(2 * time.Minute).Unix(),
	})
	if err != nil {
		return nil, err
	}
	u := *destination
	u.Path, u.RawPath, u.RawQuery, u.Fragment = usernameLookupPath, "", "", ""
	// The resolver supplies the deployment-owned shard URL and never uses a
	// hostname from an API input.
	// #nosec G704 -- The URL comes from the deployment resolver.
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+proof)
	// #nosec G704 -- The URL comes from the deployment resolver and redirects are refused.
	response, err := s.tokenClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("username authority unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("invalid username authority response")
	}
	var output usernameLookupResponse
	if err := json.Unmarshal(data, &output); err != nil {
		return nil, errors.New("invalid username authority response")
	}
	for id, name := range output.Usernames {
		if _, ok := wanted[id]; !ok || name == "" {
			return nil, errors.New("invalid username authority response")
		}
		shardOwner, err := s.router.Owner(name)
		if err != nil || shardOwner != owner {
			return nil, errors.New("invalid username authority response")
		}
	}
	return output.Usernames, nil
}

// serveUsernameLookup is an internal shard RPC. Its short-lived encrypted proof
// is minted only after the group shard has checked membership permissions.
func (s *Service) serveUsernameLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var proof usernameLookupProof
	authorization := r.Header.Get("Authorization")
	proofValue, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || len(r.Header.Values("Authorization")) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.sessionCodec.Decode(usernameLookupProofName, proofValue, &proof); err != nil ||
		proof.Expires < time.Now().Unix() || proof.Expires > time.Now().Add(2*time.Minute).Unix() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid lookup request", http.StatusBadRequest)
		return
	}
	digest := sha256.Sum256(body)
	if proof.Digest != base64.RawURLEncoding.EncodeToString(digest[:]) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var input usernameLookupRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || len(input.UserIDs) == 0 || len(input.UserIDs) > 2*maxGroupEntries {
		http.Error(w, "invalid lookup request", http.StatusBadRequest)
		return
	}
	wanted := make(map[string]struct{}, len(input.UserIDs))
	for _, id := range input.UserIDs {
		if !validUserID(id) {
			http.Error(w, "invalid lookup request", http.StatusBadRequest)
			return
		}
		wanted[id] = struct{}{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	names := make(map[string]string)
	for id, username := range input.Expected {
		if _, ok := wanted[id]; !ok || username == "auth" {
			http.Error(w, "invalid lookup request", http.StatusBadRequest)
			return
		}
		owner, err := s.router.Owner(username)
		if err != nil || owner != s.local {
			http.Error(w, "invalid lookup request", http.StatusBadRequest)
			return
		}
		record, _, err := s.loadNamespace(ctx, username)
		if err == nil && record.Type == userNamespace && userID(record.Identity) == id {
			names[id] = username
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			serverError(w)
			return
		}
		delete(wanted, id) // Exact lookup must not silently select another alias.
	}
	other, err := s.scanLocalUsernames(ctx, wanted)
	if err != nil {
		serverError(w)
		return
	}
	maps.Copy(names, other)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(usernameLookupResponse{Usernames: names})
}
