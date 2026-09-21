// Package authz evaluates shard-local repository visibility and inherited ACLs.
package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrDenied reports that the effective policy does not allow an action.
	ErrDenied = errors.New("authorization denied")
	// ErrPolicyNotFound lets a loader omit scopes without explicit policy.
	ErrPolicyNotFound = errors.New("authorization policy not found")
)

// Action is an operation checked against repository policy.
type Action uint8

const (
	ActionUnknown Action = iota
	ActionRead
	ActionWrite
	ActionManage
)

// Role is the maximum capability granted at a policy scope.
type Role uint8

const (
	RoleNone Role = iota
	RoleReader
	RoleDeveloper
	RoleOwner
)

// Visibility controls the baseline read policy of a repository.
type Visibility uint8

const (
	VisibilityUnknown Visibility = iota
	VisibilityPrivate
	VisibilityInternal
	VisibilityPublic
)

// Subject carries the immutable identity resolved by authentication.
type Subject struct {
	UserID        string
	Authenticated bool
}

// Repository identifies a repository without using its mutable display name.
type Repository struct {
	ID         string
	Path       string
	Visibility Visibility
}

// Policy is one compiled control-repository generation for a path scope.
type Policy struct {
	Generation uint64
	Members    map[string]Role
}

// PolicyLoader reads compiled policies for exact group/repository scopes.
type PolicyLoader interface {
	LoadPolicy(ctx context.Context, path string) (Policy, error)
}

// Authorizer is the consumer-facing authorization contract.
type Authorizer interface {
	Check(ctx context.Context, subject Subject, repository Repository, action Action) error
}

// Service evaluates visibility and every ancestor policy on one shard.
type Service struct {
	loader PolicyLoader
}

// New constructs an authorization service.
func New(loader PolicyLoader) (*Service, error) {
	if loader == nil {
		return nil, errors.New("policy loader is required")
	}

	return &Service{loader: loader}, nil
}

// Check evaluates visibility followed by inherited and direct grants.
func (s *Service) Check(
	ctx context.Context,
	subject Subject,
	repository Repository,
	action Action,
) error {
	if err := validateRequest(subject, repository, action); err != nil {
		return err
	}
	if action == ActionRead {
		if repository.Visibility == VisibilityPublic {
			return nil
		}
		if repository.Visibility == VisibilityInternal && subject.Authenticated {
			return nil
		}
	}
	if !subject.Authenticated || subject.UserID == "" {
		return ErrDenied
	}

	effectiveRole := RoleNone
	for _, scope := range ancestorScopes(repository.Path) {
		policy, err := s.loader.LoadPolicy(ctx, scope)
		if err != nil {
			if errors.Is(err, ErrPolicyNotFound) {
				continue
			}
			return fmt.Errorf("load policy for %q: %w", scope, err)
		}
		if err := validatePolicy(policy); err != nil {
			return fmt.Errorf("validate policy for %q: %w", scope, err)
		}
		if role := policy.Members[subject.UserID]; role > effectiveRole {
			effectiveRole = role
		}
	}
	if effectiveRole < minimumRole(action) {
		return ErrDenied
	}

	return nil
}

func validatePolicy(policy Policy) error {
	if policy.Generation == 0 {
		return errors.New("policy generation must be positive")
	}
	for userID, role := range policy.Members {
		if userID == "" {
			return errors.New("policy contains an empty user ID")
		}
		if role < RoleReader || role > RoleOwner {
			return fmt.Errorf("policy contains invalid role %d", role)
		}
	}

	return nil
}

func validateRequest(subject Subject, repository Repository, action Action) error {
	if action < ActionRead || action > ActionManage {
		return errors.New("unknown authorization action")
	}
	if repository.ID == "" {
		return errors.New("repository ID is required")
	}
	if repository.Visibility < VisibilityPrivate || repository.Visibility > VisibilityPublic {
		return errors.New("unknown repository visibility")
	}
	components := strings.Split(repository.Path, "/")
	if len(components) < 2 {
		return errors.New("repository path must include a top level and repository")
	}
	for _, component := range components {
		if !validComponent(component) {
			return errors.New("repository path is not canonical")
		}
	}
	if subject.Authenticated && subject.UserID == "" {
		return errors.New("authenticated subject requires an immutable user ID")
	}
	if !subject.Authenticated && subject.UserID != "" {
		return errors.New("anonymous subject cannot carry a user ID")
	}

	return nil
}

func ancestorScopes(path string) []string {
	components := strings.Split(path, "/")
	scopes := make([]string, 0, len(components))
	for index := range components {
		scopes = append(scopes, strings.Join(components[:index+1], "/"))
	}

	return scopes
}

func validComponent(component string) bool {
	if component == "" || component == "." || component == ".." {
		return false
	}
	for index := range len(component) {
		value := component[index]
		isLetter := value >= 'a' && value <= 'z'
		isDigit := value >= '0' && value <= '9'
		isPunctuation := value == '-' || value == '_' || value == '.'
		if !isLetter && !isDigit && !isPunctuation {
			return false
		}
	}

	return true
}

func minimumRole(action Action) Role {
	switch action {
	case ActionRead:
		return RoleReader
	case ActionWrite:
		return RoleDeveloper
	case ActionManage:
		return RoleOwner
	case ActionUnknown:
		return RoleNone
	default:
		return RoleNone
	}
}

var _ Authorizer = (*Service)(nil)
