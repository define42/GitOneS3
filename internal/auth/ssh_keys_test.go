package auth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/define42/GitOneS3/internal/storage"
)

func testSSHPublicKey(t *testing.T, seed byte) ssh.PublicKey {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	public, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	return public
}

func sshAuthorizedKey(key ssh.PublicKey) string { return string(ssh.MarshalAuthorizedKey(key)) }

func TestParseSSHPublicKey(t *testing.T) {
	t.Parallel()
	public := testSSHPublicKey(t, 1)
	line := strings.TrimSpace(sshAuthorizedKey(public))
	certificate := &ssh.Certificate{Key: public, CertType: ssh.UserCert, SignatureKey: public,
		Signature: &ssh.Signature{Format: public.Type(), Blob: make([]byte, 64)}}
	dsaWire := ssh.Marshal(struct {
		Name       string
		P, Q, G, Y *big.Int
	}{Name: "ssh-dss", P: new(big.Int).Lsh(big.NewInt(1), 1023), Q: new(big.Int).Lsh(big.NewInt(1), 159),
		G: big.NewInt(2), Y: big.NewInt(3)})
	if _, err := ssh.ParsePublicKey(dsaWire); err != nil {
		t.Fatalf("invalid DSA rejection fixture: %v", err)
	}
	for _, test := range []struct {
		name, input string
		valid       bool
	}{
		{name: "ed25519", input: line, valid: true},
		{name: "comment and trailing newline", input: line + " alice@laptop\n", valid: true},
		{name: "empty", input: ""},
		{name: "private key", input: "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----"},
		{name: "options", input: `command="git-shell" ` + line},
		{name: "multiple keys", input: line + "\n" + line},
		{name: "comment then key", input: "# comment\n" + line},
		{name: "certificate", input: sshAuthorizedKey(certificate)},
		{name: "DSA", input: "ssh-dss " + base64.StdEncoding.EncodeToString(dsaWire)},
		{name: "oversized", input: line + " " + strings.Repeat("x", maxSSHPublicKeyBytes)},
		{name: "invalid base64", input: "ssh-ed25519 not-base64"},
		{name: "mismatched type", input: strings.Replace(line, "ssh-ed25519", "ssh-rsa", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSSHPublicKey(test.input)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
	for _, bits := range []uint{2048, 3072, 4096, 8192, 8193} {
		t.Run(fmt.Sprintf("RSA %d", bits), func(t *testing.T) {
			t.Parallel()
			// Only public-key size validation is under test, not RSA signatures.
			n := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bits), big.NewInt(1))
			key, err := ssh.NewPublicKey(&rsa.PublicKey{N: n, E: 65537})
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseSSHPublicKey(sshAuthorizedKey(key))
			valid := bits >= 3072 && bits <= 8192
			if (err == nil) != valid {
				t.Fatalf("valid=%v, error=%v", valid, err)
			}
		})
	}
	for _, curve := range []elliptic.Curve{elliptic.P256(), elliptic.P384(), elliptic.P521()} {
		t.Run(curve.Params().Name, func(t *testing.T) {
			t.Parallel()
			private, err := ecdsa.GenerateKey(curve, rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			key, err := ssh.NewPublicKey(private.Public())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseSSHPublicKey(sshAuthorizedKey(key)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSSHKeyLifecycle(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	s := testService(t, 1, store, &fakeProvider{}, nil)
	identity := Identity{Subject: "alice-id", Email: "alice@example.com"}
	principal := SSHPrincipal{Username: "alice", Identity: identity}
	if err := s.bindUser(t.Context(), "alice", identity); err != nil {
		t.Fatal(err)
	}
	keys, err := s.listSSHKeys(t.Context(), "alice")
	if err != nil || keys == nil || len(keys) != 0 {
		t.Fatalf("empty list: %+v %v", keys, err)
	}
	public := testSSHPublicKey(t, 1)
	view, err := s.createSSHKey(t.Context(), principal, " Laptop ", sshAuthorizedKey(public)+" ")
	if err != nil {
		t.Fatal(err)
	}
	if view.Name != "Laptop" || view.Fingerprint != ssh.FingerprintSHA256(public) || !validSSHKeyID(view.ID) {
		t.Fatalf("invalid view: %+v", view)
	}
	if _, err := s.createSSHKey(t.Context(), principal, "Duplicate", sshAuthorizedKey(public)); !errors.Is(err, errSSHKeyExists) {
		t.Fatalf("duplicate accepted: %v", err)
	}
	restarted := testService(t, 1, store, &fakeProvider{}, nil)
	verified, err := restarted.VerifySSHKey(t.Context(), "alice", public.Marshal())
	if err != nil || verified.Username != "alice" || userID(verified.Identity) != userID(identity) {
		t.Fatalf("verify: %+v %v", verified, err)
	}
	if verified.Identity.Email != "" {
		t.Fatal("unnecessary identity email persisted")
	}
	if err := restarted.AuthorizeSSH(t.Context(), verified, "alice", true); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := restarted.revokeSSHKey(t.Context(), "alice", view.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.VerifySSHKey(t.Context(), "alice", public.Marshal()); !errors.Is(err, errInvalidSSHKey) {
		t.Fatalf("revocation not observed by old service: %v", err)
	}
	if _, err := s.createSSHKey(t.Context(), principal, "Reuse", sshAuthorizedKey(public)); !errors.Is(err, errSSHKeyExists) {
		t.Fatalf("revoked key reused: %v", err)
	}
	keys, err = s.listSSHKeys(t.Context(), "alice")
	if err != nil || len(keys) != 1 || keys[0].RevokedAt == nil {
		t.Fatalf("revoked key missing from list: %+v %v", keys, err)
	}
}

func TestSSHKeyVerificationFailsClosed(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"unknown key", "wrong username", "wrong shard", "changed identity", "changed issuer",
		"missing user", "corrupt record", "oversized record", "key metadata mismatch", "wire key too large"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := storage.NewMemoryStore()
			s := testService(t, 1, store, &fakeProvider{}, nil)
			identity := Identity{Subject: "alice-id"}
			if err := s.bindUser(t.Context(), "alice", identity); err != nil {
				t.Fatal(err)
			}
			public := testSSHPublicKey(t, 1)
			if _, err := s.createSSHKey(t.Context(), SSHPrincipal{Username: "alice", Identity: identity},
				"Laptop", sshAuthorizedKey(public)); err != nil {
				t.Fatal(err)
			}
			record, version, err := s.loadSSHKeyring(t.Context(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			username, wire := "alice", public.Marshal()
			switch name {
			case "unknown key":
				wire = testSSHPublicKey(t, 2).Marshal()
			case "wrong username":
				username = "bob"
			case "wrong shard":
				s = testService(t, 0, store, &fakeProvider{}, nil)
			case "changed identity":
				record.Identity.Subject = "other"
			case "changed issuer":
				record.Identity.Issuer = "https://other.example"
			case "missing user":
				if err := store.Delete(t.Context(), namespaceKey("alice"), ""); err != nil {
					t.Fatal(err)
				}
			case "key metadata mismatch":
				record.Keys[0].Fingerprint = "SHA256:invalid"
			case "wire key too large":
				wire = make([]byte, maxSSHPublicKeyBytes+1)
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if name == "corrupt record" {
				data = []byte(`{"schemaVersion":999}`)
			}
			if name == "oversized record" {
				data = bytes.Repeat([]byte{' '}, maxSSHKeyringBytes+1)
			}
			if _, err := store.Put(t.Context(), sshKeyringKey("alice"), bytes.NewReader(data), int64(len(data)),
				storage.PutOptions{IfMatch: version}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.VerifySSHKey(t.Context(), username, wire); err == nil {
				t.Fatal("invalid key identity accepted")
			}
		})
	}
}

func TestSSHKeyConcurrentCreationAndLimit(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	principal := SSHPrincipal{Username: "alice", Identity: Identity{Subject: "alice-id"}}
	if err := s.bindUser(t.Context(), "alice", principal.Identity); err != nil {
		t.Fatal(err)
	}
	public := testSSHPublicKey(t, 1)
	results := make(chan error, 8)
	var workers sync.WaitGroup
	for range cap(results) {
		workers.Go(func() {
			_, err := s.createSSHKey(t.Context(), principal, "Laptop", sshAuthorizedKey(public))
			results <- err
		})
	}
	workers.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
			continue
		}
		if !errors.Is(err, errSSHKeyExists) {
			t.Fatal(err)
		}
	}
	if created != 1 {
		t.Fatalf("created %d copies of the same key", created)
	}
	record, version, err := s.loadSSHKeyring(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	for seed := byte(2); seed <= maxSSHKeyRecords; seed++ {
		key := testSSHPublicKey(t, seed)
		now := time.Now().UTC()
		record.Keys = append(record.Keys, sshKeyView{ID: sshKeyID(key), Name: "Previous key",
			PublicKey: strings.TrimSpace(sshAuthorizedKey(key)), Fingerprint: ssh.FingerprintSHA256(key),
			CreatedAt: now, RevokedAt: &now})
	}
	if err := s.writeSSHKeyring(t.Context(), record, version); err != nil {
		t.Fatal(err)
	}
	_, err = s.createSSHKey(t.Context(), principal, "New key", sshAuthorizedKey(testSSHPublicKey(t, 101)))
	if !errors.Is(err, errSSHKeyLimit) {
		t.Fatalf("record limit not enforced: %v", err)
	}
}

func TestSSHAuthorizeCurrentGroupRole(t *testing.T) {
	t.Parallel()
	principal := SSHPrincipal{Username: "alice", Identity: Identity{Subject: "alice-id"}}
	for _, step := range []struct {
		name, initialRole, action, role string
		invited, read, write            bool
	}{
		{name: "outsider"},
		{name: "invited", initialRole: "reader", invited: true},
		{name: "reader", initialRole: "reader", read: true},
		{name: "developer", initialRole: "developer", read: true, write: true},
		{name: "demoted", initialRole: "developer", action: "set-role", role: "reader", read: true},
		{name: "removed", initialRole: "developer", action: "remove"},
	} {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
			record := namespaceRecord{SchemaVersion: 1, Type: groupNamespace, CreatorUserID: "google:owner-id",
				Members: map[string]string{"google:owner-id": "owner"}, Invitations: map[string]string{}}
			if step.initialRole != "" {
				if step.invited {
					record.Invitations[userID(principal.Identity)] = step.initialRole
				} else {
					record.Members[userID(principal.Identity)] = step.initialRole
				}
			}
			if err := s.writeNamespace(t.Context(), "acme", record, ""); err != nil {
				t.Fatal(err)
			}
			if step.action != "" {
				// Observe the original permission before changing its record, then
				// require the next authorization to see the current stored role.
				if err := s.AuthorizeSSH(t.Context(), principal, "acme", true); err != nil {
					t.Fatal(err)
				}
				if _, err := s.updateGroup(t.Context(), "acme", "google:owner-id", step.action,
					userID(principal.Identity), step.role); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.AuthorizeSSH(t.Context(), principal, "acme", false); (err == nil) != step.read {
				t.Fatalf("read allowed=%v: %v", step.read, err)
			}
			if err := s.AuthorizeSSH(t.Context(), principal, "acme", true); (err == nil) != step.write {
				t.Fatalf("write allowed=%v: %v", step.write, err)
			}
		})
	}
}

func TestSSHKeyStorageContainsOnlyPublicMaterial(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	s := testService(t, 1, store, &fakeProvider{}, nil)
	principal := SSHPrincipal{Username: "alice", Identity: Identity{Subject: "alice-id", Email: "private@example.com"}}
	if err := s.bindUser(t.Context(), "alice", principal.Identity); err != nil {
		t.Fatal(err)
	}
	key := testSSHPublicKey(t, 1)
	if _, err := s.createSSHKey(t.Context(), principal, "Laptop",
		strings.TrimSpace(sshAuthorizedKey(key))+" private-comment"); err != nil {
		t.Fatal(err)
	}
	body, _, err := store.Get(t.Context(), sshKeyringKey("alice"))
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(principal.Identity.Email)) || bytes.Contains(data, []byte("private-comment")) {
		t.Fatal("unnecessary private metadata persisted")
	}
	if !bytes.Contains(data, []byte(strings.TrimSpace(sshAuthorizedKey(key)))) {
		t.Fatal("public key missing from persistence")
	}
}
