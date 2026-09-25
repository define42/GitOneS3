package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/gorilla/securecookie"

	"github.com/define42/GitOneS3/internal/authz"
	"github.com/define42/GitOneS3/internal/config"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

const (
	loginLifetime    = 10 * time.Minute
	sessionLifetime  = 12 * time.Hour
	sessionCookie    = "__Host-gitone-session"
	loginCookie      = "__Host-gitone-login"
	stateLabel       = "gitone-google-state-v1"
	transactionLabel = "gitone-google-transaction-v1"
	// State and the encrypted transaction must both fit securecookie's 4096
	// byte encoded limit. Budget serialized bytes, including JSON escaping.
	maxReturnToBytes = 1024
)

type loginState struct {
	Issuer   string `json:",omitempty"`
	Mode     string `json:",omitempty"`
	UI       bool   `json:",omitempty"`
	ReturnTo string `json:",omitempty"`
	Username string
	Owner    shard.ShardID
	ID       string
	Origin   string
	Expires  int64
}

type transaction struct {
	State       loginState
	BrowserHash [32]byte
	Nonce       string
	Verifier    string
}

type session struct {
	Username string
	Identity Identity
	Origin   string
	CSRF     string
	Expires  int64
}

type subjectKey struct{}

// Subject returns the verified provider-scoped identity attached to a request.
func Subject(ctx context.Context) (authz.Subject, bool) {
	subject, ok := ctx.Value(subjectKey{}).(authz.Subject)
	return subject, ok
}

// Options supplies only shard-local storage; callbacks are routed before use.
type Options struct {
	Config         config.Auth
	LocalShard     shard.ShardID
	Router         *shard.Router
	Store          storage.ObjectStore
	Provider       Provider
	Next           http.Handler
	TokenResolver  TokenResolver
	TokenTransport http.RoundTripper
}

// Service resolves callback state on any pod and authenticates on the owner.
type Service struct {
	local         shard.ShardID
	router        *shard.Router
	store         storage.ObjectStore
	repositories  *repository.Store
	provider      Provider
	next          http.Handler
	origin        string
	issuer        string
	callbackPath  string
	loginCodec    *securecookie.SecureCookie
	sessionCodec  *securecookie.SecureCookie
	api           http.Handler
	tokenResolver TokenResolver
	tokenClient   *http.Client
}

func New(options Options) (*Service, error) {
	if err := options.Config.Validate(); err != nil {
		return nil, err
	}
	if !options.Config.Enabled || options.Router == nil || options.Store == nil || options.Provider == nil || options.Next == nil {
		return nil, errors.New("authentication requires enabled config, router, store, provider, and next handler")
	}
	if uint32(options.LocalShard) >= options.Router.ShardCount() {
		return nil, errors.New("authentication shard is outside configured range")
	}
	hash, block, err := options.Config.CookieKeys()
	if err != nil {
		return nil, err
	}
	repositories, err := repository.New(options.Store)
	if err != nil {
		return nil, err
	}
	service := &Service{
		local: options.LocalShard, router: options.Router, store: options.Store,
		repositories: repositories,
		provider:     options.Provider, next: options.Next, origin: options.Config.PublicURL,
		tokenResolver: options.TokenResolver,
		tokenClient: &http.Client{Transport: options.TokenTransport, Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		issuer: options.Config.IssuerURL(), callbackPath: options.Config.CallbackPath(),
		loginCodec:   securecookie.New(hash, block).MaxAge(int(loginLifetime.Seconds())).SetSerializer(securecookie.JSONEncoder{}),
		sessionCodec: securecookie.New(hash, block).MaxAge(int(sessionLifetime.Seconds())).SetSerializer(securecookie.JSONEncoder{}),
	}
	service.api = service.newAPIHandler()
	return service, nil
}

func (s *Service) ShardCount() uint32 { return s.router.ShardCount() }

// Resolve authenticates callback routing data without exchanging the code.
// State never supplies a URL or hostname; destinations come from shard DNS.
func (s *Service) Resolve(r *http.Request) (shard.Route, error) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return s.resolveAPI(r)
	}
	if r.URL.Path == s.callbackPath {
		state, err := s.decodeState(r)
		if err != nil {
			return shard.Route{}, err
		}
		return shard.Route{Owner: state.Owner, Path: shard.RequestPath{TopLevel: state.Username}}, nil
	}
	if strings.Count(r.URL.Path, "/") == 2 && strings.HasSuffix(r.URL.Path, "/") && r.URL.RawPath == "" {
		username := strings.Trim(r.URL.Path, "/")
		owner, err := s.router.Owner(username)
		if err != nil || username == "auth" {
			return shard.Route{}, errors.New("invalid user space")
		}
		return shard.Route{Owner: owner, Path: shard.RequestPath{TopLevel: username}}, nil
	}
	route, err := s.router.Resolve(r)
	if err == nil && route.Path.TopLevel == "auth" {
		return shard.Route{}, errors.New("auth is reserved for authentication endpoints")
	}
	return route, err
}

func (s *Service) decodeState(r *http.Request) (loginState, error) {
	var state loginState
	if r.Method != http.MethodGet || r.URL.RawPath != "" {
		return state, errors.New("invalid callback request")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["state"]) != 1 {
		return state, errors.New("exactly one state is required")
	}
	if err := s.loginCodec.Decode(stateLabel, query.Get("state"), &state); err != nil {
		return state, errors.New("invalid callback state")
	}
	owner, err := s.router.Owner(state.Username)
	id, idErr := base64.RawURLEncoding.DecodeString(state.ID)
	if err != nil || state.Username == "auth" || state.Owner != owner || state.Origin != s.origin ||
		state.Expires <= time.Now().Unix() || idErr != nil || len(id) != 32 ||
		identityIssuer(Identity{Issuer: state.Issuer}) != s.issuer {
		return state, errors.New("invalid callback state")
	}
	if (state.Mode != "" && state.Mode != "login" && state.Mode != "register") || !s.validReturnTo(state.ReturnTo) {
		return state, errors.New("invalid callback destination")
	}
	return state, nil
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	route, err := s.Resolve(r)
	if err != nil {
		http.Error(w, "invalid authentication request", http.StatusBadRequest)
		return
	}
	if route.Owner != s.local {
		http.Error(w, "authentication routing mismatch", http.StatusBadGateway)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.api.ServeHTTP(w, r)
		return
	}
	if r.URL.Path == s.callbackPath {
		s.callback(w, r)
		return
	}
	if isGitRequest(r) {
		s.serveGit(w, r, route.Path.TopLevel)
		return
	}
	username := route.Path.TopLevel
	loginPath := "/" + username + "/auth/oidc/login"
	legacyLogin := s.issuer == config.GoogleIssuer && r.URL.Path == "/"+username+"/auth/google/login"
	if r.URL.Path == loginPath || legacyLogin {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		s.login(w, r, username)
		return
	}
	current, err := s.readSession(r)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		if r.Header.Get("Origin") != s.origin || !equal(r.Header.Get("X-CSRF-Token"), current.CSRF) {
			http.Error(w, "invalid CSRF token or origin", http.StatusForbidden)
			return
		}
	}
	root := r.URL.Path == "/"+username || r.URL.Path == "/"+username+"/"
	if root && r.Method == http.MethodPost {
		s.serveCreateGroup(w, r, username, current)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	record, _, err := s.loadNamespace(ctx, username)
	cancel()
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "forbidden", http.StatusForbidden)
		} else {
			serverError(w)
		}
		return
	}
	if record.Type == groupNamespace {
		s.serveGroup(w, r, username, current, record)
		return
	}
	if current.Username != username || userID(record.Identity) != userID(current.Identity) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/"+username+"/auth/logout" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		setCookie(w, sessionCookie, "", -1)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/"+username || r.URL.Path == "/"+username+"/" || r.URL.Path == "/"+username+"/auth/session" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Username string   `json:"username"`
			Identity Identity `json:"identity"`
			CSRF     string   `json:"csrfToken"`
			UserID   string   `json:"userId"`
		}{current.Username, current.Identity, current.CSRF, userID(current.Identity)})
		return
	}
	subject := authz.Subject{UserID: userID(current.Identity), Authenticated: true}
	s.next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), subjectKey{}, subject)))
}

func (s *Service) login(w http.ResponseWriter, r *http.Request, username string) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["mode"]) > 1 || len(query["ui"]) > 1 || len(query["returnTo"]) > 1 {
		http.Error(w, "invalid login parameters", http.StatusBadRequest)
		return
	}
	state := loginState{Username: username, Mode: query.Get("mode"), UI: query.Get("ui") == "1", ReturnTo: query.Get("returnTo")}
	if (state.Mode != "" && state.Mode != "login" && state.Mode != "register") || !s.validReturnTo(state.ReturnTo) {
		http.Error(w, "invalid login parameters", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	record, _, err := s.loadNamespace(ctx, username)
	if err == nil && record.Type != userNamespace {
		s.loginError(w, r, state, "namespace belongs to a group; log in through your user space", http.StatusConflict)
		return
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		serverError(w)
		return
	}
	if state.Mode == "register" && err == nil {
		s.loginError(w, r, state, "username is already claimed", http.StatusConflict)
		return
	}
	if state.Mode == "login" && errors.Is(err, storage.ErrNotFound) {
		s.loginError(w, r, state, "username is not registered", http.StatusNotFound)
		return
	}
	id, err := randomToken()
	if err != nil {
		serverError(w)
		return
	}
	browser, err := randomToken()
	if err != nil {
		serverError(w)
		return
	}
	nonce, err := randomToken()
	if err != nil {
		serverError(w)
		return
	}
	verifier, err := randomToken()
	if err != nil {
		serverError(w)
		return
	}
	state.Owner, state.ID, state.Origin, state.Expires = s.local, id, s.origin, time.Now().Add(loginLifetime).Unix()
	if s.issuer != config.GoogleIssuer {
		state.Issuer = s.issuer
	}
	tx := transaction{State: state, BrowserHash: sha256.Sum256([]byte(browser)), Nonce: nonce, Verifier: verifier}
	encodedTx, err := s.loginCodec.Encode(transactionLabel, tx)
	if err != nil {
		serverError(w)
		return
	}
	encodedState, err := s.loginCodec.Encode(stateLabel, state)
	if err != nil {
		serverError(w)
		return
	}
	_, err = s.store.Put(ctx, "auth/transactions/"+id, strings.NewReader(encodedTx), int64(len(encodedTx)), storage.PutOptions{IfNoneMatch: true})
	if err != nil {
		serverError(w)
		return
	}
	setCookie(w, loginCookie, browser, int(loginLifetime.Seconds()))
	// Request data is encoded only in query parameters.
	// #nosec G710 -- AuthorizationURL uses the configured OIDC provider endpoint.
	http.Redirect(w, r, s.provider.AuthorizationURL(encodedState, nonce, verifier), http.StatusFound)
}

func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	state, err := s.decodeState(r)
	if err != nil {
		http.Error(w, "invalid callback state", http.StatusBadRequest)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["code"]) > 1 || len(query["error"]) > 1 ||
		(query.Get("code") == "" && query.Get("error") == "") ||
		(query.Get("code") != "" && query.Get("error") != "") {
		http.Error(w, "invalid callback parameters", http.StatusBadRequest)
		return
	}
	browser, err := uniqueCookie(r, loginCookie)
	if err != nil {
		http.Error(w, "login browser cookie required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	key := "auth/transactions/" + state.ID
	data, version, err := s.readObject(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "login expired or already used", http.StatusBadRequest)
		} else {
			serverError(w)
		}
		return
	}
	var tx transaction
	browserHash := sha256.Sum256([]byte(browser))
	if s.loginCodec.Decode(transactionLabel, string(data), &tx) != nil || tx.State != state ||
		subtle.ConstantTimeCompare(browserHash[:], tx.BrowserHash[:]) != 1 {
		http.Error(w, "invalid login transaction", http.StatusBadRequest)
		return
	}
	// Consume atomically before exchanging the code so parallel callbacks and
	// retries cannot create multiple sessions from one login transaction.
	if _, err := s.store.Put(ctx, key, strings.NewReader("used"), 4, storage.PutOptions{IfMatch: version}); err != nil {
		http.Error(w, "login expired or already used", http.StatusBadRequest)
		return
	}
	setCookie(w, loginCookie, "", -1)
	if query.Get("error") != "" {
		s.loginError(w, r, state, "login declined", http.StatusUnauthorized)
		return
	}
	identity, err := s.provider.Exchange(ctx, query.Get("code"), tx.Verifier, tx.Nonce)
	if err != nil || !validIdentity(identity) || identityIssuer(identity) != s.issuer {
		s.loginError(w, r, state, "authentication failed", http.StatusUnauthorized)
		return
	}
	if err := s.completeLogin(ctx, state, identity); err != nil {
		if errors.Is(err, errUsernameTaken) {
			s.loginError(w, r, state, "username is already claimed or belongs to another account", http.StatusConflict)
		} else {
			serverError(w)
		}
		return
	}
	csrf, err := randomToken()
	if err != nil {
		serverError(w)
		return
	}
	current := session{Username: state.Username, Identity: identity, Origin: s.origin, CSRF: csrf, Expires: time.Now().Add(sessionLifetime).Unix()}
	encoded, err := s.sessionCodec.Encode(sessionCookie, current)
	if err != nil {
		serverError(w)
		return
	}
	setCookie(w, sessionCookie, encoded, int(sessionLifetime.Seconds()))
	destination := state.ReturnTo
	if destination == "" {
		destination = "/" + state.Username
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

// Only canonical UI routes may be preserved across the OIDC round trip.
// Repository navigation permits bounded, allow-listed query parameters, never
// hosts, encoded route paths, fragments, or protocol endpoints.
func (s *Service) validReturnTo(target string) bool {
	if target == "" {
		return true
	}
	u, err := url.Parse(target)
	if err != nil || len(target) > maxReturnToBytes || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" ||
		u.Fragment != "" || u.RawPath != "" || u.EscapedPath() != u.Path ||
		!strings.HasPrefix(u.Path, "/") || strings.Contains(u.Path, "//") || strings.ContainsAny(target, "\\#") ||
		strings.ContainsFunc(target, unicode.IsControl) {
		return false
	}
	serialized, err := json.Marshal(target)
	if err != nil || len(serialized) > maxReturnToBytes {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for _, values := range query {
		if len(values) != 1 || strings.ContainsFunc(values[0], unicode.IsControl) || strings.Contains(values[0], "\\") {
			return false
		}
	}
	if u.Path == "/auth/new-repository" {
		for key, values := range query {
			if key != "namespace" || values[0] == "auth" {
				return false
			}
			if _, err := s.router.Owner(values[0]); err != nil {
				return false
			}
		}
		return true
	}
	if u.Path == "/" || u.Path == "/auth/new-group" || u.Path == "/auth/tokens" {
		return len(query) == 0
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), "/")
	if parts[0] == "auth" {
		return false
	}
	if _, err := s.router.Owner(parts[0]); err != nil {
		return false
	}
	if len(parts) == 2 && repository.ValidName(parts[1]) {
		for key, values := range query {
			switch key {
			case "ref":
				if len(values[0]) > 256 {
					return false
				}
			case "path":
				if len(values[0]) > 4096 {
					return false
				}
			case "view":
				if values[0] != "commits" {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	return len(query) == 0 && (len(parts) == 1 ||
		(len(parts) == 2 && parts[1] == "settings") ||
		(len(parts) == 3 && parts[1] == "invitations" && parts[2] == "accept"))
}

func (s *Service) loginError(w http.ResponseWriter, r *http.Request, state loginState, message string, status int) {
	if !state.UI {
		http.Error(w, message, status)
		return
	}
	path := "/auth/login"
	if state.Mode == "register" {
		path = "/auth/register"
	}
	query := url.Values{"error": {message}, "username": {state.Username}}
	if state.ReturnTo != "" {
		query.Set("returnTo", state.ReturnTo)
	}
	http.Redirect(w, r, path+"?"+query.Encode(), http.StatusSeeOther)
}

func (s *Service) completeLogin(ctx context.Context, state loginState, identity Identity) error {
	switch state.Mode {
	case "register":
		record := namespaceRecord{SchemaVersion: 1, Type: userNamespace, Identity: identity}
		if err := s.writeNamespace(ctx, state.Username, record, ""); err != nil {
			if isNamespaceConflict(err) {
				return errUsernameTaken
			}
			return err
		}
		return nil
	case "login":
		record, _, err := s.loadNamespace(ctx, state.Username)
		if err != nil {
			return err
		}
		if record.Type != userNamespace || userID(record.Identity) != userID(identity) {
			return errUsernameTaken
		}
		return nil
	default:
		// Preserve the original first-login registration API for existing clients.
		return s.bindUser(ctx, state.Username, identity)
	}
}

func (s *Service) readSession(r *http.Request) (session, error) {
	var current session
	value, err := uniqueCookie(r, sessionCookie)
	if err != nil {
		return current, err
	}
	if err := s.sessionCodec.Decode(sessionCookie, value, &current); err != nil {
		return current, err
	}
	if current.Expires <= time.Now().Unix() || current.Origin != s.origin || !validIdentity(current.Identity) ||
		identityIssuer(current.Identity) != s.issuer || current.CSRF == "" {
		return current, errors.New("invalid session")
	}
	if _, err := s.router.Owner(current.Username); err != nil {
		return current, err
	}
	return current, nil
}

func randomToken() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func uniqueCookie(r *http.Request, name string) (string, error) {
	var value string
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			value = cookie.Value
			count++
		}
	}
	if count != 1 || value == "" {
		return "", errors.New("exactly one cookie required")
	}
	return value, nil
}

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	expires := time.Now().Add(time.Duration(maxAge) * time.Second)
	if maxAge < 0 {
		expires = time.Unix(1, 0)
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge, Expires: expires})
}

func equal(a, b string) bool { return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func serverError(w http.ResponseWriter) {
	http.Error(w, "authentication service unavailable", http.StatusServiceUnavailable)
}
func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
