package config

import "testing"

func TestSSHValidation(t *testing.T) {
	t.Parallel()
	valid := SSH{Enabled: true, Port: 2222, PublicURL: "ssh://git.example:2222", HostKeyFile: "/keys/host", ForwardKeyFile: "/keys/forward"}
	if err := valid.validate(true, 8080); err != nil {
		t.Fatal(err)
	}
	if err := (SSH{}).validate(false, 8080); err != nil {
		t.Fatal(err)
	}
	if err := valid.validate(false, 8080); err == nil {
		t.Fatal("SSH without auth accepted")
	}
	for name, change := range map[string]func(*SSH){
		"disabled with keys": func(s *SSH) { s.Enabled = false },
		"zero port":          func(s *SSH) { s.Port = 0 },
		"HTTP port":          func(s *SSH) { s.Port = 8080 },
		"wrong scheme":       func(s *SSH) { s.PublicURL = "https://git.example" },
		"username":           func(s *SSH) { s.PublicURL = "ssh://alice@git.example" },
		"path":               func(s *SSH) { s.PublicURL = "ssh://git.example/path" },
		"query":              func(s *SSH) { s.PublicURL = "ssh://git.example?x" },
		"fragment":           func(s *SSH) { s.PublicURL = "ssh://git.example#x" },
		"invalid port":       func(s *SSH) { s.PublicURL = "ssh://git.example:65536" },
		"zero public port":   func(s *SSH) { s.PublicURL = "ssh://git.example:0" },
		"relative key":       func(s *SSH) { s.HostKeyFile = "host" },
		"missing key":        func(s *SSH) { s.ForwardKeyFile = "" },
		"same keys":          func(s *SSH) { s.ForwardKeyFile = s.HostKeyFile },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			change(&cfg)
			if err := cfg.validate(true, 8080); err == nil {
				t.Fatal("invalid SSH configuration accepted")
			}
		})
	}
}

func TestLoadSSH(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"GITONE_SSH_ENABLED", "GITONE_SSH_PORT"} {
		if _, err := loadSSH(testLookup(map[string]string{key: "invalid"})); err == nil {
			t.Fatalf("accepted %s", key)
		}
	}
	cfg, err := loadSSH(testLookup(map[string]string{
		"GITONE_SSH_ENABLED": "true", "GITONE_SSH_PORT": "2223", "GITONE_SSH_PUBLIC_URL": "ssh://git.example",
		"GITONE_SSH_HOST_KEY_FILE": "/keys/host", "GITONE_SSH_FORWARD_KEY_FILE": "/keys/forward",
	}))
	if err != nil || !cfg.Enabled || cfg.Port != 2223 || cfg.validate(true, 8080) != nil {
		t.Fatalf("SSH load: %+v, %v", cfg, err)
	}
}
