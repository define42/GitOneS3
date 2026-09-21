package storage

import (
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"repos/AD-01/states/0001.json",
		"namespaces/acme/root.json",
		"repos/répo/states/refs.json",
	} {
		if err := ValidateKey(key); err != nil {
			t.Errorf("ValidateKey(%q) error = %v", key, err)
		}
	}

	for _, key := range []string{
		"",
		"/absolute",
		"repos//state",
		"repos/../state",
		`repos\state`,
		"repos/state\x00",
		"repos/state\n",
		"repos/state\x7f",
		string([]byte{'r', 'e', 'p', 'o', 's', '/', 0xff}),
		strings.Repeat("a", 1025),
	} {
		if err := ValidateKey(key); err == nil {
			t.Errorf("ValidateKey(%q) error = nil, want rejection", key)
		}
	}
}

func TestValidatePrefixAllowsOneTrailingSlash(t *testing.T) {
	t.Parallel()

	if err := ValidatePrefix("repos/AD-01/"); err != nil {
		t.Fatalf("ValidatePrefix() error = %v", err)
	}
	if err := ValidatePrefix("repos/AD-01//"); err == nil {
		t.Fatal("ValidatePrefix() error = nil, want repeated slash rejection")
	}
}
