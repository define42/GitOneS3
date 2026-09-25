package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/define42/GitOneS3/internal/authz"
	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/shard"
)

// TokenResolver pins authority traffic to deployment-owned shard destinations.
// No host, scheme, or forwarding identity is taken from a client credential.
type TokenResolver interface {
	Resolve(shard.ShardID) (*url.URL, error)
}

func parseTokenCredentials(r *http.Request) (tokenCredentials, bool) {
	if len(r.Header.Values("Authorization")) != 1 || len(r.Header.Get("Authorization")) > 512 {
		return tokenCredentials{}, false
	}
	username, raw, ok := r.BasicAuth()
	routeName, _, valid := splitToken(raw)
	if !ok || !valid || username != routeName {
		return tokenCredentials{}, false
	}
	return tokenCredentials{username, raw}, true
}

func isGitRequest(r *http.Request) bool {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		return false
	}
	return (len(parts) == 4 && parts[2] == "info" && parts[3] == "refs") ||
		(len(parts) == 3 && (parts[2] == "git-upload-pack" || parts[2] == "git-receive-pack"))
}

func (s *Service) verifyToken(ctx context.Context, credentials tokenCredentials) (tokenPrincipal, error) {
	owner, err := s.router.Owner(credentials.username)
	if err != nil || credentials.username == "auth" {
		return tokenPrincipal{}, errInvalidToken
	}
	if owner == s.local {
		record, err := s.verifyLocalToken(ctx, credentials.username, credentials.raw)
		if err != nil {
			return tokenPrincipal{}, err
		}
		return principalFor(record), nil
	}
	if s.tokenResolver == nil {
		return tokenPrincipal{}, errors.New("token authority resolver unavailable")
	}
	destination, err := s.tokenResolver.Resolve(owner)
	if err != nil || destination == nil {
		return tokenPrincipal{}, errors.New("token authority unavailable")
	}
	if (destination.Scheme != "http" && destination.Scheme != "https") || destination.Host == "" || destination.User != nil || destination.Opaque != "" {
		return tokenPrincipal{}, errors.New("invalid token authority destination")
	}
	u := *destination
	u.Path, u.RawPath = "/api/v1/users/"+credentials.username+"/tokens/verify", ""
	u.RawQuery, u.Fragment = "", ""
	// The validated username only selects the fixed verification path.
	// #nosec G704 -- The resolver supplies a deployment-owned shard URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return tokenPrincipal{}, errors.New("invalid token authority request")
	}
	req.SetBasicAuth(credentials.username, credentials.raw)
	req.Header.Set("Accept", "application/json")
	// #nosec G704 -- The authority URL comes from the deployment resolver, and tokenClient refuses all redirects.
	response, err := s.tokenClient.Do(req)
	if err != nil {
		return tokenPrincipal{}, errors.New("token authority unavailable")
	}
	// Closing a read-only response cannot change the verification result.
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized {
		return tokenPrincipal{}, errInvalidToken
	}
	if response.StatusCode != http.StatusOK {
		return tokenPrincipal{}, errors.New("token authority unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, tokenRecordLimit+1))
	if err != nil || len(data) > tokenRecordLimit {
		return tokenPrincipal{}, errors.New("invalid token authority response")
	}
	var principal tokenPrincipal
	if json.Unmarshal(data, &principal) != nil {
		return tokenPrincipal{}, errors.New("invalid token authority response")
	}
	_, id, ok := splitToken(credentials.raw)
	if !ok || principal.Username != credentials.username || principal.TokenID != id || !validIdentity(principal.Identity) ||
		identityIssuer(principal.Identity) != s.issuer || (principal.Permission != "read" && principal.Permission != "write") ||
		!s.validTokenScopes(principal.AllRepositories, principal.Repositories) || !principal.ExpiresAt.After(time.Now()) {
		return tokenPrincipal{}, errors.New("invalid token authority response")
	}
	return principal, nil
}

func (s *Service) serveGit(w http.ResponseWriter, r *http.Request, namespace string) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	repo := strings.TrimSuffix(parts[1], ".git")
	if !repository.ValidName(repo) {
		http.Error(w, "invalid repository", http.StatusBadRequest)
		return
	}
	write := strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
	var credentials tokenCredentials
	usingPAT := len(r.Header.Values("Authorization")) != 0
	if usingPAT {
		var ok bool
		credentials, ok = parseTokenCredentials(r)
		if !ok {
			gitAuthError(w, huma.Error401Unauthorized("invalid credentials"))
			return
		}
	}
	// Reuse exactly the same credential and current ACL checks before publishing
	// a push. Never cache positive authentication across Git HTTP requests.
	authorize := func(ctx context.Context) (session, string, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var current session
		if usingPAT {
			principal, err := s.verifyToken(ctx, credentials)
			if errors.Is(err, errInvalidToken) {
				return current, "", huma.Error401Unauthorized("invalid credentials")
			}
			if err != nil {
				return current, "", huma.Error503ServiceUnavailable("token authority unavailable")
			}
			// This broadens only token scope. The live namespace permission check
			// below still applies, including on the final push publication check.
			if (!principal.AllRepositories && !slices.Contains(principal.Repositories, namespace+"/"+repo)) || (write && principal.Permission != "write") {
				return current, "", huma.Error403Forbidden("token does not permit this repository operation")
			}
			if origin := r.Header.Get("Origin"); origin != "" && origin != s.origin {
				return current, "", huma.Error403Forbidden("invalid origin")
			}
			current = session{Username: principal.Username, Identity: principal.Identity}
		} else {
			var err error
			current, err = s.readSession(r)
			if err != nil {
				return current, "", huma.Error401Unauthorized("authentication required")
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions &&
				(r.Header.Get("Origin") != s.origin || !equal(r.Header.Get("X-CSRF-Token"), current.CSRF)) {
				return current, "", huma.Error403Forbidden("invalid CSRF token or origin")
			}
		}
		ctx = context.WithValue(ctx, apiSessionKey{}, apiSession{current: current, isAuthenticated: true})
		role, err := s.repositoryRole(ctx, namespace, write)
		return current, role, err
	}
	current, _, err := authorize(r.Context())
	if err != nil {
		gitAuthError(w, err)
		return
	}
	ctx := context.WithValue(r.Context(), subjectKey{}, authz.Subject{UserID: userID(current.Identity), Authenticated: true})
	if write {
		ctx = gittransport.WithWriteAuthorization(ctx, func(ctx context.Context) error { _, _, err := authorize(ctx); return err })
	}
	s.next.ServeHTTP(w, r.WithContext(ctx))
}

func gitAuthError(w http.ResponseWriter, err error) {
	status, message := http.StatusServiceUnavailable, "authentication unavailable"
	if apiErr, ok := errors.AsType[huma.StatusError](err); ok {
		status, message = apiErr.GetStatus(), apiErr.Error()
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitOne", charset="UTF-8"`)
	}
	http.Error(w, message, status)
}
