package sshserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

func (s *Server) parseLFSCommand(command string) (gitCommand, error) {
	const service = "git-lfs-authenticate"
	remainder := strings.TrimPrefix(command, service+" ")
	path, operation, ok := strings.Cut(remainder, " ")
	if !ok || (operation != "upload" && operation != "download") || len(command) > 256 ||
		strings.ContainsAny(command, "\t\r\n\x00") {
		return gitCommand{}, errors.New("ssh: invalid LFS authentication command")
	}
	// Native LFS clients use an unquoted path when it contains no shell
	// metacharacters. Reuse Git's strict path parser for either exact form.
	if !strings.HasPrefix(path, "'") {
		path = "'" + path + "'"
	}
	parsed, err := s.parseCommand("git-upload-pack " + path)
	if err != nil {
		return gitCommand{}, err
	}
	parsed.service, parsed.lfsOperation = service, operation
	return parsed, nil
}

func (s *Server) serveLFSAuthentication(ctx context.Context, request peerRequest, command gitCommand, channel ssh.Channel) error {
	authority, ok := s.options.Authority.(LFSAuthority)
	if !ok {
		return errors.New("ssh: LFS authentication unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, authorityTimeout)
	defer cancel()
	principal, err := s.verify(ctx, request.Username, request.Key)
	if err != nil {
		return err
	}
	if err := s.options.Authority.AuthorizeSSH(ctx, principal, command.namespace, command.lfsOperation == "upload"); err != nil {
		return err
	}
	credentials, err := authority.IssueLFSCredentials(ctx, principal, request.Key, command.namespace, command.repository, command.lfsOperation)
	if err != nil {
		return err
	}
	return json.NewEncoder(channel).Encode(credentials)
}
