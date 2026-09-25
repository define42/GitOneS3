package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestAPITokenLifecycleAndCSRF(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	if err := s.bindUser(context.Background(), "alice", Identity{Subject: "alice-id"}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	path := "/api/v1/users/alice/tokens"
	body := `{"name":"Laptop","permission":"read","repositories":["alice/project"]}`
	for _, test := range []struct {
		name   string
		cookie *http.Cookie
		csrf   string
		status int
	}{
		{"anonymous", nil, csrf, 401},
		{"no CSRF", cookie, "", 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := groupRequest(s, "POST", path, body, test.cookie, test.csrf)
			if w.Code != test.status {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
	other, _ := groupSession(t, s, "alice", "different-subject")
	if w := groupRequest(s, "GET", path, "", other, ""); w.Code != 403 {
		t.Fatalf("wrong identity status=%d", w.Code)
	}
	w := groupRequest(s, "POST", path, body, cookie, csrf)
	if w.Code != 201 {
		t.Fatalf("creation status=%d", w.Code)
	}
	var created struct {
		Token    string    `json:"token"`
		Metadata tokenView `json:"metadata"`
	}
	if json.Unmarshal(w.Body.Bytes(), &created) != nil || created.Token == "" || created.Metadata.ID == "" {
		t.Fatal("missing creation result")
	}
	if created.Metadata.ExpiresAt.Sub(created.Metadata.CreatedAt).Hours() != 30*24 {
		t.Fatal("wrong default expiration")
	}
	w = groupRequest(s, "GET", path, "", cookie, csrf)
	if w.Code != 200 || strings.Contains(w.Body.String(), created.Token) || strings.Contains(w.Body.String(), `"digest"`) {
		t.Fatal("metadata list failed or exposed verifier")
	}
	verify := func(raw string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(
			t.Context(),
			"POST",
			path+"/verify",
			nil,
		)
		if raw != "" {
			r.SetBasicAuth("alice", raw)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := verify(created.Token, nil); w.Code != 200 || strings.Contains(w.Body.String(), created.Token) || strings.Contains(w.Body.String(), `"digest"`) {
		t.Fatalf("verification status=%d", w.Code)
	}
	if w := verify("", cookie); w.Code != 401 {
		t.Fatal("cookie substituted for credential on verification RPC")
	}
	for range 2 {
		if w := groupRequest(s, "DELETE", path+"/"+created.Metadata.ID, "", cookie, csrf); w.Code != 204 {
			t.Fatalf("revocation status=%d", w.Code)
		}
	}
	if w := verify(created.Token, nil); w.Code != 401 {
		t.Fatal("revoked token accepted")
	}
	for _, body := range []string{
		`{"name":"Laptop","permission":"read","repositories":["*"]}`,
		`{"name":"Laptop","permission":"read","repositories":["alice/../project"]}`,
		`{"name":"Laptop","permission":"read","repositories":["alice/project","alice/project"]}`,
		`{"name":"Laptop","permission":"admin","repositories":["alice/project"]}`,
		`{"name":"Laptop","permission":"read","repositories":["alice/project"],"expiresInDays":91}`,
	} {
		if w := groupRequest(s, "POST", path, body, cookie, csrf); w.Code != 400 && w.Code != 422 {
			t.Fatalf("invalid input status=%d", w.Code)
		}
	}
}

func TestAPITokenRepositorySelection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, selection string
		status          int
		all             bool
	}{
		{"legacy selected", `"repositories":["alice/project"]`, 201, false},
		{"explicit selected", `"allRepositories":false,"repositories":["alice/project"]`, 201, false},
		{"all without list", `"allRepositories":true`, 201, true},
		{"all with empty list", `"allRepositories":true,"repositories":[]`, 201, true},
		{"mixed selections", `"allRepositories":true,"repositories":["alice/project"]`, 400, false},
		{"all with wildcard", `"allRepositories":true,"repositories":["*"]`, 400, false},
		{"missing selection", `"expiresInDays":30`, 400, false},
		{"false without list", `"allRepositories":false`, 400, false},
		{"empty selected list", `"repositories":[]`, 400, false},
		{"invalid flag type", `"allRepositories":"true"`, 422, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
			if err := s.bindUser(context.Background(), "alice", Identity{Subject: "alice-id"}); err != nil {
				t.Fatal(err)
			}
			cookie, csrf := groupSession(t, s, "alice", "alice-id")
			path := "/api/v1/users/alice/tokens"
			body := `{"name":"Laptop","permission":"read",` + test.selection + `}`
			w := groupRequest(s, "POST", path, body, cookie, csrf)
			if w.Code != test.status {
				t.Fatalf("status=%d want=%d", w.Code, test.status)
			}
			if w.Code != 201 {
				return
			}
			var created struct {
				Token    string    `json:"token"`
				Metadata tokenView `json:"metadata"`
			}
			if json.Unmarshal(w.Body.Bytes(), &created) != nil || created.Token == "" || created.Metadata.AllRepositories != test.all {
				t.Fatal("wrong token selection response")
			}
			if test.all && (created.Metadata.Repositories == nil || len(created.Metadata.Repositories) != 0) {
				t.Fatal("all scope should return an empty list")
			}
			request := httptest.NewRequestWithContext(
				t.Context(),
				"POST",
				path+"/verify",
				nil,
			)
			request.SetBasicAuth("alice", created.Token)
			verified := httptest.NewRecorder()
			s.ServeHTTP(verified, request)
			var principal tokenPrincipal
			if verified.Code != 200 || json.Unmarshal(verified.Body.Bytes(), &principal) != nil || principal.AllRepositories != test.all {
				t.Fatal("verification did not preserve selection")
			}
			listed := groupRequest(s, "GET", path, "", cookie, csrf)
			var result struct {
				Tokens []tokenView `json:"tokens"`
			}
			if listed.Code != 200 || json.Unmarshal(listed.Body.Bytes(), &result) != nil || len(result.Tokens) != 1 || result.Tokens[0].AllRepositories != test.all {
				t.Fatal("list did not preserve selection")
			}
		})
	}
}
