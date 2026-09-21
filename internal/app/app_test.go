package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/define42/GitOneS3/internal/config"
)

func TestReadInternalToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		contents  string
		want      string
		wantError bool
	}{
		{
			name:     "trailing secret newline",
			contents: strings.Repeat("a", 32) + "\n",
			want:     strings.Repeat("a", 32),
		},
		{
			name:      "too short",
			contents:  "short",
			wantError: true,
		},
		{
			name:      "embedded whitespace",
			contents:  strings.Repeat("a", 32) + " b",
			wantError: true,
		},
		{
			name:      "embedded nul",
			contents:  strings.Repeat("a", 32) + "\x00b",
			wantError: true,
		},
		{
			name:      "delete byte",
			contents:  strings.Repeat("a", 32) + "\x7f",
			wantError: true,
		},
		{
			name:      "non ascii",
			contents:  strings.Repeat("a", 32) + "é",
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			got, err := readInternalToken(path)
			if test.wantError && err == nil {
				t.Fatal("readInternalToken() error = nil, want error")
			}
			if !test.wantError && err != nil {
				t.Fatalf("readInternalToken() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("readInternalToken() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestListenAddress(t *testing.T) {
	t.Parallel()

	if got, want := listenAddress("::", 8080), "[::]:8080"; got != want {
		t.Fatalf("listenAddress() = %q, want %q", got, want)
	}
}

func TestNewForwardTransportBypassesEnvironmentProxy(t *testing.T) {
	t.Parallel()

	transport, err := newForwardTransport(256)
	if err != nil {
		t.Fatalf("newForwardTransport() error = %v", err)
	}
	if transport.Proxy != nil {
		t.Fatal("newForwardTransport() retained an HTTP proxy function")
	}
	if transport == http.DefaultTransport {
		t.Fatal("newForwardTransport() returned the shared default transport")
	}
}

func TestConfigureS3ClientPinsEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{name: "AWS regional endpoint", endpoint: "", want: ""},
		{name: "explicit compatible endpoint", endpoint: "https://s3.example", want: "https://s3.example"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := s3.Options{BaseEndpoint: aws.String("https://environment-override.invalid")}
			configureS3Client(config.S3{
				Endpoint:     test.endpoint,
				UsePathStyle: true,
			})(&options)
			if got := aws.ToString(options.BaseEndpoint); got != test.want {
				t.Fatalf("BaseEndpoint = %q, want %q", got, test.want)
			}
			if !options.UsePathStyle {
				t.Fatal("UsePathStyle was not applied")
			}
		})
	}
}
