package auth

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestAPISSHKeyLifecycleAndCSRF(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	if err := s.bindUser(t.Context(), "alice", Identity{Subject: "alice-id"}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	path := "/api/v1/users/alice/ssh-keys"
	body, err := json.Marshal(map[string]string{"name": "Laptop", "publicKey": sshAuthorizedKey(testSSHPublicKey(t, 1))})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		cookie *http.Cookie
		csrf   string
		status int
	}{
		{name: "anonymous", csrf: csrf, status: 401},
		{name: "missing CSRF", cookie: cookie, status: 403},
		{name: "incorrect CSRF", cookie: cookie, csrf: "invalid", status: 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := groupRequest(s, "POST", path, string(body), test.cookie, test.csrf)
			if w.Code != test.status {
				t.Fatalf("status=%d expected=%d", w.Code, test.status)
			}
		})
	}
	otherIdentity, _ := groupSession(t, s, "alice", "different-subject")
	otherUser, _ := groupSession(t, s, "bob", "alice-id")
	for _, unauthorized := range []*http.Cookie{otherIdentity, otherUser} {
		if w := groupRequest(s, "GET", path, "", unauthorized, ""); w.Code != 403 {
			t.Fatalf("wrong owner status=%d", w.Code)
		}
	}
	if w := groupRequest(s, "GET", path, "", cookie, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"keys":[]`) {
		t.Fatalf("empty list: %d %s", w.Code, w.Body.String())
	}
	w := groupRequest(s, "POST", path, string(body), cookie, csrf)
	var created struct {
		Key sshKeyView `json:"key"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil || !validSSHKeyID(created.Key.ID) {
		t.Fatalf("creation: %d %s", w.Code, w.Body.String())
	}
	if w := groupRequest(s, "POST", path, string(body), cookie, csrf); w.Code != 409 {
		t.Fatalf("duplicate status=%d", w.Code)
	}
	if w := groupRequest(s, "GET", path, "", cookie, ""); w.Code != 200 || !strings.Contains(w.Body.String(), created.Key.Fingerprint) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if w := groupRequest(s, "DELETE", path+"/"+created.Key.ID, "", cookie, ""); w.Code != 403 {
		t.Fatalf("revocation without CSRF: %d", w.Code)
	}
	for range 2 {
		if w := groupRequest(s, "DELETE", path+"/"+created.Key.ID, "", cookie, csrf); w.Code != 204 {
			t.Fatalf("revocation: %d %s", w.Code, w.Body.String())
		}
	}
	if w := groupRequest(s, "POST", path, string(body), cookie, csrf); w.Code != 409 {
		t.Fatalf("revoked duplicate status=%d", w.Code)
	}
	if w := groupRequest(s, "DELETE", path+"/"+strings.Repeat("0", 64), "", cookie, csrf); w.Code != 404 {
		t.Fatalf("missing key status=%d", w.Code)
	}
}

func TestAPISSHKeyInputValidation(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	if err := s.bindUser(t.Context(), "alice", Identity{Subject: "alice-id"}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	public := sshAuthorizedKey(testSSHPublicKey(t, 1))
	for _, test := range []struct {
		name, keyName, key string
		status             int
	}{
		{name: "empty name", key: public, status: 422},
		{name: "blank name", keyName: " ", key: public, status: 400},
		{name: "control name", keyName: "bad\x00name", key: public, status: 400},
		{name: "long name", keyName: strings.Repeat("n", 101), key: public, status: 422},
		{name: "invalid key", keyName: "Laptop", key: "invalid", status: 400},
		{name: "multiple keys", keyName: "Laptop", key: public + public, status: 400},
		{name: "oversized key", keyName: "Laptop", key: strings.Repeat("x", maxSSHPublicKeyBytes+1), status: 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(map[string]string{"name": test.keyName, "publicKey": test.key})
			if err != nil {
				t.Fatal(err)
			}
			w := groupRequest(s, "POST", "/api/v1/users/alice/ssh-keys", string(body), cookie, csrf)
			if w.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", w.Code, test.status, w.Body.String())
			}
		})
	}
}

func TestAPISessionSSHURLAndReturnTo(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	s.sshPublicURL = "ssh://git.example:2222"
	w := groupRequest(s, "GET", "/api/v1/session", "", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sshURL":"ssh://git.example:2222"`) {
		t.Fatalf("SSH URL missing: %d %s", w.Code, w.Body.String())
	}
	if !s.validReturnTo("/auth/ssh-keys") {
		t.Fatal("SSH settings cannot be used as a login return URL")
	}
}
