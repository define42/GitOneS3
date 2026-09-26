package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/define42/GitOneS3/internal/repository"
)

const (
	lfsGrantLabel       = "gitone-lfs-grant-v1"
	lfsGrantPrefix      = "gitone-lfs."
	lfsGrantLifetime    = 15 * time.Minute
	lfsKeyCheckLabel    = "gitone-lfs-key-check-v1"
	lfsKeyCheckPrefix   = "gitone-lfs-check."
	lfsKeyCheckLifetime = 30 * time.Second
	maxLFSBatchBytes    = 1 << 20
)

// LFSCredentials is Git LFS's SSH authentication response. Its URL always points
// to GitOne; object storage addresses and credentials are never exposed.
type LFSCredentials struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header"`
	ExpiresIn int               `json:"expires_in"`
}

type lfsGrant struct {
	Principal  SSHPrincipal
	Key        []byte
	Namespace  string
	Repository string
	Operation  string
	Origin     string
	Expires    int64
}

// lfsWriteRequest classifies batch download as read despite its POST method.
// Decode the operation once with exact spelling and no duplicate top-level
// members so another JSON decoder cannot interpret a different permission.
func lfsWriteRequest(r *http.Request, path []string) (bool, error) {
	if len(path) == 2 && path[0] == "objects" && path[1] == "batch" && r.Method == http.MethodPost {
		if r.Body == nil {
			return false, huma.Error400BadRequest("invalid LFS batch request")
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxLFSBatchBytes+1))
		_ = r.Body.Close() // The bounded control body has been consumed.
		if err != nil {
			return false, huma.Error400BadRequest("cannot read LFS batch request")
		}
		if len(data) > maxLFSBatchBytes {
			return false, huma.Error413RequestEntityTooLarge("LFS batch request too large")
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		decoder := json.NewDecoder(bytes.NewReader(data))
		token, err := decoder.Token()
		if err != nil || token != json.Delim('{') {
			return false, huma.Error400BadRequest("invalid LFS batch request")
		}
		seen := make(map[string]bool)
		operation := ""
		for decoder.More() {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] || (strings.EqualFold(key, "operation") && key != "operation") {
				return false, huma.Error400BadRequest("invalid LFS batch request")
			}
			seen[key] = true
			var value json.RawMessage
			if decoder.Decode(&value) != nil {
				return false, huma.Error400BadRequest("invalid LFS batch request")
			}
			if key == "operation" && json.Unmarshal(value, &operation) != nil {
				return false, huma.Error400BadRequest("invalid LFS batch operation")
			}
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
			return false, huma.Error400BadRequest("invalid LFS batch request")
		}
		if decoder.Decode(new(any)) != io.EOF || (operation != "upload" && operation != "download") {
			return false, huma.Error400BadRequest("invalid LFS batch operation")
		}
		return operation == "upload", nil
	}
	if len(path) == 2 && path[0] == "objects" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		return false, nil
	}
	return true, nil
}

// IssueLFSCredentials issues an encrypted, short-lived grant after the SSH
// server verifies private-key possession and current key registration. Every
// HTTP use rechecks registration on the user shard and current repository ACLs.
func (s *Service) IssueLFSCredentials(
	ctx context.Context,
	principal SSHPrincipal,
	key []byte,
	namespace, repo, operation string,
) (LFSCredentials, error) {
	if (operation != "upload" && operation != "download") || !repository.ValidName(repo) ||
		len(key) == 0 || len(key) > maxSSHPublicKeyBytes {
		return LFSCredentials{}, errors.New("invalid LFS authentication request")
	}
	owner, err := s.router.Owner(namespace)
	if err != nil || owner != s.local || namespace == "auth" {
		return LFSCredentials{}, errors.New("invalid LFS repository shard")
	}
	if err := s.AuthorizeSSH(ctx, principal, namespace, operation == "upload"); err != nil {
		return LFSCredentials{}, err
	}
	grant := lfsGrant{
		Principal: principal, Key: key, Namespace: namespace, Repository: repo,
		Operation: operation, Origin: s.origin, Expires: time.Now().Add(lfsGrantLifetime).Unix(),
	}
	raw, err := s.sessionCodec.Encode(lfsGrantLabel, grant)
	if err != nil {
		return LFSCredentials{}, errors.New("cannot issue LFS credential")
	}
	return LFSCredentials{
		Href:      s.origin + "/" + namespace + "/" + repo + ".git/info/lfs",
		Header:    map[string]string{"Authorization": "Bearer " + lfsGrantPrefix + raw},
		ExpiresIn: int(lfsGrantLifetime.Seconds()),
	}, nil
}

func (s *Service) readLFSGrant(r *http.Request) (lfsGrant, error) {
	return s.readLFSCredential(r, lfsGrantLabel, lfsGrantPrefix, lfsGrantLifetime)
}

func (s *Service) readLFSCredential(r *http.Request, label, prefix string, lifetime time.Duration) (lfsGrant, error) {
	var grant lfsGrant
	header := r.Header.Get("Authorization")
	if len(r.Header.Values("Authorization")) != 1 || len(header) > 4096+len(prefix)+7 ||
		!strings.HasPrefix(header, "Bearer "+prefix) {
		return grant, huma.Error401Unauthorized("invalid LFS credential")
	}
	raw := strings.TrimPrefix(header, "Bearer "+prefix)
	if s.sessionCodec.Decode(label, raw, &grant) != nil || grant.Origin != s.origin ||
		grant.Expires <= time.Now().Unix() || grant.Expires > time.Now().Add(lifetime).Unix()+1 ||
		(grant.Operation != "upload" && grant.Operation != "download") ||
		!repository.ValidName(grant.Repository) || !validIdentity(grant.Principal.Identity) ||
		identityIssuer(grant.Principal.Identity) != s.issuer ||
		len(grant.Key) == 0 || len(grant.Key) > maxSSHPublicKeyBytes {
		return lfsGrant{}, huma.Error401Unauthorized("invalid LFS credential")
	}
	for _, name := range []string{grant.Namespace, grant.Principal.Username} {
		if _, err := s.router.Owner(name); err != nil || name == "auth" {
			return lfsGrant{}, huma.Error401Unauthorized("invalid LFS credential")
		}
	}
	return grant, nil
}

func (s *Service) parseLFSRequestGrant(r *http.Request, namespace, repo string, write bool) (lfsGrant, error) {
	grant, err := s.readLFSGrant(r)
	if err != nil {
		return grant, err
	}
	if grant.Namespace != namespace || grant.Repository != repo || (grant.Operation == "upload") != write {
		return lfsGrant{}, huma.Error403Forbidden("LFS credential does not permit this repository operation")
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.origin {
		return lfsGrant{}, huma.Error403Forbidden("invalid origin")
	}
	return grant, nil
}

func (s *Service) verifyLFSGrant(ctx context.Context, grant lfsGrant) error {
	owner, _ := s.router.Owner(grant.Principal.Username)
	if owner == s.local {
		return s.verifyLFSGrantKey(ctx, grant)
	}
	return s.verifyRemoteLFSGrant(ctx, owner, grant)
}

func (s *Service) verifyLFSGrantKey(ctx context.Context, grant lfsGrant) error {
	principal, err := s.VerifySSHKey(ctx, grant.Principal.Username, grant.Key)
	if errors.Is(err, errInvalidSSHKey) {
		return huma.Error401Unauthorized("invalid LFS credential")
	}
	if err != nil {
		return huma.Error503ServiceUnavailable("SSH key authority unavailable")
	}
	if userID(principal.Identity) != userID(grant.Principal.Identity) {
		return huma.Error401Unauthorized("invalid LFS credential")
	}
	return nil
}

func (s *Service) serveVerifyLFSGrant(w http.ResponseWriter, r *http.Request, username string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	// This endpoint accepts only a fresh deployment-signed authority check.
	// Client grants cannot extend their own lifetime or bypass admission.
	grant, err := s.readLFSCredential(r, lfsKeyCheckLabel, lfsKeyCheckPrefix, lfsKeyCheckLifetime)
	if err != nil || grant.Principal.Username != username {
		gitAuthError(w, huma.Error401Unauthorized("invalid LFS credential"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.verifyLFSGrantKey(ctx, grant); err != nil {
		gitAuthError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
