package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"

	"github.com/define42/GitOneS3/internal/authz"
	"github.com/define42/GitOneS3/internal/storage"
)

const (
	userNamespace     = "user"
	groupNamespace    = "group"
	maxNamespaceBytes = 128 << 10
	maxGroupEntries   = 1000
)

var (
	errNamespaceTaken = errors.New("namespace is already claimed")
	errGroupDenied    = errors.New("group permission denied")
	errNotGroup       = errors.New("namespace is not a group")
	errLastOwner      = errors.New("group must retain at least one owner")
	errGroupConflict  = errors.New("group changed concurrently; retry the request")
	errMemberNotFound = errors.New("member or invitation not found")
	errInvalidMember  = errors.New("invalid user ID or role")
	errGroupFull      = errors.New("group member or invitation limit reached")
)

// namespaceRecord is the single atomic claim for either a user or a group.
// Keep the existing auth/users key and top-level identity fields: old user
// records remain valid, and old binaries cannot claim a newly created group.
type namespaceRecord struct {
	SchemaVersion int    `json:"schemaVersion,omitempty"`
	Type          string `json:"type,omitempty"`
	Identity
	CreatorUserID string            `json:"creatorUserId,omitempty"`
	Members       map[string]string `json:"members,omitempty"`
	Invitations   map[string]string `json:"invitations,omitempty"`
}

func namespaceKey(name string) string { return "auth/users/" + name + ".json" }
func userID(identity Identity) string { return "google:" + identity.Subject }

func memberRole(role string) authz.Role {
	switch role {
	case "reader":
		return authz.RoleReader
	case "developer":
		return authz.RoleDeveloper
	case "owner":
		return authz.RoleOwner
	default:
		return authz.RoleNone
	}
}

func validUserID(id string) bool {
	subject, ok := strings.CutPrefix(id, "google:")
	return ok && subject != "" && len(subject) <= 255 && !strings.ContainsFunc(subject, unicode.IsSpace) &&
		!strings.ContainsFunc(subject, unicode.IsControl)
}

func (s *Service) validateNamespaceOwner(name string) error {
	owner, err := s.router.Owner(name)
	if err != nil {
		return err
	}
	if name == "auth" || owner != s.local {
		return errors.New("invalid namespace owner")
	}
	return nil
}

func (s *Service) loadNamespace(ctx context.Context, name string) (namespaceRecord, storage.Version, error) {
	var record namespaceRecord
	if err := s.validateNamespaceOwner(name); err != nil {
		return record, "", err
	}
	body, info, err := s.store.Get(ctx, namespaceKey(name))
	if err != nil {
		return record, "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(body, maxNamespaceBytes+1))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return record, "", err
	}
	if len(data) > maxNamespaceBytes || info.Version == "" {
		return record, "", errors.New("invalid namespace object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return record, "", errors.New("invalid namespace JSON")
	}
	// Records written before groups were introduced contain only subject/email.
	if record.SchemaVersion == 0 && record.Type == "" && record.Subject != "" {
		record.SchemaVersion = 1
		record.Type = userNamespace
	}
	if err := validateNamespace(record); err != nil {
		return record, "", err
	}
	return record, info.Version, nil
}

func validateNamespace(record namespaceRecord) error {
	if record.SchemaVersion != 1 {
		return errors.New("unsupported namespace schema")
	}
	switch record.Type {
	case userNamespace:
		if !validUserID(userID(record.Identity)) || record.CreatorUserID != "" || len(record.Members) != 0 || len(record.Invitations) != 0 {
			return errors.New("invalid user namespace")
		}
	case groupNamespace:
		if record.Subject != "" || record.Email != "" || !validUserID(record.CreatorUserID) || len(record.Members) == 0 ||
			len(record.Members) > maxGroupEntries || len(record.Invitations) > maxGroupEntries {
			return errors.New("invalid group namespace")
		}
		owners := 0
		for id, role := range record.Members {
			if !validUserID(id) || memberRole(role) == authz.RoleNone {
				return errors.New("invalid group member")
			}
			if role == "owner" {
				owners++
			}
		}
		if owners == 0 {
			return errLastOwner
		}
		for id, role := range record.Invitations {
			if !validUserID(id) || memberRole(role) == authz.RoleNone || record.Members[id] != "" {
				return errors.New("invalid group invitation")
			}
		}
	default:
		return errors.New("invalid namespace type")
	}
	return nil
}

func (s *Service) writeNamespace(ctx context.Context, name string, record namespaceRecord, version storage.Version) error {
	if err := s.validateNamespaceOwner(name); err != nil {
		return err
	}
	if err := validateNamespace(record); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > maxNamespaceBytes {
		return errGroupFull
	}
	_, err = s.store.Put(ctx, namespaceKey(name), bytes.NewReader(data), int64(len(data)),
		storage.PutOptions{IfMatch: version, IfNoneMatch: version == ""})
	return err
}

func isNamespaceConflict(err error) bool {
	return errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrPreconditionFailed) ||
		errors.Is(err, storage.ErrConditionalConflict)
}

func (s *Service) createGroup(ctx context.Context, name, creator string) (namespaceRecord, error) {
	record := namespaceRecord{SchemaVersion: 1, Type: groupNamespace, CreatorUserID: creator,
		Members: map[string]string{creator: "owner"}}
	err := s.writeNamespace(ctx, name, record, "")
	if isNamespaceConflict(err) {
		return namespaceRecord{}, errNamespaceTaken
	}
	return record, err
}

// updateGroup reloads both membership and ownership on every CAS retry.
// A stale owner cannot overwrite a concurrent revocation or lose another update.
func (s *Service) updateGroup(ctx context.Context, name, caller, action, target, role string) (namespaceRecord, error) {
	for range 8 {
		record, version, err := s.loadNamespace(ctx, name)
		if err != nil {
			return namespaceRecord{}, err
		}
		if record.Type != groupNamespace {
			return namespaceRecord{}, errNotGroup
		}
		if action == "accept" {
			invitedRole := record.Invitations[caller]
			if invitedRole == "" {
				return namespaceRecord{}, errMemberNotFound
			}
			if len(record.Members) >= maxGroupEntries {
				return namespaceRecord{}, errGroupFull
			}
			record.Members[caller] = invitedRole
			delete(record.Invitations, caller)
		} else {
			if record.Members[caller] != "owner" {
				return namespaceRecord{}, errGroupDenied
			}
			if !validUserID(target) {
				return namespaceRecord{}, errInvalidMember
			}
			switch action {
			case "invite":
				if memberRole(role) == authz.RoleNone {
					return namespaceRecord{}, errInvalidMember
				}
				if record.Members[target] != "" {
					return namespaceRecord{}, errGroupConflict
				}
				if len(record.Invitations) >= maxGroupEntries && record.Invitations[target] == "" {
					return namespaceRecord{}, errGroupFull
				}
				if record.Invitations == nil {
					record.Invitations = make(map[string]string)
				}
				record.Invitations[target] = role
			case "cancel":
				if record.Invitations[target] == "" {
					return namespaceRecord{}, errMemberNotFound
				}
				delete(record.Invitations, target)
			case "set-role":
				if memberRole(role) == authz.RoleNone {
					return namespaceRecord{}, errInvalidMember
				}
				if record.Members[target] == "" {
					return namespaceRecord{}, errMemberNotFound
				}
				record.Members[target] = role
			case "remove":
				if record.Members[target] == "" {
					return namespaceRecord{}, errMemberNotFound
				}
				delete(record.Members, target)
			default:
				return namespaceRecord{}, errors.New("invalid group update")
			}
		}
		owners := 0
		for _, role := range record.Members {
			if role == "owner" {
				owners++
			}
		}
		if owners == 0 {
			return namespaceRecord{}, errLastOwner
		}
		err = s.writeNamespace(ctx, name, record, version)
		if isNamespaceConflict(err) {
			continue
		}
		return record, err
	}
	return namespaceRecord{}, errGroupConflict
}
