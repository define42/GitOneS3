package config

import (
	"strings"
	"testing"
)

func TestMetricsToken(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		token string
		valid bool
	}{
		{name: "disabled", valid: true},
		{name: "minimum", token: strings.Repeat("a", 32), valid: true},
		{name: "maximum", token: strings.Repeat("a", 1024), valid: true},
		{name: "base64", token: strings.Repeat("a", 32) + "+/==", valid: true},
		{name: "URL token", token: strings.Repeat("a", 32) + "-._~", valid: true},
		{name: "too short", token: strings.Repeat("a", 31)},
		{name: "too long", token: strings.Repeat("a", 1025)},
		{name: "newline", token: strings.Repeat("a", 32) + "\n"},
		{name: "space", token: strings.Repeat("a", 32) + " "},
		{name: "non-ASCII", token: strings.Repeat("ø", 32)},
		{name: "only padding", token: strings.Repeat("=", 32)},
		{name: "internal padding", token: strings.Repeat("a", 32) + "=a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			environment := baseEnvironment()
			environment["GITONE_METRICS_TOKEN"] = test.token
			cfg, err := Load(testLookup(environment))
			if (err == nil) != test.valid {
				t.Fatalf("Load error = %v, want valid %v", err, test.valid)
			}
			if test.valid && cfg.MetricsToken != test.token {
				t.Fatal("Load changed the token")
			}
			if err != nil && strings.Contains(err.Error(), test.token) {
				t.Fatal("Load error contains the token")
			}
			base, err := Load(testLookup(baseEnvironment()))
			if err != nil {
				t.Fatal(err)
			}
			base.MetricsToken = test.token
			if err := base.Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate error = %v, want valid %v", err, test.valid)
			}
		})
	}
}
