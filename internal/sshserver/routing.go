package sshserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/define42/GitOneS3/internal/auth"
	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/shard"
)

type peerRequest struct {
	Operation string `json:"operation"`
	Username  string `json:"username"`
	Key       []byte `json:"key"`
	Command   string `json:"command,omitempty"`
}

type gitCommand struct{ service, namespace, repository string }

// Parse only Git's exact single-quoted command form; never invoke a shell.
func (s *Server) parseCommand(command string) (gitCommand, error) {
	service, path, ok := strings.Cut(command, " ")
	if !ok || (service != "git-upload-pack" && service != "git-receive-pack") ||
		len(path) < 3 || path[0] != '\'' || path[len(path)-1] != '\'' || len(command) > 256 {
		return gitCommand{}, errors.New("ssh: only Git upload/receive commands are supported")
	}
	path = strings.TrimPrefix(path[1:len(path)-1], "/")
	namespace, repo, ok := strings.Cut(path, "/")
	if !ok || !strings.HasSuffix(repo, ".git") {
		return gitCommand{}, errors.New("ssh: invalid repository path")
	}
	repo = strings.TrimSuffix(repo, ".git")
	if _, err := s.options.Router.Owner(namespace); err != nil || namespace == "auth" || !repository.ValidName(repo) {
		return gitCommand{}, errors.New("ssh: invalid repository path")
	}
	return gitCommand{service: service, namespace: namespace, repository: repo}, nil
}

func (s *Server) execute(
	ctx context.Context,
	permissions *ssh.Permissions,
	command string,
	channel ssh.Channel,
	workers *sync.WaitGroup,
) error {
	if permissions == nil {
		return errors.New("ssh: missing authenticated identity")
	}
	if permissions.Extensions["peer"] == "true" {
		request, err := decodePeerRequest(command)
		if err != nil {
			return err
		}
		if request.Operation == "key-check" {
			owner, err := s.options.Router.Owner(request.Username)
			if err != nil || owner != s.options.LocalShard {
				return errors.New("ssh: wrong key authority shard")
			}
			principal, err := s.verify(ctx, request.Username, request.Key)
			if err != nil {
				return err
			}
			return json.NewEncoder(channel).Encode(principal)
		}
		if request.Operation != "git" {
			return errors.New("ssh: invalid peer operation")
		}
		parsed, err := s.parseCommand(request.Command)
		if err != nil {
			return err
		}
		owner, err := s.options.Router.Owner(parsed.namespace)
		if err != nil || owner != s.options.LocalShard {
			return errors.New("ssh: wrong repository shard")
		}
		// Delegated Git operations terminate here; never delegate a second time.
		return s.serveGit(ctx, request, parsed, channel)
	}
	parsed, err := s.parseCommand(command)
	if err != nil {
		return err
	}
	key, err := base64.RawStdEncoding.DecodeString(permissions.Extensions["key"])
	if err != nil {
		return errors.New("ssh: invalid authenticated key")
	}
	request := peerRequest{Operation: "git", Username: permissions.Extensions["username"], Key: key, Command: command}
	owner, err := s.options.Router.Owner(parsed.namespace)
	if err != nil {
		return err
	}
	if owner == s.options.LocalShard {
		return s.serveGit(ctx, request, parsed, channel)
	}
	return s.forward(ctx, owner, request, channel, workers)
}

func (s *Server) serveGit(ctx context.Context, request peerRequest, command gitCommand, channel ssh.Channel) error {
	authorize := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, authorityTimeout)
		defer cancel()
		principal, err := s.verify(ctx, request.Username, request.Key)
		if err != nil {
			return err
		}
		return s.options.Authority.AuthorizeSSH(ctx, principal, command.namespace, command.service == "git-receive-pack")
	}
	if err := authorize(ctx); err != nil {
		return err
	}
	ctx = gittransport.WithWriteAuthorization(ctx, authorize)
	return s.options.Git.ServeSSH(ctx, gittransport.SSHRequest{
		Namespace: command.namespace, Repository: command.repository, Service: command.service, Stream: channel,
	})
}

func (s *Server) verify(ctx context.Context, username string, key []byte) (auth.SSHPrincipal, error) {
	ctx, cancel := context.WithTimeout(ctx, authorityTimeout)
	defer cancel()
	owner, err := s.options.Router.Owner(username)
	if err != nil || username == "auth" || len(key) == 0 || len(key) > 8192 {
		return auth.SSHPrincipal{}, errors.New("ssh: invalid credentials")
	}
	if owner == s.options.LocalShard {
		return s.options.Authority.VerifySSHKey(ctx, username, key)
	}
	client, cleanup, err := s.peer(ctx, owner)
	if err != nil {
		return auth.SSHPrincipal{}, err
	}
	defer cleanup()
	session, err := client.NewSession()
	if err != nil {
		return auth.SSHPrincipal{}, fmt.Errorf("SSH authority session: %w", err)
	}
	defer func() { _ = session.Close() }()
	command, err := encodePeerRequest(peerRequest{Operation: "key-check", Username: username, Key: key})
	if err != nil {
		return auth.SSHPrincipal{}, err
	}
	var output boundedOutput
	session.Stdout = &output
	if err := session.Run(command); err != nil {
		return auth.SSHPrincipal{}, errors.New("ssh: key authority rejected request")
	}
	var principal auth.SSHPrincipal
	if json.Unmarshal(output.Bytes(), &principal) != nil || principal.Username != username {
		return auth.SSHPrincipal{}, errors.New("ssh: invalid authority response")
	}
	return principal, nil
}

func (s *Server) peer(ctx context.Context, owner shard.ShardID) (*ssh.Client, func(), error) {
	address, err := s.options.PeerAddress(owner)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve SSH peer: %w", err)
	}
	var dialer net.Dialer
	raw, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("dial SSH peer: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	cleanup := func() { stop(); _ = raw.Close() }
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := raw.SetDeadline(deadline); err != nil {
		cleanup()
		return nil, nil, err
	}
	algorithms := ssh.SupportedAlgorithms()
	config := &ssh.ClientConfig{
		Config: ssh.Config{KeyExchanges: algorithms.KeyExchanges, Ciphers: algorithms.Ciphers, MACs: algorithms.MACs},
		User:   peerUser, Auth: []ssh.AuthMethod{ssh.PublicKeys(s.options.ForwardKey)},
		HostKeyCallback:   ssh.FixedHostKey(s.options.HostKey.PublicKey()),
		HostKeyAlgorithms: algorithms.HostKeys,
	}
	conn, channels, requests, err := ssh.NewClientConn(raw, address, config)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("authenticate SSH peer: %w", err)
	}
	deadline = time.Now().Add(operationTimeout)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := raw.SetDeadline(deadline); err != nil {
		cleanup()
		return nil, nil, err
	}
	client := ssh.NewClient(conn, channels, requests)
	return client, func() { _ = client.Close(); cleanup() }, nil
}

func (s *Server) forward(
	ctx context.Context,
	owner shard.ShardID,
	request peerRequest,
	channel ssh.Channel,
	workers *sync.WaitGroup,
) error {
	client, cleanup, err := s.peer(ctx, owner)
	if err != nil {
		return err
	}
	defer cleanup()
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH Git peer session: %w", err)
	}
	defer func() { _ = session.Close() }()
	command, err := encodePeerRequest(request)
	if err != nil {
		return err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	session.Stdout, session.Stderr = channel, channel.Stderr()
	if err := session.Start(command); err != nil {
		return err
	}
	workers.Go(func() {
		// The outer connection closes after exit-status, unblocking this reader
		// even when the Git client never sends stdin EOF. Its owner joins us.
		_, err := io.Copy(stdin, io.LimitReader(channel, 74<<20))
		_ = stdin.Close()
		if err != nil {
			_ = client.Close()
		}
	})
	if err := session.Wait(); err != nil {
		return fmt.Errorf("SSH Git peer: %w", err)
	}
	return nil
}

func encodePeerRequest(request peerRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	command := "gitone-peer " + base64.RawStdEncoding.EncodeToString(data)
	if len(command) > maxCommand-4 {
		return "", errors.New("ssh: peer request too large")
	}
	return command, nil
}

func decodePeerRequest(command string) (peerRequest, error) {
	var request peerRequest
	if !strings.HasPrefix(command, "gitone-peer ") || len(command) > maxCommand {
		return request, errors.New("ssh: invalid peer command")
	}
	data, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(command, "gitone-peer "))
	if err != nil {
		return request, errors.New("ssh: invalid peer request")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || len(request.Key) > 8192 {
		return request, errors.New("ssh: invalid peer request")
	}
	return request, nil
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 8192 {
		return 0, errors.New("ssh: authority response too large")
	}
	return b.Buffer.Write(data)
}
