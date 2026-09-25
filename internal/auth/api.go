package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/define42/GitOneS3/internal/config"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

type apiSessionKey struct{}

type apiSession struct {
	current         session
	isAuthenticated bool
}

type sessionView struct {
	Authenticated bool      `json:"authenticated"`
	Username      string    `json:"username,omitempty"`
	Identity      *Identity `json:"identity,omitempty"`
	UserID        string    `json:"userId,omitempty"`
	CSRF          string    `json:"csrfToken,omitempty"`
	ShardCount    uint32    `json:"shardCount"`
	Provider      string    `json:"provider" enum:"google,oidc"`
	SSHURL        string    `json:"sshURL,omitempty"`
}

type sessionOutput struct {
	Body sessionView
}

type nameInput struct {
	Name string `path:"name" minLength:"1" maxLength:"63" pattern:"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"`
}

type nameOutput struct {
	Body struct {
		Name      string `json:"name"`
		Available bool   `json:"available"`
	}
}

type userOutput struct {
	Body struct {
		Username string `json:"username"`
		UserID   string `json:"userId"`
	}
}

type spacesInput struct {
	Shard  uint32 `query:"shard" required:"true" minimum:"0"`
	Cursor string `query:"cursor" maxLength:"4096"`
	Limit  int    `query:"limit" default:"100" minimum:"1" maximum:"100"`
}

type spaceView struct {
	Name    string `json:"name"`
	Type    string `json:"type" enum:"group"`
	Role    string `json:"role" enum:"reader,developer,owner"`
	Invited bool   `json:"invited"`
}

type spacesOutput struct {
	Body struct {
		Spaces     []spaceView `json:"spaces"`
		NextCursor string      `json:"nextCursor,omitempty"`
	}
}

type groupOutput struct {
	Status   int
	Location string `header:"Location"`
	Body     *groupView
}

type invitationOutput struct {
	Body struct {
		Name string `json:"name"`
		Role string `json:"role" enum:"reader,developer,owner"`
	}
}

type memberInput struct {
	Name string `path:"name" minLength:"1" maxLength:"63" pattern:"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"`
	Body struct {
		UserID string `json:"userId" minLength:"1" maxLength:"262"`
		Role   string `json:"role" enum:"reader,developer,owner"`
	}
}

type removeMemberInput struct {
	Name string `path:"name" minLength:"1" maxLength:"63" pattern:"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"`
	Body struct {
		UserID string `json:"userId" minLength:"1" maxLength:"262"`
	}
}

type logoutOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
}

// resolveAPI chooses a destination only from validated names or shard numbers.
// Clients cannot supply forwarding hosts, and all shards verify the same cookie.
func (s *Service) resolveAPI(r *http.Request) (shard.Route, error) {
	local := shard.Route{Owner: s.local}
	if r == nil || r.URL == nil || r.URL.Opaque != "" {
		return shard.Route{}, errors.New("invalid api url")
	}
	path := r.URL.Path
	if r.URL.RawPath != "" || !strings.HasPrefix(path, "/api/") || r.URL.EscapedPath() != path ||
		strings.Contains(path, "//") || strings.ContainsAny(path, "\\%#") {
		return shard.Route{}, errors.New("invalid api path")
	}
	if r.RequestURI != "" {
		target, _, _ := strings.Cut(r.RequestURI, "?")
		if target != path {
			return shard.Route{}, errors.New("invalid api request target")
		}
	}
	for part := range strings.SplitSeq(r.URL.Path, "/") {
		if part == "." || part == ".." {
			return shard.Route{}, errors.New("invalid api path")
		}
	}
	if r.URL.Path == "/api/v1/spaces" {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query["shard"]) != 1 || len(query["cursor"]) > 1 || len(query["limit"]) > 1 {
			return shard.Route{}, errors.New("exactly one shard is required")
		}
		value := query.Get("shard")
		owner, err := strconv.ParseUint(value, 10, 32)
		if err != nil || owner >= uint64(s.ShardCount()) || strconv.FormatUint(owner, 10) != value {
			return shard.Route{}, errors.New("invalid shard")
		}
		return shard.Route{Owner: shard.ShardID(owner)}, nil
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) >= 4 && parts[0] == "api" && parts[1] == "v1" &&
		(parts[2] == "names" || parts[2] == "users" || parts[2] == "groups" || parts[2] == "repos") {
		name := parts[3]
		owner, err := s.router.Owner(name)
		if err != nil || name == "auth" {
			return shard.Route{}, errors.New("invalid namespace")
		}
		return shard.Route{Owner: owner, Path: shard.RequestPath{TopLevel: name}}, nil
	}
	return local, nil
}

func (s *Service) newAPIHandler() http.Handler {
	mux := http.NewServeMux()
	apiConfig := huma.DefaultConfig("GitOne API", "1.0.0")
	apiConfig.OpenAPIPath = "/api/openapi"
	apiConfig.DocsPath = "/api/docs"
	apiConfig.SchemasPath = "/api/schemas"
	apiConfig.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"pat": {Type: "http", Scheme: "basic", Description: "GitOne username and personal access token."},
		"session": {Type: "apiKey", In: "cookie", Name: sessionCookie,
			Description: "OIDC session cookie. Mutations also require the configured Origin and X-CSRF-Token from the session response."},
	}
	api := humago.New(mux, apiConfig)
	api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		r, _ := humago.Unwrap(ctx)
		if ctx.Operation().OperationID == "verify-token" {
			credentials, ok := parseTokenCredentials(r)
			if !ok {
				ctx.SetHeader("WWW-Authenticate", `Basic realm="GitOne", charset="UTF-8"`)
				_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "invalid credentials")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && origin != s.origin {
				_ = huma.WriteErr(api, ctx, http.StatusForbidden, "invalid origin")
				return
			}
			requestCtx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
			defer cancel()
			next(huma.WithValue(huma.WithContext(ctx, requestCtx), tokenCredentialsKey{}, credentials))
			return
		}
		current, err := s.readSession(r)
		public := ctx.Operation().OperationID == "get-session" || ctx.Operation().OperationID == "check-name"
		if err != nil && !public {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "authentication required")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions &&
			(r.Header.Get("Origin") != s.origin || !equal(r.Header.Get("X-CSRF-Token"), current.CSRF)) {
			_ = huma.WriteErr(api, ctx, http.StatusForbidden, "invalid CSRF token or origin")
			return
		}
		requestCtx, cancel := context.WithTimeout(ctx.Context(), 15*time.Second)
		defer cancel()
		next(huma.WithValue(huma.WithContext(ctx, requestCtx), apiSessionKey{}, apiSession{current, err == nil}))
	})

	registerAPI(api, "get-session", "GET", "/api/v1/session", "Get the current session", true, http.StatusOK, s.apiSession)
	registerAPI(api, "logout", "POST", "/api/v1/logout", "Sign out of GitOne", false, http.StatusNoContent, s.apiLogout)
	registerAPI(api, "check-name", "GET", "/api/v1/names/{name}", "Check username or group availability", true, http.StatusOK, s.apiName)
	registerAPI(api, "get-user", "GET", "/api/v1/users/{name}", "Resolve a username for a group invitation", false, http.StatusOK, s.apiUser)
	registerAPI(api, "list-spaces", "GET", "/api/v1/spaces", "List memberships and invitations on one shard", false, http.StatusOK, s.apiSpaces)
	registerAPI(api, "create-group", "POST", "/api/v1/groups/{name}", "Create a shared group", false, http.StatusCreated, s.apiCreateGroup)
	registerAPI(api, "get-group", "GET", "/api/v1/groups/{name}", "Get a shared group", false, http.StatusOK, s.apiGroup)
	registerAPI(api, "get-invitation", "GET", "/api/v1/groups/{name}/invitation", "Preview your group invitation", false, http.StatusOK, s.apiInvitation)
	registerAPI(api, "invite-member", "POST", "/api/v1/groups/{name}/invitations", "Invite a user to a group", false, http.StatusOK,
		func(ctx context.Context, input *memberInput) (*groupOutput, error) {
			return s.apiUpdateGroup(ctx, input.Name, "invite", input.Body.UserID, input.Body.Role)
		})
	registerAPI(api, "cancel-invitation", "DELETE", "/api/v1/groups/{name}/invitations", "Cancel a group invitation", false, http.StatusOK,
		func(ctx context.Context, input *removeMemberInput) (*groupOutput, error) {
			return s.apiUpdateGroup(ctx, input.Name, "cancel", input.Body.UserID, "")
		})
	registerAPI(api, "accept-invitation", "POST", "/api/v1/groups/{name}/invitations/accept", "Accept your group invitation", false, http.StatusOK,
		func(ctx context.Context, input *nameInput) (*groupOutput, error) {
			return s.apiUpdateGroup(ctx, input.Name, "accept", "", "")
		})
	registerAPI(api, "set-member-role", "PUT", "/api/v1/groups/{name}/members", "Change a group member's role", false, http.StatusOK,
		func(ctx context.Context, input *memberInput) (*groupOutput, error) {
			return s.apiUpdateGroup(ctx, input.Name, "set-role", input.Body.UserID, input.Body.Role)
		})
	registerAPI(api, "remove-member", "DELETE", "/api/v1/groups/{name}/members", "Remove a group member", false, http.StatusOK,
		func(ctx context.Context, input *removeMemberInput) (*groupOutput, error) {
			return s.apiUpdateGroup(ctx, input.Name, "remove", input.Body.UserID, "")
		})
	s.registerRepositoryAPI(api)
	s.registerTokenAPI(api)
	s.registerSSHKeyAPI(api)
	return mux
}

func registerAPI[I, O any](api huma.API, id, method, path, summary string, public bool, status int,
	handler func(context.Context, *I) (*O, error)) {
	op := huma.Operation{OperationID: id, Method: method, Path: path, Summary: summary,
		DefaultStatus: status, MaxBodyBytes: 4096, BodyReadTimeout: 5 * time.Second,
		Errors: []int{400, 401, 403, 404, 409, 413, 422, 503}}
	if !public {
		op.Security = []map[string][]string{{"session": {}}}
	}
	huma.Register(api, op, handler)
}

func currentAPISession(ctx context.Context) apiSession {
	current, _ := ctx.Value(apiSessionKey{}).(apiSession)
	return current
}

func (s *Service) apiSession(ctx context.Context, _ *struct{}) (*sessionOutput, error) {
	current := currentAPISession(ctx)
	provider := "oidc"
	if s.issuer == config.GoogleIssuer {
		provider = "google"
	}
	view := sessionView{Authenticated: current.isAuthenticated, ShardCount: s.ShardCount(), Provider: provider}
	view.SSHURL = s.sshPublicURL
	if current.isAuthenticated {
		view.Username = current.current.Username
		view.Identity = &current.current.Identity
		view.UserID = userID(current.current.Identity)
		view.CSRF = current.current.CSRF
	}
	return &sessionOutput{Body: view}, nil
}

func (s *Service) apiLogout(context.Context, *struct{}) (*logoutOutput, error) {
	return &logoutOutput{SetCookie: http.Cookie{Name: sessionCookie, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)}}, nil
}

func (s *Service) apiName(ctx context.Context, input *nameInput) (*nameOutput, error) {
	_, _, err := s.loadNamespace(ctx, input.Name)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, apiError(err)
	}
	output := &nameOutput{}
	output.Body.Name = input.Name
	output.Body.Available = errors.Is(err, storage.ErrNotFound)
	return output, nil
}

func (s *Service) apiUser(ctx context.Context, input *nameInput) (*userOutput, error) {
	record, _, err := s.loadNamespace(ctx, input.Name)
	if err != nil {
		return nil, apiError(err)
	}
	if record.Type != userNamespace {
		return nil, huma.Error404NotFound("user not found")
	}
	output := &userOutput{}
	output.Body.Username = input.Name
	output.Body.UserID = userID(record.Identity)
	return output, nil
}

func (s *Service) apiSpaces(ctx context.Context, input *spacesInput) (*spacesOutput, error) {
	if input.Shard != uint32(s.local) {
		return nil, huma.Error400BadRequest("invalid shard")
	}
	current := currentAPISession(ctx)
	if !current.isAuthenticated {
		return nil, huma.Error401Unauthorized("authentication required")
	}
	if input.Limit < 1 || input.Limit > spacePageSize {
		return nil, huma.Error400BadRequest("invalid space page size")
	}
	id := userID(current.current.Identity)
	after, err := s.decodeSpaceCursor(input.Cursor, id)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid space cursor")
	}
	spaces, next, err := s.listSpaces(ctx, id, after, input.Limit)
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("space discovery unavailable")
	}
	output := &spacesOutput{}
	output.Body.Spaces = spaces
	if next != "" {
		output.Body.NextCursor, err = s.encodeSpaceCursor(next, id)
		if err != nil {
			return nil, huma.Error503ServiceUnavailable("space discovery unavailable")
		}
	}
	return output, nil
}

func (s *Service) apiCreateGroup(ctx context.Context, input *nameInput) (*groupOutput, error) {
	current := currentAPISession(ctx).current
	record, err := s.createGroup(ctx, input.Name, userID(current.Identity))
	if err != nil {
		return nil, apiError(err)
	}
	return &groupOutput{Status: http.StatusCreated, Location: "/" + input.Name + "/", Body: apiGroupView(input.Name, record, current)}, nil
}

func (s *Service) apiGroup(ctx context.Context, input *nameInput) (*groupOutput, error) {
	current := currentAPISession(ctx).current
	record, _, err := s.loadNamespace(ctx, input.Name)
	if err != nil {
		return nil, apiError(err)
	}
	if record.Type != groupNamespace {
		return nil, apiError(errNotGroup)
	}
	if record.Members[userID(current.Identity)] == "" {
		return nil, apiError(errGroupDenied)
	}
	return &groupOutput{Status: http.StatusOK, Body: apiGroupView(input.Name, record, current)}, nil
}

func (s *Service) apiInvitation(ctx context.Context, input *nameInput) (*invitationOutput, error) {
	record, _, err := s.loadNamespace(ctx, input.Name)
	if err != nil {
		return nil, apiError(err)
	}
	role := record.Invitations[userID(currentAPISession(ctx).current.Identity)]
	if record.Type != groupNamespace || role == "" {
		return nil, huma.Error404NotFound("invitation not found")
	}
	output := &invitationOutput{}
	output.Body.Name, output.Body.Role = input.Name, role
	return output, nil
}

func (s *Service) apiUpdateGroup(ctx context.Context, name, action, target, role string) (*groupOutput, error) {
	current := currentAPISession(ctx).current
	record, err := s.updateGroup(ctx, name, userID(current.Identity), action, target, role)
	if err != nil {
		return nil, apiError(err)
	}
	if record.Members[userID(current.Identity)] == "" {
		return &groupOutput{Status: http.StatusNoContent}, nil
	}
	return &groupOutput{Status: http.StatusOK, Body: apiGroupView(name, record, current)}, nil
}

func apiGroupView(name string, record namespaceRecord, current session) *groupView {
	view := &groupView{Name: name, Type: groupNamespace, CreatorUserID: record.CreatorUserID,
		Role: record.Members[userID(current.Identity)], Members: record.Members, CSRF: current.CSRF}
	if view.Role == "owner" {
		view.Invitations = record.Invitations
	}
	return view
}

func apiError(err error) error {
	switch {
	case errors.Is(err, errNamespaceTaken), errors.Is(err, errLastOwner), errors.Is(err, errGroupConflict), errors.Is(err, errGroupFull):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, errGroupDenied):
		return huma.Error403Forbidden("forbidden")
	case errors.Is(err, errNotGroup), errors.Is(err, errMemberNotFound), errors.Is(err, storage.ErrNotFound):
		return huma.Error404NotFound("group, user, member, or invitation not found")
	case errors.Is(err, errInvalidMember):
		return huma.Error400BadRequest(err.Error())
	default:
		return huma.Error503ServiceUnavailable("authentication service unavailable")
	}
}
