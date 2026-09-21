package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/authz"
	"github.com/define42/GitOneS3/internal/storage"
)

type groupView struct {
	Name          string            `json:"name"`
	Type          string            `json:"type"`
	CreatorUserID string            `json:"creatorUserId"`
	Role          string            `json:"role"`
	Members       map[string]string `json:"members"`
	Invitations   map[string]string `json:"invitations,omitempty"`
	CSRF          string            `json:"csrfToken"`
}

func writeGroup(w http.ResponseWriter, status int, name string, record namespaceRecord, current session) {
	view := groupView{Name: name, Type: groupNamespace, CreatorUserID: record.CreatorUserID,
		Role: record.Members[userID(current.Identity)], Members: record.Members, CSRF: current.CSRF}
	if view.Role == "owner" {
		view.Invitations = record.Invitations
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(view)
}

func (s *Service) serveCreateGroup(w http.ResponseWriter, r *http.Request, name string, current session) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	record, err := s.createGroup(ctx, name, userID(current.Identity))
	if err != nil {
		groupError(w, err)
		return
	}
	w.Header().Set("Location", "/"+name+"/")
	writeGroup(w, http.StatusCreated, name, record, current)
}

func (s *Service) serveGroup(w http.ResponseWriter, r *http.Request, name string, current session, record namespaceRecord) {
	id := userID(current.Identity)
	suffix := strings.TrimPrefix(r.URL.Path, "/"+name)
	if suffix == "/invitations/accept" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		s.serveGroupUpdate(w, r, name, current, "accept", "", "")
		return
	}
	role := memberRole(record.Members[id])
	if role == authz.RoleNone {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch suffix {
	case "", "/":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET, POST")
			return
		}
		writeGroup(w, http.StatusOK, name, record, current)
		return
	case "/invitations", "/members":
		if role != authz.RoleOwner {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodGet {
			writeGroup(w, http.StatusOK, name, record, current)
			return
		}
		action := ""
		if suffix == "/invitations" {
			switch r.Method {
			case http.MethodPost:
				action = "invite"
			case http.MethodDelete:
				action = "cancel"
			default:
				methodNotAllowed(w, "GET, POST, DELETE")
				return
			}
		} else {
			switch r.Method {
			case http.MethodPut:
				action = "set-role"
			case http.MethodDelete:
				action = "remove"
			default:
				methodNotAllowed(w, "GET, PUT, DELETE")
				return
			}
		}
		var input struct {
			UserID string `json:"userId"`
			Role   string `json:"role"`
		}
		if err := decodeGroupJSON(w, r, &input); err != nil {
			http.Error(w, "invalid member JSON", http.StatusBadRequest)
			return
		}
		s.serveGroupUpdate(w, r, name, current, action, input.UserID, input.Role)
		return
	}
	required, err := requiredGroupRole(w, r)
	if err != nil {
		http.Error(w, "invalid repository operation", http.StatusBadRequest)
		return
	}
	if role < required {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	subject := authz.Subject{UserID: id, Authenticated: true}
	s.next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), subjectKey{}, subject)))
}

func (s *Service) serveGroupUpdate(w http.ResponseWriter, r *http.Request, name string, current session, action, target, role string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	record, err := s.updateGroup(ctx, name, userID(current.Identity), action, target, role)
	if err != nil {
		groupError(w, err)
		return
	}
	// Do not disclose the remaining roster after an owner removes themselves.
	if record.Members[userID(current.Identity)] == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeGroup(w, http.StatusOK, name, record, current)
}

func decodeGroupJSON(w http.ResponseWriter, r *http.Request, output any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("JSON content type required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

// Git uses POST for reads and GET for push discovery. Authorize the protocol
// operation, not just the HTTP verb. Unknown mutations require developer.
func requiredGroupRole(w http.ResponseWriter, r *http.Request) (authz.Role, error) {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		if strings.HasSuffix(r.URL.Path, ".git/info/refs") {
			query, err := url.ParseQuery(r.URL.RawQuery)
			if err != nil || len(query["service"]) > 1 {
				return authz.RoleNone, errors.New("invalid Git service")
			}
			if query.Get("service") == "git-receive-pack" {
				return authz.RoleDeveloper, nil
			}
		}
		return authz.RoleReader, nil
	case http.MethodPost:
		if strings.HasSuffix(r.URL.Path, ".git/git-upload-pack") {
			return authz.RoleReader, nil
		}
		if strings.HasSuffix(r.URL.Path, ".git/info/lfs/objects/batch") {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			data, err := io.ReadAll(r.Body)
			err = errors.Join(err, r.Body.Close())
			if err != nil {
				return authz.RoleNone, err
			}
			var batch map[string]json.RawMessage
			if err := json.Unmarshal(data, &batch); err != nil {
				return authz.RoleNone, err
			}
			for key := range batch {
				if strings.EqualFold(key, "operation") && key != "operation" {
					return authz.RoleNone, errors.New("noncanonical LFS operation key")
				}
			}
			var operation string
			if err := json.Unmarshal(batch["operation"], &operation); err != nil {
				return authz.RoleNone, err
			}
			if operation != "download" && operation != "upload" {
				return authz.RoleNone, errors.New("invalid LFS operation")
			}
			// Forward canonical JSON so a downstream parser cannot interpret
			// duplicate operation keys differently from this permission check.
			data, err = json.Marshal(batch)
			if err != nil {
				return authz.RoleNone, err
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			r.ContentLength = int64(len(data))
			r.Header.Set("Content-Length", strconv.Itoa(len(data)))
			if operation == "download" {
				return authz.RoleReader, nil
			}
		}
	}
	return authz.RoleDeveloper, nil
}

func groupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNamespaceTaken), errors.Is(err, errLastOwner), errors.Is(err, errGroupConflict), errors.Is(err, errGroupFull):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errGroupDenied):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, errNotGroup), errors.Is(err, errMemberNotFound), errors.Is(err, storage.ErrNotFound):
		http.Error(w, "group, member, or invitation not found", http.StatusNotFound)
	case errors.Is(err, errInvalidMember):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		serverError(w)
	}
}
