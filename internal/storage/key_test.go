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

func TestValidateListPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prefix  string
		after   string
		limit   int
		wantErr bool
	}{
		{name: "first page", prefix: "packs/", limit: 1},
		{name: "maximum page", prefix: "packs/", after: "packs/a.pack", limit: MaxListPageSize},
		{name: "partial prefix", prefix: "packs/a", after: "packs/abc.pack", limit: 10},
		{name: "cursor equals prefix key", prefix: "packs", after: "packs", limit: 10},
		{name: "empty prefix", limit: 1, wantErr: true},
		{name: "ambiguous prefix", prefix: "packs//", limit: 1, wantErr: true},
		{name: "zero limit", prefix: "packs/", wantErr: true},
		{name: "negative limit", prefix: "packs/", limit: -1, wantErr: true},
		{name: "excessive limit", prefix: "packs/", limit: MaxListPageSize + 1, wantErr: true},
		{name: "cursor outside prefix", prefix: "packs/", after: "other/a.pack", limit: 1, wantErr: true},
		{name: "cursor is prefix only", prefix: "packs/", after: "packs/", limit: 1, wantErr: true},
		{name: "cursor traversal", prefix: "packs/", after: "packs/../a.pack", limit: 1, wantErr: true},
		{name: "invalid cursor encoding", prefix: "packs/", after: "packs/\xff", limit: 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateListPage(test.prefix, test.after, test.limit)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateListPage() error = %v, want error %v", err, test.wantErr)
			}
		})
	}
}
