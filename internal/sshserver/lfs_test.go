package sshserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/define42/GitOneS3/internal/auth"
	"github.com/define42/GitOneS3/internal/shard"
)

type unitLFSAuthority struct {
	*unitAuthority
	issued atomic.Int32
}

func (a *unitLFSAuthority) IssueLFSCredentials(
	ctx context.Context, principal auth.SSHPrincipal, key []byte, namespace, repo, operation string,
) (auth.LFSCredentials, error) {
	if ctx.Err() != nil || principal.Username != "alice" || len(key) == 0 || namespace != "alice" || repo != "demo" {
		return auth.LFSCredentials{}, errors.New("invalid LFS identity or scope")
	}
	a.issued.Add(1)
	return auth.LFSCredentials{
		Href:      "https://git.example/alice/demo.git/info/lfs",
		Header:    map[string]string{"Authorization": "Bearer test-" + operation},
		ExpiresIn: 900,
	}, nil
}

func TestParseLFSCommand(t *testing.T) {
	t.Parallel()
	server, err := New(unitServerOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, command string
		valid         bool
	}{
		{name: "unquoted download", command: "git-lfs-authenticate alice/demo.git download", valid: true},
		{name: "quoted upload", command: "git-lfs-authenticate 'alice/demo.git' upload", valid: true},
		{name: "leading slash", command: "git-lfs-authenticate '/alice/demo.git' upload", valid: true},
		{name: "extra argument", command: "git-lfs-authenticate alice/demo.git upload extra"},
		{name: "case operation", command: "git-lfs-authenticate alice/demo.git Upload"},
		{name: "unknown operation", command: "git-lfs-authenticate alice/demo.git delete"},
		{name: "missing operation", command: "git-lfs-authenticate alice/demo.git"},
		{name: "missing path", command: "git-lfs-authenticate upload"},
		{name: "shell suffix", command: "git-lfs-authenticate alice/demo.git upload;id"},
		{name: "shell substitution", command: "git-lfs-authenticate alice/$(id).git download"},
		{name: "double quoted", command: "git-lfs-authenticate \"alice/demo.git\" download"},
		{name: "broken quote", command: "git-lfs-authenticate 'alice/demo.git download"},
		{name: "traversal", command: "git-lfs-authenticate alice/../demo.git download"},
		{name: "newline", command: "git-lfs-authenticate alice/demo.git download\n"},
		{name: "pure SSH unsupported", command: "git-lfs-transfer alice/demo.git download"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := server.parseCommand(test.command)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v parsed=%+v err=%v", test.valid, parsed, err)
			}
			if test.valid && (parsed.service != "git-lfs-authenticate" || parsed.namespace != "alice" ||
				parsed.repository != "demo" || (parsed.lfsOperation != "upload" && parsed.lfsOperation != "download")) {
				t.Fatalf("unexpected command: %+v", parsed)
			}
		})
	}
}

func TestLFSAuthenticationSSH(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, operation         string
		readOnly, revoke, valid bool
	}{
		{name: "download", operation: "download", valid: true},
		{name: "upload", operation: "upload", valid: true},
		{name: "reader download", operation: "download", readOnly: true, valid: true},
		{name: "reader upload", operation: "upload", readOnly: true},
		{name: "revoked after handshake", operation: "download", revoke: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := unitServerOptions(t)
			authority := &unitLFSAuthority{unitAuthority: options.Authority.(*unitAuthority)}
			options.Authority = authority
			fixture := startUnitSSHOptions(t, options)
			client, err := fixture.dial(t, "alice", ssh.PublicKeys(unitSigner(t, 3)))
			if err != nil {
				t.Fatal(err)
			}
			authority.readOnly.Store(test.readOnly)
			authority.revoked.Store(test.revoke)
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			output, err := session.Output("git-lfs-authenticate 'alice/demo.git' " + test.operation)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
			if !test.valid {
				if authority.issued.Load() != 0 || len(output) != 0 {
					t.Fatal("denied SSH command returned credentials")
				}
				return
			}
			var response auth.LFSCredentials
			if err := json.Unmarshal(output, &response); err != nil {
				t.Fatal(err)
			}
			if response.Href != "https://git.example/alice/demo.git/info/lfs" || response.ExpiresIn != 900 ||
				response.Header["Authorization"] != "Bearer test-"+test.operation ||
				authority.verified.Load() < 2 || authority.issued.Load() != 1 {
				t.Fatalf("invalid LFS authentication response: %+v", response)
			}
		})
	}
}

func TestLFSAuthenticationRoutesToRepositoryShard(t *testing.T) {
	t.Parallel()
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		t.Fatal(err)
	}
	router, err := shard.NewRouter(2, parser)
	if err != nil {
		t.Fatal(err)
	}
	ownerOptions := unitServerOptions(t)
	ownerAuthority := &unitLFSAuthority{unitAuthority: ownerOptions.Authority.(*unitAuthority)}
	ownerOptions.Router, ownerOptions.LocalShard, ownerOptions.Authority = router, 1, ownerAuthority
	owner := startUnitSSHOptions(t, ownerOptions)
	entryOptions := unitServerOptions(t)
	entryAuthority := &unitLFSAuthority{unitAuthority: entryOptions.Authority.(*unitAuthority)}
	entryOptions.Router, entryOptions.LocalShard, entryOptions.Authority = router, 0, entryAuthority
	entryOptions.PeerAddress = func(id shard.ShardID) (string, error) {
		if id != 1 {
			return "", errors.New("unexpected owner")
		}
		return owner.address, nil
	}
	entry := startUnitSSHOptions(t, entryOptions)
	client, err := entry.dial(t, "alice", ssh.PublicKeys(unitSigner(t, 3)))
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	output, err := session.Output("git-lfs-authenticate alice/demo.git upload")
	if err != nil || !strings.Contains(string(output), "https://git.example/alice/demo.git/info/lfs") {
		t.Fatalf("routed authentication failed: %s %v", output, err)
	}
	if entryAuthority.issued.Load() != 0 || ownerAuthority.issued.Load() != 1 || entryAuthority.authorized.Load() != 0 {
		t.Fatal("LFS credentials were not issued exclusively on the repository shard")
	}
}
