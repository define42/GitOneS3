// Package sshserver serves Git-only SSH sessions with authenticated shard routing.
package sshserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/define42/GitOneS3/internal/auth"
	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/shard"
)

const (
	peerUser         = "__gitone_peer"
	maxCommand       = 16384
	operationTimeout = 90 * time.Second
	authorityTimeout = 5 * time.Second
)

// Authority verifies keys on the user shard and ACLs on the repository shard.
type Authority interface {
	VerifySSHKey(context.Context, string, []byte) (auth.SSHPrincipal, error)
	AuthorizeSSH(context.Context, auth.SSHPrincipal, string, bool) error
}

// LFSAuthority exchanges a freshly authenticated SSH identity for a scoped
// GitOne HTTP credential. File bytes continue through the owning GitOne pod.
type LFSAuthority interface {
	IssueLFSCredentials(context.Context, auth.SSHPrincipal, []byte, string, string, string) (auth.LFSCredentials, error)
}

// Options supplies deployment-owned routing and distinct persistent SSH keys.
type Options struct {
	Address     string
	LocalShard  shard.ShardID
	Router      *shard.Router
	PeerAddress func(shard.ShardID) (string, error)
	HostKey     ssh.Signer
	ForwardKey  ssh.Signer
	Authority   Authority
	Git         *gittransport.Handler
	Logger      *slog.Logger
}

// Server owns a bounded listener. No shell or TCP forwarding is provided.
type Server struct{ options Options }

// New validates the dependencies before any listener is opened.
func New(options Options) (*Server, error) {
	if options.Address == "" || options.Router == nil || options.PeerAddress == nil ||
		options.HostKey == nil || options.ForwardKey == nil || options.Authority == nil || options.Git == nil {
		return nil, errors.New("ssh: missing server dependency")
	}
	if options.LocalShard >= shard.ShardID(options.Router.ShardCount()) {
		return nil, errors.New("ssh: invalid local shard")
	}
	if bytes.Equal(options.HostKey.PublicKey().Marshal(), options.ForwardKey.PublicKey().Marshal()) {
		return nil, errors.New("ssh: host and forwarding keys must differ")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Server{options: options}, nil
}

// LoadSigner reads bounded, unencrypted private-key material from a configured file.
func LoadSigner(path string) (ssh.Signer, error) {
	// #nosec G304 -- This path is an operator-configured secret mount, never client input.
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open SSH key: %w", err)
	}
	defer func() { _ = file.Close() }() // A read-only close cannot change parsing.
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return nil, errors.New("ssh: cannot read private key")
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, errors.New("ssh: invalid or encrypted private key")
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("ssh: server and forwarding keys must be Ed25519")
	}
	return signer, nil
}

// Run serves until cancellation or listener failure, waiting for all sessions.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", s.options.Address)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return fmt.Errorf("listen SSH: %w", err)
	}
	return s.Serve(ctx, listener)
}

// Serve takes ownership of listener and closes it and active sessions on exit.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	defer func() { _ = listener.Close() }()
	s.options.Logger.InfoContext(ctx, "ssh server starting", "address", listener.Addr().String())
	// Handshakes are bounded independently of Git's shared one-operation slot.
	slots := make(chan struct{}, 128)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer cancel()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept SSH: %w", err)
		}
		select {
		case slots <- struct{}{}:
			workers.Go(func() {
				defer func() { <-slots }()
				s.serveConnection(ctx, conn)
			})
		default:
			_ = conn.Close() // Reject excess connections without allocating workers.
		}
	}
}

func (s *Server) serveConnection(ctx context.Context, raw net.Conn) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	defer func() { _ = raw.Close() }()
	if err := raw.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	algorithms := ssh.SupportedAlgorithms()
	config := &ssh.ServerConfig{
		Config:                  ssh.Config{KeyExchanges: algorithms.KeyExchanges, Ciphers: algorithms.Ciphers, MACs: algorithms.MACs},
		PublicKeyAuthAlgorithms: algorithms.PublicKeyAuths,
		MaxAuthTries:            3,
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() == peerUser {
				if !bytes.Equal(key.Marshal(), s.options.ForwardKey.PublicKey().Marshal()) {
					return nil, errors.New("ssh: invalid peer")
				}
				return &ssh.Permissions{Extensions: map[string]string{"peer": "true"}}, nil
			}
			if _, err := s.verify(ctx, metadata.User(), key.Marshal()); err != nil {
				return nil, errors.New("ssh: invalid credentials")
			}
			return &ssh.Permissions{Extensions: map[string]string{
				"username": metadata.User(), "key": base64.RawStdEncoding.EncodeToString(key.Marshal()),
			}}, nil
		},
	}
	config.AddHostKey(s.options.HostKey)
	conn, channels, requests, err := ssh.NewServerConn(raw, config)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		return
	}
	var workers sync.WaitGroup
	workers.Go(func() { ; ssh.DiscardRequests(requests) })
	defer workers.Wait()
	defer func() { _ = conn.Close() }()
	// One command per connection, with all extra channels explicitly refused.
	used := false
	for incoming := range channels {
		if used || incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.Prohibited, "Git sessions only")
			continue
		}
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			return
		}
		used = true
		workers.Go(func() {
			s.serveSession(ctx, conn.Permissions, channel, channelRequests, &workers)
		})
	}
}

func (s *Server) serveSession(
	ctx context.Context,
	permissions *ssh.Permissions,
	channel ssh.Channel,
	requests <-chan *ssh.Request,
	workers *sync.WaitGroup,
) {
	defer func() { _ = channel.Close() }()
	for request := range requests {
		var payload struct{ Command string }
		if request.Type != "exec" || len(request.Payload) > maxCommand ||
			ssh.Unmarshal(request.Payload, &payload) != nil {
			_ = request.Reply(false, nil)
			continue
		}
		if err := request.Reply(true, nil); err != nil {
			return
		}
		// Drain and reject any additional requests while Git uses the channel.
		done := make(chan struct{})
		drainCtx, stopDrain := context.WithCancel(ctx)
		go func() {
			defer close(done)
			for {
				select {
				case request, ok := <-requests:
					if !ok {
						return
					}
					_ = request.Reply(false, nil)
				case <-drainCtx.Done():
					return
				}
			}
		}()
		err := s.execute(ctx, permissions, payload.Command, channel, workers)
		status := uint32(0)
		if err != nil {
			status = 1
			_, _ = io.WriteString(channel.Stderr(), "GitOne: access denied, invalid Git request, or service unavailable.\n")
			s.options.Logger.WarnContext(ctx, "SSH operation rejected", "error", err)
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		_ = channel.Close()
		stopDrain()
		<-done
		return
	}
}
