package app

import (
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/define42/GitOneS3/internal/config"
)

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
