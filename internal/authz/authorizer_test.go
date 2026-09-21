package authz

import (
	"context"
	"errors"
	"testing"
)

func TestService_Check(t *testing.T) {
	t.Parallel()

	loader := mapLoader{
		"acme": {
			Generation: 1,
			Members: map[string]Role{
				"alice-id": RoleOwner,
				"bob-id":   RoleDeveloper,
			},
		},
		"acme/platform": {
			Generation: 2,
			Members: map[string]Role{
				"carol-id": RoleReader,
			},
		},
		"acme/security/scanner": {
			Generation: 4,
			Members: map[string]Role{
				"dave-id": RoleDeveloper,
			},
		},
	}
	service, err := New(loader)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := []struct {
		name       string
		subject    Subject
		repository Repository
		action     Action
		wantDenied bool
	}{
		{
			name:       "root owner manages descendant",
			subject:    Subject{UserID: "alice-id", Authenticated: true},
			repository: privateRepository("acme/platform/linux"),
			action:     ActionManage,
		},
		{
			name:       "root developer writes descendant",
			subject:    Subject{UserID: "bob-id", Authenticated: true},
			repository: privateRepository("acme/platform/linux"),
			action:     ActionWrite,
		},
		{
			name:       "subgroup reader reads descendant",
			subject:    Subject{UserID: "carol-id", Authenticated: true},
			repository: privateRepository("acme/platform/linux"),
			action:     ActionRead,
		},
		{
			name:       "direct repository grant cannot reach sibling",
			subject:    Subject{UserID: "dave-id", Authenticated: true},
			repository: privateRepository("acme/security/audit"),
			action:     ActionWrite,
			wantDenied: true,
		},
		{
			name:       "anonymous public read",
			subject:    Subject{},
			repository: repositoryWithVisibility("acme/public/site", VisibilityPublic),
			action:     ActionRead,
		},
		{
			name:       "authenticated internal read",
			subject:    Subject{UserID: "eve-id", Authenticated: true},
			repository: repositoryWithVisibility("acme/internal/docs", VisibilityInternal),
			action:     ActionRead,
		},
		{
			name:       "anonymous private read denied",
			subject:    Subject{},
			repository: privateRepository("acme/platform/linux"),
			action:     ActionRead,
			wantDenied: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := service.Check(context.Background(), test.subject, test.repository, test.action)
			if test.wantDenied && !errors.Is(err, ErrDenied) {
				t.Fatalf("Check() error = %v, want ErrDenied", err)
			}
			if !test.wantDenied && err != nil {
				t.Fatalf("Check() error = %v", err)
			}
		})
	}
}

type mapLoader map[string]Policy

func (m mapLoader) LoadPolicy(_ context.Context, path string) (Policy, error) {
	policy, ok := m[path]
	if !ok {
		return Policy{}, ErrPolicyNotFound
	}

	return policy, nil
}

func privateRepository(path string) Repository {
	return repositoryWithVisibility(path, VisibilityPrivate)
}

func repositoryWithVisibility(path string, visibility Visibility) Repository {
	return Repository{ID: "repo-id", Path: path, Visibility: visibility}
}
