package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/define42/GitOneS3/internal/storage"
)

type createTokenInput struct {
	Name string `path:"name" minLength:"1" maxLength:"63"`
	Body struct {
		Name            string   `json:"name" minLength:"1" maxLength:"100"`
		Permission      string   `json:"permission" enum:"read,write"`
		AllRepositories bool     `json:"allRepositories,omitempty" doc:"Include all current and future repositories the user can access; repositories must be empty when true."`
		Repositories    []string `json:"repositories,omitempty" maxItems:"100" uniqueItems:"true" doc:"Select 1 to 100 exact namespace/repository scopes unless allRepositories is true."`
		ExpiresInDays   int      `json:"expiresInDays,omitempty" default:"30" minimum:"1" maximum:"90"`
	}
}

type createTokenOutput struct {
	Body struct {
		Token    string    `json:"token"`
		Metadata tokenView `json:"metadata"`
	}
}

type listTokensOutput struct {
	Body struct {
		Tokens     []tokenView `json:"tokens"`
		NextCursor string      `json:"nextCursor,omitempty"`
	}
}
type listTokensInput struct {
	Name  string `path:"name" minLength:"1" maxLength:"63"`
	After string `query:"after" maxLength:"32"`
}
type revokeTokenInput struct {
	Name string `path:"name" minLength:"1" maxLength:"63"`
	ID   string `path:"id" pattern:"^[a-f0-9]{32}$"`
}

type tokenPrincipal struct {
	Username        string    `json:"username"`
	TokenID         string    `json:"tokenId"`
	Identity        Identity  `json:"identity"`
	Permission      string    `json:"permission"`
	AllRepositories bool      `json:"allRepositories,omitempty"`
	Repositories    []string  `json:"repositories"`
	ExpiresAt       time.Time `json:"expiresAt"`
}
type verifyTokenOutput struct{ Body tokenPrincipal }
type tokenCredentials struct{ username, raw string }
type tokenCredentialsKey struct{}

func (s *Service) registerTokenAPI(api huma.API) {
	registerAPI(api, "list-tokens", "GET", "/api/v1/users/{name}/tokens", "List your personal access tokens", false, http.StatusOK, s.apiListTokens)
	huma.Register(api, huma.Operation{OperationID: "create-token", Method: "POST", Path: "/api/v1/users/{name}/tokens",
		Summary: "Create a scoped token; its secret is returned only once", DefaultStatus: http.StatusCreated,
		MaxBodyBytes: 16 << 10, BodyReadTimeout: 5 * time.Second, Security: []map[string][]string{{"session": {}}},
		Errors: []int{400, 401, 403, 409, 413, 422, 503}}, s.apiCreateToken)
	registerAPI(api, "revoke-token", "DELETE", "/api/v1/users/{name}/tokens/{id}", "Revoke a personal access token", false, http.StatusNoContent, s.apiRevokeToken)
	huma.Register(api, huma.Operation{OperationID: "verify-token", Method: "POST", Path: "/api/v1/users/{name}/tokens/verify",
		Summary: "Verify a PAT against its authoritative user shard", DefaultStatus: http.StatusOK,
		MaxBodyBytes: 4096, BodyReadTimeout: 5 * time.Second, Security: []map[string][]string{{"pat": {}}}, Errors: []int{400, 401, 403, 503}}, s.apiVerifyToken)
}

func (s *Service) tokenOwner(ctx context.Context, username string) (session, error) {
	current := currentAPISession(ctx)
	if !current.isAuthenticated || current.current.Username != username {
		return session{}, huma.Error403Forbidden("tokens can only be managed by their owner")
	}
	if _, err := s.repositoryRole(ctx, username, true); err != nil {
		return session{}, err
	}
	return current.current, nil
}

func (s *Service) apiListTokens(ctx context.Context, input *listTokensInput) (*listTokensOutput, error) {
	if _, err := s.tokenOwner(ctx, input.Name); err != nil {
		return nil, err
	}
	if input.After != "" && !validTokenID(input.After) {
		return nil, huma.Error400BadRequest("invalid token cursor")
	}
	views, cursor, err := s.listTokenPage(ctx, input.Name, input.After)
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("tokens unavailable")
	}
	out := &listTokensOutput{}
	out.Body.Tokens = views
	out.Body.NextCursor = cursor
	return out, nil
}

func (s *Service) apiCreateToken(ctx context.Context, input *createTokenInput) (*createTokenOutput, error) {
	current, err := s.tokenOwner(ctx, input.Name)
	if err != nil {
		return nil, err
	}
	// Validate settings before attempting storage so invalid scopes are a 400.
	probe := tokenView{ID: "00000000000000000000000000000000", Username: input.Name, Name: input.Body.Name,
		Permission: input.Body.Permission, AllRepositories: input.Body.AllRepositories, Repositories: sortedScopes(input.Body.Repositories), CreatedAt: time.Now().UTC()}
	probe.ExpiresAt = probe.CreatedAt.Add(time.Duration(input.Body.ExpiresInDays) * 24 * time.Hour)
	if !s.validTokenMetadata(probe) {
		return nil, huma.Error400BadRequest("invalid token settings or repository scopes")
	}
	raw, metadata, err := s.createToken(ctx, input.Name, current.Identity, input.Body.Name, input.Body.Permission, input.Body.Repositories, input.Body.AllRepositories, input.Body.ExpiresInDays)
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("token could not be created")
	}
	out := &createTokenOutput{}
	out.Body.Token, out.Body.Metadata = raw, metadata
	return out, nil
}

func (s *Service) apiRevokeToken(ctx context.Context, input *revokeTokenInput) (*struct{}, error) {
	if _, err := s.tokenOwner(ctx, input.Name); err != nil {
		return nil, err
	}
	err := s.revokeToken(ctx, input.Name, input.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, huma.Error404NotFound("token not found")
	}
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("token could not be revoked")
	}
	return nil, nil
}

func (s *Service) apiVerifyToken(ctx context.Context, input *nameInput) (*verifyTokenOutput, error) {
	credentials, ok := ctx.Value(tokenCredentialsKey{}).(tokenCredentials)
	if !ok || credentials.username != input.Name {
		return nil, tokenUnauthorized()
	}
	record, err := s.verifyLocalToken(ctx, credentials.username, credentials.raw)
	if errors.Is(err, errInvalidToken) {
		return nil, tokenUnauthorized()
	}
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("token authority unavailable")
	}
	return &verifyTokenOutput{Body: principalFor(record)}, nil
}

func tokenUnauthorized() error {
	return huma.ErrorWithHeaders(huma.Error401Unauthorized("invalid credentials"),
		http.Header{"WWW-Authenticate": {`Basic realm="GitOne", charset="UTF-8"`}})
}

func principalFor(record tokenRecord) tokenPrincipal {
	return tokenPrincipal{Username: record.Metadata.Username, TokenID: record.Metadata.ID, Identity: record.Identity,
		Permission: record.Metadata.Permission, AllRepositories: record.Metadata.AllRepositories, Repositories: record.Metadata.Repositories, ExpiresAt: record.Metadata.ExpiresAt}
}
