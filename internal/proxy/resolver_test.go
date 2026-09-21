package proxy

import (
	"errors"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/shard"
)

func TestStatefulSetResolverResolve(t *testing.T) {
	t.Parallel()

	resolver, err := NewStatefulSetResolver(StatefulSetResolverOptions{
		Scheme:          "https",
		StatefulSet:     "gitone",
		HeadlessService: "gitone-headless",
		Namespace:       "production",
		ServiceDomain:   "svc.cluster.local",
		Port:            8443,
	})
	if err != nil {
		t.Fatalf("NewStatefulSetResolver() error = %v", err)
	}

	destination, err := resolver.Resolve(shard.ShardID(419))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	const expected = "https://gitone-419.gitone-headless.production.svc.cluster.local:8443"
	if actual := destination.String(); actual != expected {
		t.Fatalf("Resolve() = %q, expected %q", actual, expected)
	}

	destination.Host = "mutated.invalid"
	second, err := resolver.Resolve(shard.ShardID(419))
	if err != nil {
		t.Fatalf("second Resolve() error = %v", err)
	}
	if actual := second.String(); actual != expected {
		t.Fatalf("second Resolve() = %q, expected independent %q", actual, expected)
	}
}

func TestStatefulSetResolverDefaultServiceDomain(t *testing.T) {
	t.Parallel()

	resolver, err := NewStatefulSetResolver(StatefulSetResolverOptions{
		Scheme:          "http",
		StatefulSet:     "gitone",
		HeadlessService: "gitone-headless",
		Namespace:       "default",
		Port:            8080,
	})
	if err != nil {
		t.Fatalf("NewStatefulSetResolver() error = %v", err)
	}
	destination, err := resolver.Resolve(7)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	const expected = "http://gitone-7.gitone-headless.default.svc:8080"
	if actual := destination.String(); actual != expected {
		t.Fatalf("Resolve() = %q, expected %q", actual, expected)
	}
}

func TestNewStatefulSetResolverRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	valid := StatefulSetResolverOptions{
		Scheme:          "https",
		StatefulSet:     "gitone",
		HeadlessService: "gitone-headless",
		Namespace:       "production",
		ServiceDomain:   "svc.cluster.local",
		Port:            8443,
	}
	tests := []struct {
		name   string
		mutate func(*StatefulSetResolverOptions)
	}{
		{name: "unsupported scheme", mutate: func(o *StatefulSetResolverOptions) { o.Scheme = "ftp" }},
		{name: "zero port", mutate: func(o *StatefulSetResolverOptions) { o.Port = 0 }},
		{name: "uppercase statefulset", mutate: func(o *StatefulSetResolverOptions) { o.StatefulSet = "GitOne" }},
		{name: "empty service", mutate: func(o *StatefulSetResolverOptions) { o.HeadlessService = "" }},
		{name: "edge hyphen namespace", mutate: func(o *StatefulSetResolverOptions) { o.Namespace = "-prod" }},
		{name: "empty domain label", mutate: func(o *StatefulSetResolverOptions) { o.ServiceDomain = "svc..local" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := valid
			test.mutate(&options)
			_, err := NewStatefulSetResolver(options)
			if !errors.Is(err, ErrInvalidResolverConfig) {
				t.Fatalf("NewStatefulSetResolver() error = %v, expected resolver config error", err)
			}
		})
	}
}

func TestStatefulSetResolverRejectsGeneratedLongPodName(t *testing.T) {
	t.Parallel()

	resolver, err := NewStatefulSetResolver(StatefulSetResolverOptions{
		Scheme:          "http",
		StatefulSet:     strings.Repeat("a", 63),
		HeadlessService: "headless",
		Namespace:       "default",
		Port:            8080,
	})
	if err != nil {
		t.Fatalf("NewStatefulSetResolver() error = %v", err)
	}
	if _, err := resolver.Resolve(1); !errors.Is(err, ErrInvalidResolverConfig) {
		t.Fatalf("Resolve() error = %v, expected resolver config error", err)
	}
}
