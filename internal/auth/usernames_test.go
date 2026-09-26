package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/define42/GitOneS3/internal/proxy"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

func nameOnShard(t *testing.T, s *Service, owner shard.ShardID, prefix string) string {
	t.Helper()
	for i := range 1000 {
		name := prefix + "-" + string(rune('a'+i%26))
		shardOwner, err := s.router.Owner(name)
		if err != nil {
			t.Fatal(err)
		}
		if shardOwner == owner {
			return name
		}
	}
	t.Fatal("no name on requested shard")
	return ""
}

func TestGroupUsernameLookupBackfillsLegacyAcrossShardsAndCaches(t *testing.T) {
	t.Parallel()
	groupStore, userStore := storage.NewMemoryStore(), storage.NewMemoryStore()
	groupShard := testService(t, 0, groupStore, &fakeProvider{}, nil)
	userShard := testService(t, 1, userStore, &fakeProvider{}, nil)
	groupName := nameOnShard(t, groupShard, 0, "group")
	ownerName := nameOnShard(t, groupShard, 0, "owner")
	memberName := nameOnShard(t, groupShard, 1, "member")
	inviteeName := nameOnShard(t, groupShard, 1, "invitee")
	ownerIdentity := Identity{Subject: "owner-id", Email: "private-owner@example.invalid"}
	memberIdentity := Identity{Issuer: "https://keycloak.example/realms/test", Subject: "member-subject", Email: "private-member@example.invalid"}
	inviteeIdentity := Identity{Issuer: "https://keycloak.example/realms/test", Subject: "invitee-subject", Email: "private-invitee@example.invalid"}
	for _, entry := range []struct {
		service  *Service
		username string
		identity Identity
	}{
		{groupShard, ownerName, ownerIdentity},
		{userShard, memberName, memberIdentity},
		{userShard, inviteeName, inviteeIdentity},
	} {
		if err := entry.service.bindUser(t.Context(), entry.username, entry.identity); err != nil {
			t.Fatal(err)
		}
	}
	ownerID, memberID, inviteeID := userID(ownerIdentity), userID(memberIdentity), userID(inviteeIdentity)
	legacy := namespaceRecord{SchemaVersion: 1, Type: groupNamespace, CreatorUserID: ownerID,
		Members:     map[string]string{ownerID: "owner", memberID: "developer"},
		Invitations: map[string]string{inviteeID: "reader"}}
	if err := groupShard.writeNamespace(t.Context(), groupName, legacy, ""); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	userProxy, err := proxy.NewHandler(proxy.HandlerOptions{
		LocalShard: 1, Router: userShard, Next: userShard,
		Resolver: resolverFunc(func(shard.ShardID) (*url.URL, error) {
			return url.Parse("http://unused.internal")
		}),
		Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("local username lookup was forwarded")
			return nil, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	configureLookup := func(s *Service) {
		s.tokenResolver = resolverFunc(func(id shard.ShardID) (*url.URL, error) {
			if id != 1 {
				t.Fatalf("unexpected lookup shard: %d", id)
			}
			return url.Parse("http://user-shard.internal")
		})
		s.tokenClient.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			w := httptest.NewRecorder()
			userProxy.ServeHTTP(w, r)
			return w.Result(), nil
		})
	}
	configureLookup(groupShard)
	ownerCookie, ownerCSRF := groupSession(t, groupShard, ownerName, ownerIdentity.Subject)
	readGroup := func(handler http.Handler) groupView {
		t.Helper()
		w := groupRequest(handler, http.MethodGet, "/api/v1/groups/"+groupName, "", ownerCookie, ownerCSRF)
		if w.Code != http.StatusOK {
			t.Fatalf("group read=%d: %s", w.Code, w.Body.String())
		}
		if body := w.Body.String(); containsAny(body, ownerIdentity.Email, memberIdentity.Email, inviteeIdentity.Email) {
			t.Fatalf("group disclosed email: %s", body)
		}
		var view groupView
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	view := readGroup(groupShard.newAPIHandler())
	if view.MemberUsernames[ownerID] != ownerName || view.MemberUsernames[memberID] != memberName ||
		view.InvitationUsernames[inviteeID] != inviteeName || len(view.UnresolvedUserIDs) != 0 || calls.Load() != 1 {
		t.Fatalf("legacy names not resolved: %+v, calls=%d", view, calls.Load())
	}
	cache, _, err := groupShard.loadGroupUsernameCache(t.Context(), groupName)
	if err != nil || len(cache.Usernames) != 3 {
		t.Fatalf("username cache=%+v, err=%v", cache, err)
	}
	restarted := testService(t, 0, groupStore, &fakeProvider{}, nil)
	configureLookup(restarted)
	view = readGroup(restarted.newAPIHandler())
	if view.MemberUsernames[memberID] != memberName || view.InvitationUsernames[inviteeID] != inviteeName || calls.Load() != 1 {
		t.Fatalf("cached names lost after restart: %+v, calls=%d", view, calls.Load())
	}
	w := groupRequest(restarted.newAPIHandler(), http.MethodDelete, "/api/v1/groups/"+groupName+"/invitations",
		`{"userId":"`+inviteeID+`"}`, ownerCookie, ownerCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel invitation=%d: %s", w.Code, w.Body.String())
	}
	cache, _, err = restarted.loadGroupUsernameCache(t.Context(), groupName)
	if err != nil || len(cache.Usernames) != 2 || cache.Usernames[inviteeID] != "" {
		t.Fatalf("stale invitation name in cache: %+v, err=%v", cache, err)
	}
}

func TestGroupUsernameLookupRejectsUnsignedRequestAndReportsAuthorityFailure(t *testing.T) {
	t.Parallel()
	groupShard := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	groupName := nameOnShard(t, groupShard, 0, "group")
	ownerName := nameOnShard(t, groupShard, 0, "owner")
	ownerID := "google:owner-id"
	memberID := "google:member-id"
	if err := groupShard.writeNamespace(t.Context(), groupName, namespaceRecord{
		SchemaVersion: 1, Type: groupNamespace, CreatorUserID: ownerID,
		Members: map[string]string{ownerID: "owner", memberID: "reader"},
	}, ""); err != nil {
		t.Fatal(err)
	}
	unsigned := groupRequest(groupShard.newAPIHandler(), http.MethodPost, usernameLookupPath,
		`{"userIds":["`+memberID+`"]}`, nil, "")
	if unsigned.Code != http.StatusForbidden {
		t.Fatalf("unsigned lookup=%d: %s", unsigned.Code, unsigned.Body.String())
	}
	ownerCookie, ownerCSRF := groupSession(t, groupShard, ownerName, "owner-id")
	localRead := groupRequest(groupShard.newAPIHandler(), http.MethodGet, "/api/v1/groups/"+groupName, "", ownerCookie, ownerCSRF)
	if localRead.Code != http.StatusOK {
		t.Fatalf("local lookup=%d: %s", localRead.Code, localRead.Body.String())
	}
	var localView groupView
	if err := json.Unmarshal(localRead.Body.Bytes(), &localView); err != nil {
		t.Fatal(err)
	}
	if len(localView.UnresolvedUserIDs) != 1 || localView.UnresolvedUserIDs[0] != memberID || localView.MemberUsernames[memberID] != "" {
		t.Fatalf("missing account was not reported: %+v", localView)
	}
	groupShard.tokenResolver = resolverFunc(func(shard.ShardID) (*url.URL, error) {
		return url.Parse("http://unavailable.internal")
	})
	groupShard.tokenClient.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("authority unavailable")
	})
	w := groupRequest(groupShard.newAPIHandler(), http.MethodGet, "/api/v1/groups/"+groupName, "", ownerCookie, ownerCSRF)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("authority failure=%d: %s", w.Code, w.Body.String())
	}
	mutation := groupRequest(groupShard.newAPIHandler(), http.MethodPut, "/api/v1/groups/"+groupName+"/members",
		`{"userId":"`+memberID+`","role":"developer"}`, ownerCookie, ownerCSRF)
	if mutation.Code != http.StatusOK || !strings.Contains(mutation.Body.String(), `"usernameLookupError"`) ||
		!strings.Contains(mutation.Body.String(), `"`+memberID+`":"developer"`) {
		t.Fatalf("committed mutation reported failure: %d %s", mutation.Code, mutation.Body.String())
	}
	cache, _, err := groupShard.loadGroupUsernameCache(context.Background(), groupName)
	if err != nil || len(cache.Usernames) != 1 || cache.Usernames[memberID] != "" {
		t.Fatalf("failed lookup published unverified name: %+v, err=%v", cache, err)
	}
}

func TestInvitationPreservesVerifiedAlias(t *testing.T) {
	t.Parallel()
	groupShard := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	userShard := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	groupName := nameOnShard(t, groupShard, 0, "group")
	ownerName := nameOnShard(t, groupShard, 0, "owner")
	readerName := nameOnShard(t, groupShard, 0, "reader")
	otherAlias := nameOnShard(t, groupShard, 0, "alias")
	selectedAlias := nameOnShard(t, groupShard, 1, "selected")
	ownerIdentity := Identity{Subject: "owner-id"}
	readerIdentity := Identity{Subject: "reader-id"}
	memberIdentity := Identity{Subject: "member-id"}
	for _, binding := range []struct {
		service *Service
		name    string
		id      Identity
	}{
		{groupShard, ownerName, ownerIdentity},
		{groupShard, readerName, readerIdentity},
		{groupShard, otherAlias, memberIdentity},
		{userShard, selectedAlias, memberIdentity},
	} {
		if err := binding.service.bindUser(t.Context(), binding.name, binding.id); err != nil {
			t.Fatal(err)
		}
	}
	userProxy, err := proxy.NewHandler(proxy.HandlerOptions{LocalShard: 1, Router: userShard, Next: userShard,
		Resolver:  resolverFunc(func(shard.ShardID) (*url.URL, error) { return url.Parse("http://unused.internal") }),
		Transport: transportFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected forward"); return nil, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	groupShard.tokenResolver = resolverFunc(func(shard.ShardID) (*url.URL, error) { return url.Parse("http://user-shard.internal") })
	groupShard.tokenClient.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		userProxy.ServeHTTP(w, r)
		return w.Result(), nil
	})
	ownerCookie, ownerCSRF := groupSession(t, groupShard, ownerName, ownerIdentity.Subject)
	readerCookie, readerCSRF := groupSession(t, groupShard, readerName, readerIdentity.Subject)
	memberCookie, memberCSRF := groupSession(t, groupShard, otherAlias, memberIdentity.Subject)
	api := groupShard.newAPIHandler()
	legacyGroup := nameOnShard(t, groupShard, 0, "legacy")
	if err := groupShard.writeNamespace(t.Context(), legacyGroup, namespaceRecord{SchemaVersion: 1, Type: groupNamespace,
		CreatorUserID: userID(ownerIdentity), Members: map[string]string{userID(ownerIdentity): "owner", userID(memberIdentity): "reader"}}, ""); err != nil {
		t.Fatal(err)
	}
	legacyRead := groupRequest(api, http.MethodGet, "/api/v1/groups/"+legacyGroup, "", ownerCookie, ownerCSRF)
	if legacyRead.Code != http.StatusOK || !strings.Contains(legacyRead.Body.String(), `"`+userID(memberIdentity)+`":"`+otherAlias+`"`) {
		t.Fatalf("legacy alias was not selected deterministically: %d %s", legacyRead.Code, legacyRead.Body.String())
	}
	raceGroup := nameOnShard(t, groupShard, 0, "race")
	if err := groupShard.writeNamespace(t.Context(), raceGroup, namespaceRecord{SchemaVersion: 1, Type: groupNamespace,
		CreatorUserID: userID(ownerIdentity), Members: map[string]string{userID(ownerIdentity): "owner"}}, ""); err != nil {
		t.Fatal(err)
	}
	if err := groupShard.prepareGroupInvitation(t.Context(), raceGroup, userID(ownerIdentity), userID(memberIdentity), selectedAlias); err != nil {
		t.Fatal(err)
	}
	precommitCache, _, err := groupShard.loadGroupUsernameCache(t.Context(), raceGroup)
	if err != nil || precommitCache.Usernames[userID(memberIdentity)] != "" {
		t.Fatalf("verified alias was cached before group commit: %+v, err=%v", precommitCache, err)
	}
	staleRecord, _, err := groupShard.loadNamespace(t.Context(), raceGroup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := groupShard.updateGroup(t.Context(), raceGroup, userID(ownerIdentity), "invite", userID(memberIdentity), "reader"); err != nil {
		t.Fatal(err)
	}
	interleavedRead := groupRequest(api, http.MethodGet, "/api/v1/groups/"+raceGroup, "", ownerCookie, ownerCSRF)
	if interleavedRead.Code != http.StatusOK || !strings.Contains(interleavedRead.Body.String(),
		`"`+userID(memberIdentity)+`":"`+otherAlias+`"`) {
		t.Fatalf("interleaved fallback lookup did not run: %d %s", interleavedRead.Code, interleavedRead.Body.String())
	}
	if err := groupShard.cacheVerifiedGroupUsername(t.Context(), raceGroup, userID(memberIdentity), selectedAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := groupShard.groupView(t.Context(), raceGroup, staleRecord,
		session{Username: ownerName, Identity: ownerIdentity, CSRF: ownerCSRF}); err != nil {
		t.Fatal(err)
	}
	repairedRead := groupRequest(api, http.MethodGet, "/api/v1/groups/"+raceGroup, "", ownerCookie, ownerCSRF)
	if repairedRead.Code != http.StatusOK || !strings.Contains(repairedRead.Body.String(),
		`"`+userID(memberIdentity)+`":"`+selectedAlias+`"`) {
		t.Fatalf("selected alias did not replace interleaved fallback: %d %s", repairedRead.Code, repairedRead.Body.String())
	}
	created := groupRequest(api, http.MethodPost, "/api/v1/groups/"+groupName, "", ownerCookie, ownerCSRF)
	if created.Code != http.StatusCreated {
		t.Fatalf("create group=%d: %s", created.Code, created.Body.String())
	}
	groupRecord, version, err := groupShard.loadNamespace(t.Context(), groupName)
	if err != nil {
		t.Fatal(err)
	}
	groupRecord.Members[userID(readerIdentity)] = "reader"
	if err := groupShard.writeNamespace(t.Context(), groupName, groupRecord, version); err != nil {
		t.Fatal(err)
	}
	memberID := userID(memberIdentity)
	wrongAlias := groupRequest(api, http.MethodPost, "/api/v1/groups/"+groupName+"/invitations",
		`{"userId":"google:other-id","username":"`+selectedAlias+`","role":"reader"}`, ownerCookie, ownerCSRF)
	if wrongAlias.Code != http.StatusNotFound {
		t.Fatalf("mismatched alias=%d: %s", wrongAlias.Code, wrongAlias.Body.String())
	}
	record, _, err := groupShard.loadNamespace(t.Context(), groupName)
	if err != nil || len(record.Invitations) != 0 {
		t.Fatalf("mismatched alias mutated group: %+v, err=%v", record, err)
	}
	invited := groupRequest(api, http.MethodPost, "/api/v1/groups/"+groupName+"/invitations",
		`{"userId":"`+memberID+`","username":"`+selectedAlias+`","role":"reader"}`, ownerCookie, ownerCSRF)
	if invited.Code != http.StatusOK {
		t.Fatalf("invite=%d: %s", invited.Code, invited.Body.String())
	}
	var view groupView
	if err := json.Unmarshal(invited.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.InvitationUsernames[memberID] != selectedAlias {
		t.Fatalf("invited alias changed: %+v", view)
	}
	const hiddenID = "google:hidden-id"
	groupRecord, version, err = groupShard.loadNamespace(t.Context(), groupName)
	if err != nil {
		t.Fatal(err)
	}
	groupRecord.Invitations[hiddenID] = "reader"
	if err := groupShard.writeNamespace(t.Context(), groupName, groupRecord, version); err != nil {
		t.Fatal(err)
	}
	ownerWithUnresolved := groupRequest(api, http.MethodGet, "/api/v1/groups/"+groupName, "", ownerCookie, ownerCSRF)
	if ownerWithUnresolved.Code != http.StatusOK || !strings.Contains(ownerWithUnresolved.Body.String(), hiddenID) {
		t.Fatalf("owner could not see unresolved invitation: %d %s", ownerWithUnresolved.Code, ownerWithUnresolved.Body.String())
	}
	readerRead := groupRequest(api, http.MethodGet, "/api/v1/groups/"+groupName, "", readerCookie, readerCSRF)
	if readerRead.Code != http.StatusOK || strings.Contains(readerRead.Body.String(), `"invitations"`) ||
		strings.Contains(readerRead.Body.String(), memberID) || strings.Contains(readerRead.Body.String(), selectedAlias) ||
		strings.Contains(readerRead.Body.String(), hiddenID) {
		t.Fatalf("reader response disclosed invitation: %d %s", readerRead.Code, readerRead.Body.String())
	}
	ownerBeforeAcceptance := groupRequest(api, http.MethodGet, "/api/v1/groups/"+groupName, "", ownerCookie, ownerCSRF)
	if ownerBeforeAcceptance.Code != http.StatusOK || !strings.Contains(ownerBeforeAcceptance.Body.String(),
		`"`+memberID+`":"`+selectedAlias+`"`) {
		t.Fatalf("reader view discarded verified invitation alias: %d %s", ownerBeforeAcceptance.Code, ownerBeforeAcceptance.Body.String())
	}
	accepted := groupRequest(api, http.MethodPost, "/api/v1/groups/"+groupName+"/invitations/accept", "", memberCookie, memberCSRF)
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept=%d: %s", accepted.Code, accepted.Body.String())
	}
	ownerRead := groupRequest(api, http.MethodGet, "/api/v1/groups/"+groupName, "", ownerCookie, ownerCSRF)
	if ownerRead.Code != http.StatusOK {
		t.Fatalf("owner read=%d: %s", ownerRead.Code, ownerRead.Body.String())
	}
	if err := json.Unmarshal(ownerRead.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.MemberUsernames[memberID] != selectedAlias {
		t.Fatalf("accepted member alias changed: %+v", view)
	}
}

func TestUnresolvedUsernameLookupIsTemporarilyCached(t *testing.T) {
	t.Parallel()
	groupShard := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	userShard := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	groupName := nameOnShard(t, groupShard, 0, "group")
	ownerName := nameOnShard(t, groupShard, 0, "owner")
	ownerID, missingID := "google:owner-id", "google:missing-id"
	if err := groupShard.bindUser(t.Context(), ownerName, Identity{Subject: "owner-id"}); err != nil {
		t.Fatal(err)
	}
	if err := groupShard.writeNamespace(t.Context(), groupName, namespaceRecord{SchemaVersion: 1, Type: groupNamespace,
		CreatorUserID: ownerID, Members: map[string]string{ownerID: "owner", missingID: "reader"}}, ""); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	groupShard.tokenResolver = resolverFunc(func(shard.ShardID) (*url.URL, error) { return url.Parse("http://user-shard.internal") })
	groupShard.tokenClient.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		w := httptest.NewRecorder()
		userShard.ServeHTTP(w, r)
		return w.Result(), nil
	})
	ownerCookie, ownerCSRF := groupSession(t, groupShard, ownerName, "owner-id")
	api := groupShard.newAPIHandler()
	for range 2 {
		w := groupRequest(api, http.MethodGet, "/api/v1/groups/"+groupName, "", ownerCookie, ownerCSRF)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"unresolvedUserIds":["`+missingID+`"]`) {
			t.Fatalf("missing account response=%d: %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("unresolved ID triggered repeated shard scans: %d", calls.Load())
	}
	missingName := nameOnShard(t, groupShard, 1, "missing")
	if err := userShard.bindUser(t.Context(), missingName, Identity{Subject: "missing-id"}); err != nil {
		t.Fatal(err)
	}
	memberCookie, memberCSRF := groupSession(t, groupShard, missingName, "missing-id")
	memberRead := groupRequest(api, http.MethodGet, "/api/v1/groups/"+groupName, "", memberCookie, memberCSRF)
	if memberRead.Code != http.StatusOK {
		t.Fatalf("new member read=%d: %s", memberRead.Code, memberRead.Body.String())
	}
	ownerRead := groupRequest(api, http.MethodGet, "/api/v1/groups/"+groupName, "", ownerCookie, ownerCSRF)
	if ownerRead.Code != http.StatusOK || !strings.Contains(ownerRead.Body.String(), `"`+missingID+`":"`+missingName+`"`) {
		t.Fatalf("member's verified session did not replace negative cache: %d %s", ownerRead.Code, ownerRead.Body.String())
	}
}

type rejectGroupUsernameCachePutStore struct{ storage.ObjectStore }

func (s rejectGroupUsernameCachePutStore) Put(ctx context.Context, key string, body io.Reader, size int64,
	options storage.PutOptions) (storage.ObjectInfo, error) {
	if strings.HasPrefix(key, "auth/group-usernames/v1/") {
		return storage.ObjectInfo{}, errors.New("cache unavailable")
	}
	return s.ObjectStore.Put(ctx, key, body, size, options)
}

func TestGroupUsernameCacheWriteFailureDoesNotHideVerifiedNames(t *testing.T) {
	t.Parallel()
	store := rejectGroupUsernameCachePutStore{storage.NewMemoryStore()}
	s := testService(t, 0, store, &fakeProvider{}, nil)
	groupName := nameOnShard(t, s, 0, "group")
	ownerName := nameOnShard(t, s, 0, "owner")
	memberName := nameOnShard(t, s, 0, "member")
	inviteeName := nameOnShard(t, s, 0, "invitee")
	for _, entry := range []struct{ name, subject string }{{ownerName, "owner-id"}, {memberName, "member-id"}, {inviteeName, "invitee-id"}} {
		if err := s.bindUser(t.Context(), entry.name, Identity{Subject: entry.subject}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.writeNamespace(t.Context(), groupName, namespaceRecord{SchemaVersion: 1, Type: groupNamespace,
		CreatorUserID: "google:owner-id", Members: map[string]string{"google:owner-id": "owner", "google:member-id": "reader"}}, ""); err != nil {
		t.Fatal(err)
	}
	ownerCookie, ownerCSRF := groupSession(t, s, ownerName, "owner-id")
	api := s.newAPIHandler()
	w := groupRequest(api, http.MethodGet, "/api/v1/groups/"+groupName, "", ownerCookie, ownerCSRF)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"google:member-id":"`+memberName+`"`) {
		t.Fatalf("cache write hid verified name: %d %s", w.Code, w.Body.String())
	}
	w = groupRequest(api, http.MethodPut, "/api/v1/groups/"+groupName+"/members",
		`{"userId":"google:member-id","role":"developer"}`, ownerCookie, ownerCSRF)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"google:member-id":"developer"`) {
		t.Fatalf("cache write hid committed mutation: %d %s", w.Code, w.Body.String())
	}
	w = groupRequest(api, http.MethodPost, "/api/v1/groups/"+groupName+"/invitations",
		`{"userId":"google:invitee-id","username":"`+inviteeName+`","role":"reader"}`, ownerCookie, ownerCSRF)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"usernameLookupError"`) ||
		!strings.Contains(w.Body.String(), `"google:invitee-id":"`+inviteeName+`"`) {
		t.Fatalf("cache failure hid committed invite or selected alias: %d %s", w.Code, w.Body.String())
	}
	var view groupView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	for _, id := range view.UnresolvedUserIDs {
		if id == "google:invitee-id" {
			t.Fatalf("verified invitee was reported unresolved: %+v", view)
		}
	}
	record, _, err := s.loadNamespace(t.Context(), groupName)
	if err != nil || record.Invitations["google:invitee-id"] != "reader" {
		t.Fatalf("invite not committed after cache failure: %+v, err=%v", record, err)
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
