package app

import (
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/config"
)

func TestBackfillSpaceIndexRequiresScanMode(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		mode string
	}{
		{name: "default indexed"},
		{name: "explicit indexed", mode: "indexed"},
		{name: "invalid mode", mode: "automatic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := BackfillSpaceIndex(t.Context(), config.Config{SpaceDiscoveryMode: test.mode})
			if err == nil || !strings.Contains(err.Error(), "GITONE_SPACE_DISCOVERY_MODE=scan") {
				t.Fatalf("BackfillSpaceIndex() error = %v, want rollout guard", err)
			}
		})
	}
}

func TestBackfillSpaceIndexValidatesConfiguration(t *testing.T) {
	t.Parallel()
	err := BackfillSpaceIndex(t.Context(), config.Config{SpaceDiscoveryMode: "scan"})
	if err == nil || !strings.Contains(err.Error(), "config:") {
		t.Fatalf("BackfillSpaceIndex() error = %v, want invalid configuration", err)
	}
}
