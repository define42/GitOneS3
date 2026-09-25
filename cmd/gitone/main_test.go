package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestRunHelpWithoutConfiguration(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--help", "-h", "help"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			err := run(
				t.Context(),
				nil,
				[]string{flag},
				nil,
				&output,
			)
			if err != nil || !strings.Contains(output.String(), "backfill-space-index") {
				t.Fatalf("help output = %q, error = %v", output.String(), err)
			}
		})
	}
}

func TestRunRejectsArgumentsBeforeConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "unknown command", args: []string{"migrate"}},
		{name: "extra migration argument", args: []string{"backfill-space-index", "all"}},
		{name: "extra serving argument", args: []string{"serve", "--force"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := run(
				t.Context(),
				nil,
				test.args,
				nil,
				io.Discard,
			)
			if err == nil || !strings.Contains(err.Error(), "invalid command") {
				t.Fatalf("run() error = %v, want invalid command", err)
			}
		})
	}
}

func TestRunBackfillRequiresScanMode(t *testing.T) {
	t.Parallel()
	lookup := func(key string) (string, bool) {
		values := map[string]string{
			"GITONE_SHARD_COUNT": "1", "POD_NAME": "gitone-0", "POD_NAMESPACE": "local",
		}
		value, ok := values[key]
		return value, ok
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(
		t.Context(),
		logger,
		[]string{"backfill-space-index"},
		lookup,
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "GITONE_SPACE_DISCOVERY_MODE=scan") {
		t.Fatalf("run() error = %v, want scan-mode guard before opening the store", err)
	}
}

func TestRunReportsHelpWriteFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("output closed")
	err := run(
		t.Context(),
		nil,
		[]string{"--help"},
		nil,
		failingWriter{err: want},
	)
	if !errors.Is(err, want) {
		t.Fatalf("run() error = %v, want %v", err, want)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }
