package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"

	"github.com/define42/GitOneS3/internal/storage"
)

const (
	maxSSHPublicKeyBytes = 4096
	maxSSHKeyRecords     = 100
	maxSSHKeyringBytes   = 512 << 10
)

var (
	errInvalidSSHKey = errors.New("invalid SSH public key")
	errSSHKeyExists  = errors.New("SSH public key was already registered")
	errSSHKeyLimit   = errors.New("SSH key record limit reached")
)

// SSHPrincipal identifies a registered user after SSH private-key possession
// and the authoritative key registration have both been verified by the caller.
type SSHPrincipal struct {
	Username string   `json:"username"`
	Identity Identity `json:"identity"`
}

type sshKeyView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	PublicKey   string     `json:"publicKey"`
	Fingerprint string     `json:"fingerprint"`
	CreatedAt   time.Time  `json:"createdAt"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

// A single CAS-updated object makes the record limit and duplicate detection
// atomic. Revoked keys remain tombstones and cannot be registered again.
type sshKeyring struct {
	SchemaVersion int          `json:"schemaVersion"`
	Username      string       `json:"username"`
	Identity      Identity     `json:"identity"`
	Keys          []sshKeyView `json:"keys"`
}

func sshKeyringKey(username string) string { return "auth/ssh-keys/" + username + ".json" }

func sshKeyID(key ssh.PublicKey) string {
	digest := sha256.Sum256(key.Marshal())
	return hex.EncodeToString(digest[:])
}

func validSSHKeyID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == id
}

func parseSSHPublicKey(input string) (ssh.PublicKey, error) {
	if len(input) > maxSSHPublicKeyBytes {
		return nil, errInvalidSSHKey
	}
	line := strings.TrimSpace(input)
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return nil, errInvalidSSHKey
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errInvalidSSHKey
	}
	if !supportedSSHPublicKey(key) {
		return nil, errInvalidSSHKey
	}
	return key, nil
}

func supportedSSHPublicKey(key ssh.PublicKey) bool {
	// Certificates deliberately do not implement CryptoPublicKey; approving a
	// certificate authority would have different revocation and identity rules.
	public, ok := key.(ssh.CryptoPublicKey)
	if !ok {
		return false
	}
	switch value := public.CryptoPublicKey().(type) {
	case ed25519.PublicKey:
		return key.Type() == ssh.KeyAlgoED25519 && len(value) == ed25519.PublicKeySize
	case *rsa.PublicKey:
		return value.N != nil && value.N.BitLen() >= 3072 && value.N.BitLen() <= 8192
	case *ecdsa.PublicKey:
		return key.Type() == ssh.KeyAlgoECDSA256 || key.Type() == ssh.KeyAlgoECDSA384 || key.Type() == ssh.KeyAlgoECDSA521
	default:
		return false
	}
}

func validSSHKeyName(name string) bool {
	return name != "" && name == strings.TrimSpace(name) && len(name) <= 100 &&
		!strings.ContainsFunc(name, unicode.IsControl)
}

func validSSHKeyView(view sshKeyView) bool {
	if !validSSHKeyName(view.Name) || view.CreatedAt.IsZero() {
		return false
	}
	if view.RevokedAt != nil && view.RevokedAt.Before(view.CreatedAt) {
		return false
	}
	key, err := parseSSHPublicKey(view.PublicKey)
	return err == nil && view.ID == sshKeyID(key) && view.Fingerprint == ssh.FingerprintSHA256(key) &&
		view.PublicKey == strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func (s *Service) loadSSHKeyring(ctx context.Context, username string) (sshKeyring, storage.Version, error) {
	if err := s.validateNamespaceOwner(username); err != nil {
		return sshKeyring{}, "", fmt.Errorf("load SSH keyring: %w", err)
	}
	body, info, err := s.store.Get(ctx, sshKeyringKey(username))
	if err != nil {
		return sshKeyring{}, "", fmt.Errorf("load SSH keyring: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(body, maxSSHKeyringBytes+1))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return sshKeyring{}, "", fmt.Errorf("read SSH keyring: %w", err)
	}
	var record sshKeyring
	invalidEncoding := len(data) > maxSSHKeyringBytes || json.Unmarshal(data, &record) != nil
	if invalidEncoding || record.SchemaVersion != 1 || record.Username != username {
		return sshKeyring{}, "", errors.New("invalid stored SSH keyring")
	}
	invalidRecord := !validIdentity(record.Identity) || len(record.Keys) > maxSSHKeyRecords || info.Version == ""
	if invalidRecord || record.Keys == nil {
		return sshKeyring{}, "", errors.New("invalid stored SSH keyring")
	}
	seen := make(map[string]bool, len(record.Keys))
	for _, key := range record.Keys {
		if seen[key.ID] || !validSSHKeyView(key) {
			return sshKeyring{}, "", errors.New("invalid stored SSH public key")
		}
		seen[key.ID] = true
	}
	return record, info.Version, nil
}

func (s *Service) writeSSHKeyring(ctx context.Context, record sshKeyring, version storage.Version) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode SSH keyring: %w", err)
	}
	if len(data) > maxSSHKeyringBytes || len(record.Keys) > maxSSHKeyRecords {
		return errSSHKeyLimit
	}
	_, err = s.store.Put(
		ctx,
		sshKeyringKey(record.Username),
		bytes.NewReader(data),
		int64(len(data)),
		storage.PutOptions{IfMatch: version, IfNoneMatch: version == ""},
	)
	if err != nil {
		return fmt.Errorf("write SSH keyring: %w", err)
	}
	return nil
}

func (s *Service) createSSHKey(ctx context.Context, principal SSHPrincipal, name, publicKey string) (sshKeyView, error) {
	key, err := parseSSHPublicKey(publicKey)
	name = strings.TrimSpace(name)
	if err != nil || !validSSHKeyName(name) {
		return sshKeyView{}, errInvalidSSHKey
	}
	if err := s.checkSSHUser(ctx, principal); err != nil {
		return sshKeyView{}, err
	}
	view := sshKeyView{ID: sshKeyID(key), Name: name,
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
		Fingerprint: ssh.FingerprintSHA256(key), CreatedAt: time.Now().UTC()}
	for range 8 {
		record, version, err := s.loadSSHKeyring(ctx, principal.Username)
		if errors.Is(err, storage.ErrNotFound) {
			record = sshKeyring{SchemaVersion: 1, Username: principal.Username,
				Identity: Identity{Issuer: identityIssuer(principal.Identity), Subject: principal.Identity.Subject},
				Keys:     []sshKeyView{}}
		} else if err != nil {
			return sshKeyView{}, err
		}
		if userID(record.Identity) != userID(principal.Identity) {
			return sshKeyView{}, errInvalidSSHKey
		}
		for _, existing := range record.Keys {
			if existing.ID == view.ID {
				return sshKeyView{}, errSSHKeyExists
			}
		}
		if len(record.Keys) == maxSSHKeyRecords {
			return sshKeyView{}, errSSHKeyLimit
		}
		record.Keys = append(record.Keys, view)
		if err := s.writeSSHKeyring(ctx, record, version); isNamespaceConflict(err) {
			continue
		} else if err != nil {
			return sshKeyView{}, err
		}
		return view, nil
	}
	return sshKeyView{}, storage.ErrConditionalConflict
}

func (s *Service) listSSHKeys(ctx context.Context, username string) ([]sshKeyView, error) {
	record, _, err := s.loadSSHKeyring(ctx, username)
	if errors.Is(err, storage.ErrNotFound) {
		return []sshKeyView{}, nil
	}
	if err != nil {
		return nil, err
	}
	return record.Keys, nil
}

func (s *Service) revokeSSHKey(ctx context.Context, username, id string) error {
	if !validSSHKeyID(id) {
		return errInvalidSSHKey
	}
	for range 8 {
		record, version, err := s.loadSSHKeyring(ctx, username)
		if err != nil {
			return err
		}
		index := -1
		for i, key := range record.Keys {
			if key.ID == id {
				index = i
				break
			}
		}
		if index == -1 {
			return storage.ErrNotFound
		}
		if record.Keys[index].RevokedAt != nil {
			return nil
		}
		now := time.Now().UTC()
		record.Keys[index].RevokedAt = &now
		if err := s.writeSSHKeyring(ctx, record, version); isNamespaceConflict(err) {
			continue
		} else if err != nil {
			return err
		}
		return nil
	}
	return storage.ErrConditionalConflict
}

func (s *Service) checkSSHUser(ctx context.Context, principal SSHPrincipal) error {
	if !validIdentity(principal.Identity) || identityIssuer(principal.Identity) != s.issuer {
		return errInvalidSSHKey
	}
	namespace, _, err := s.loadNamespace(ctx, principal.Username)
	if errors.Is(err, storage.ErrNotFound) {
		return errInvalidSSHKey
	}
	if err != nil {
		return fmt.Errorf("load SSH user: %w", err)
	}
	if namespace.Type != userNamespace || userID(namespace.Identity) != userID(principal.Identity) {
		return errInvalidSSHKey
	}
	return nil
}

// VerifySSHKey checks a wire-format public key against current, shard-local
// registration. It does not verify private-key possession; the SSH handshake
// must succeed separately. Positive results must not be cached between commands.
func (s *Service) VerifySSHKey(ctx context.Context, username string, publicKey []byte) (SSHPrincipal, error) {
	if len(publicKey) == 0 || len(publicKey) > maxSSHPublicKeyBytes {
		return SSHPrincipal{}, errInvalidSSHKey
	}
	key, err := ssh.ParsePublicKey(publicKey)
	if err != nil || !supportedSSHPublicKey(key) {
		return SSHPrincipal{}, errInvalidSSHKey
	}
	record, _, err := s.loadSSHKeyring(ctx, username)
	if errors.Is(err, storage.ErrNotFound) {
		return SSHPrincipal{}, errInvalidSSHKey
	}
	if err != nil {
		return SSHPrincipal{}, err
	}
	principal := SSHPrincipal{Username: username, Identity: record.Identity}
	if err := s.checkSSHUser(ctx, principal); err != nil {
		return SSHPrincipal{}, err
	}
	id := sshKeyID(key)
	for _, registered := range record.Keys {
		if registered.ID == id && registered.RevokedAt == nil {
			return principal, nil
		}
	}
	return SSHPrincipal{}, errInvalidSSHKey
}

// AuthorizeSSH checks current personal/group access on the repository shard.
// The caller must freshly verify the key on its user shard before every call,
// including the authorization check immediately before publishing a push.
func (s *Service) AuthorizeSSH(ctx context.Context, principal SSHPrincipal, namespace string, write bool) error {
	if _, err := s.router.Owner(principal.Username); err != nil || principal.Username == "auth" {
		return errInvalidSSHKey
	}
	if !validIdentity(principal.Identity) || identityIssuer(principal.Identity) != s.issuer {
		return errInvalidSSHKey
	}
	current := apiSession{current: session{Username: principal.Username, Identity: principal.Identity}, isAuthenticated: true}
	ctx = context.WithValue(ctx, apiSessionKey{}, current)
	if _, err := s.repositoryRole(ctx, namespace, write); err != nil {
		return fmt.Errorf("authorize SSH repository: %w", err)
	}
	return nil
}
