package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/define42/GitOneS3/internal/storage"
)

type createSSHKeyInput struct {
	Name string `path:"name" minLength:"1" maxLength:"63"`
	Body struct {
		Name      string `json:"name" minLength:"1" maxLength:"100"`
		PublicKey string `json:"publicKey" minLength:"1" maxLength:"4096"`
	}
}

type createSSHKeyOutput struct {
	Body struct {
		Key sshKeyView `json:"key"`
	}
}

type listSSHKeysOutput struct {
	Body struct {
		Keys []sshKeyView `json:"keys"`
	}
}

type revokeSSHKeyInput struct {
	Name string `path:"name" minLength:"1" maxLength:"63"`
	ID   string `path:"id" pattern:"^[a-f0-9]{64}$"`
}

func (s *Service) registerSSHKeyAPI(api huma.API) {
	registerAPI(
		api,
		"list-ssh-keys",
		"GET",
		"/api/v1/users/{name}/ssh-keys",
		"List your SSH public keys, including revoked keys",
		false,
		http.StatusOK,
		s.apiListSSHKeys,
	)
	huma.Register(api, huma.Operation{OperationID: "create-ssh-key", Method: "POST",
		Path: "/api/v1/users/{name}/ssh-keys", Summary: "Register an SSH public key for Git access",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 8 << 10, BodyReadTimeout: 5 * time.Second,
		Security: []map[string][]string{{"session": {}}}, Errors: []int{400, 401, 403, 409, 413, 422, 503}},
		s.apiCreateSSHKey)
	registerAPI(
		api,
		"revoke-ssh-key",
		"DELETE",
		"/api/v1/users/{name}/ssh-keys/{id}",
		"Permanently revoke an SSH public key",
		false,
		http.StatusNoContent,
		s.apiRevokeSSHKey,
	)
}

func (s *Service) sshKeyOwner(ctx context.Context, username string) (SSHPrincipal, error) {
	current := currentAPISession(ctx)
	if !current.isAuthenticated || current.current.Username != username {
		return SSHPrincipal{}, huma.Error403Forbidden("SSH keys can only be managed by their owner")
	}
	if _, err := s.repositoryRole(ctx, username, true); err != nil {
		return SSHPrincipal{}, err
	}
	return SSHPrincipal{Username: username, Identity: current.current.Identity}, nil
}

func (s *Service) apiListSSHKeys(ctx context.Context, input *nameInput) (*listSSHKeysOutput, error) {
	if _, err := s.sshKeyOwner(ctx, input.Name); err != nil {
		return nil, err
	}
	keys, err := s.listSSHKeys(ctx, input.Name)
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("SSH keys unavailable")
	}
	out := &listSSHKeysOutput{}
	out.Body.Keys = keys
	return out, nil
}

func (s *Service) apiCreateSSHKey(ctx context.Context, input *createSSHKeyInput) (*createSSHKeyOutput, error) {
	principal, err := s.sshKeyOwner(ctx, input.Name)
	if err != nil {
		return nil, err
	}
	key, err := s.createSSHKey(ctx, principal, input.Body.Name, input.Body.PublicKey)
	switch {
	case errors.Is(err, errInvalidSSHKey):
		return nil, huma.Error400BadRequest("provide a name and one Ed25519, ECDSA, or 3072–8192-bit RSA public key without options")
	case errors.Is(err, errSSHKeyExists):
		return nil, huma.Error409Conflict("this public key was already registered; revoked keys cannot be reused")
	case errors.Is(err, errSSHKeyLimit):
		return nil, huma.Error409Conflict("SSH key limit reached (100 records including revoked keys)")
	case errors.Is(err, storage.ErrConditionalConflict):
		return nil, huma.Error409Conflict("SSH keys changed concurrently; retry the request")
	case err != nil:
		return nil, huma.Error503ServiceUnavailable("SSH key could not be registered")
	}
	out := &createSSHKeyOutput{}
	out.Body.Key = key
	return out, nil
}

func (s *Service) apiRevokeSSHKey(ctx context.Context, input *revokeSSHKeyInput) (*struct{}, error) {
	if _, err := s.sshKeyOwner(ctx, input.Name); err != nil {
		return nil, err
	}
	err := s.revokeSSHKey(ctx, input.Name, input.ID)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return nil, huma.Error404NotFound("SSH key not found")
	case errors.Is(err, storage.ErrConditionalConflict):
		return nil, huma.Error409Conflict("SSH keys changed concurrently; retry the request")
	case err != nil:
		return nil, huma.Error503ServiceUnavailable("SSH key could not be revoked")
	}
	return nil, nil
}
