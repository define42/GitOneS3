package config

import (
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
)

// SSH configures the optional Git-only listener and authenticated shard peers.
type SSH struct {
	Enabled        bool
	Port           uint16
	PublicURL      string
	HostKeyFile    string
	ForwardKeyFile string
}

func loadSSH(lookup LookupEnv) (SSH, error) {
	enabled, err := boolValue(lookup, "GITONE_SSH_ENABLED", false)
	if err != nil {
		return SSH{}, err
	}
	port, err := uint16Value(lookup, "GITONE_SSH_PORT", 2222)
	if err != nil {
		return SSH{}, err
	}
	return SSH{
		Enabled: enabled, Port: port,
		PublicURL:      value(lookup, "GITONE_SSH_PUBLIC_URL", ""),
		HostKeyFile:    value(lookup, "GITONE_SSH_HOST_KEY_FILE", ""),
		ForwardKeyFile: value(lookup, "GITONE_SSH_FORWARD_KEY_FILE", ""),
	}, nil
}

func (s SSH) validate(authEnabled bool, httpPort uint16) error {
	if !s.Enabled {
		if s.PublicURL != "" || s.HostKeyFile != "" || s.ForwardKeyFile != "" {
			return errors.New("config: SSH settings require GITONE_SSH_ENABLED=true")
		}
		return nil
	}
	if !authEnabled {
		return errors.New("config: SSH requires authentication")
	}
	if s.Port == 0 || s.Port == httpPort {
		return errors.New("config: SSH port must be positive and distinct from HTTP")
	}
	u, err := url.Parse(s.PublicURL)
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("config: SSH public URL must be an ssh origin without credentials, path, query, or fragment")
	}
	if u.Port() != "" {
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 {
			return errors.New("config: invalid SSH public URL port")
		}
	}
	if !filepath.IsAbs(s.HostKeyFile) || !filepath.IsAbs(s.ForwardKeyFile) || s.HostKeyFile == s.ForwardKeyFile {
		return errors.New("config: SSH requires distinct absolute host and forwarding private-key file paths")
	}
	return nil
}
