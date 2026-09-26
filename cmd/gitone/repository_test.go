package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/app"
	"github.com/define42/GitOneS3/internal/repository"
)

func TestRepositoryHelpWithoutConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "command help", args: []string{"repository", "--help"}},
		{name: "short help", args: []string{"repository", "-h"}},
		{name: "help command", args: []string{"repository", "help"}},
		{name: "operation help", args: []string{"repository", "gc", "--help"}},
		{name: "target help", args: []string{"repository", "gc", "alice/demo", "--help"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			if err := run(t.Context(), nil, test.args, nil, &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "--offline") || !strings.Contains(output.String(), "--snapshot") {
				t.Fatalf("incomplete help: %s", output.String())
			}
		})
	}
}

func TestParseRepositoryCommand(t *testing.T) {
	t.Parallel()
	snapshot := "states/00000000000000000001-state-" + strings.Repeat("a", 64) + ".json"
	token := strings.Repeat("a", 32)
	for _, test := range []struct {
		name string
		args []string
		want app.RepositoryMaintenanceOptions
	}{
		{name: "check", args: []string{"check", "alice/demo"}, want: app.RepositoryMaintenanceOptions{Operation: "check"}},
		{name: "generations", args: []string{"generations", "alice/demo"}, want: app.RepositoryMaintenanceOptions{Operation: "generations"}},
		{name: "repack", args: []string{"repack", "alice/demo"}, want: app.RepositoryMaintenanceOptions{Operation: "repack"}},
		{name: "lock", args: []string{"lock", "alice/demo"}, want: app.RepositoryMaintenanceOptions{Operation: "lock"}},
		{name: "gc defaults", args: []string{"gc", "alice/demo"}, want: app.RepositoryMaintenanceOptions{Operation: "gc"}},
		{name: "gc flags after target", args: []string{"gc", "alice/demo", "--apply", "--grace-period", "48h"}, want: app.RepositoryMaintenanceOptions{Operation: "gc", Apply: true, GracePeriod: 48 * time.Hour}},
		{name: "gc flags before target", args: []string{"gc", "--grace-period=48h", "--apply=true", "alice/demo"}, want: app.RepositoryMaintenanceOptions{Operation: "gc", Apply: true, GracePeriod: 48 * time.Hour}},
		{name: "gc mixed flags", args: []string{"gc", "--apply", "alice/demo", "--grace-period=48h"}, want: app.RepositoryMaintenanceOptions{Operation: "gc", Apply: true, GracePeriod: 48 * time.Hour}},
		{name: "gc explicit dry run", args: []string{"gc", "alice/demo", "--apply=false"}, want: app.RepositoryMaintenanceOptions{Operation: "gc"}},
		{name: "restore", args: []string{"restore", "alice/demo", "--snapshot", snapshot}, want: app.RepositoryMaintenanceOptions{Operation: "restore", Snapshot: snapshot}},
		{name: "unlock", args: []string{"unlock", "--token=" + token, "alice/demo", "--offline"}, want: app.RepositoryMaintenanceOptions{Operation: "unlock", Token: token, Offline: true}},
		{name: "end flags", args: []string{"gc", "--apply", "--", "alice/demo"}, want: app.RepositoryMaintenanceOptions{Operation: "gc", Apply: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseRepositoryCommand(test.args)
			if err != nil {
				t.Fatal(err)
			}
			test.want.Namespace, test.want.Repository = "alice", "demo"
			if got != test.want {
				t.Fatalf("parsed %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestRepositoryRejectsArgumentsBeforeConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing operation"},
		{name: "unknown operation", args: []string{"wipe", "alice/demo"}},
		{name: "missing target", args: []string{"check"}},
		{name: "two targets", args: []string{"check", "alice/demo", "bob/demo"}},
		{name: "namespace only", args: []string{"check", "alice"}},
		{name: "empty namespace", args: []string{"check", "/demo"}},
		{name: "empty repository", args: []string{"check", "alice/"}},
		{name: "uppercase", args: []string{"check", "Alice/demo"}},
		{name: "reserved namespace", args: []string{"check", "system/demo"}},
		{name: "auth namespace", args: []string{"check", "auth/demo"}},
		{name: "nested target", args: []string{"check", "alice/group/demo"}},
		{name: "traversal", args: []string{"check", "alice/../demo"}},
		{name: "encoded path", args: []string{"check", "alice/de%6do"}},
		{name: "git suffix", args: []string{"check", "alice/demo.git"}},
		{name: "URL target", args: []string{"check", "https://host/alice/demo.git"}},
		{name: "negative grace", args: []string{"gc", "alice/demo", "--grace-period=-1h"}},
		{name: "invalid duration", args: []string{"gc", "alice/demo", "--grace-period=tomorrow"}},
		{name: "missing duration", args: []string{"gc", "alice/demo", "--grace-period"}},
		{name: "unknown flag", args: []string{"gc", "alice/demo", "--force"}},
		{name: "duplicate flag", args: []string{"gc", "alice/demo", "--apply=false", "--apply"}},
		{name: "inapplicable false flag", args: []string{"check", "alice/demo", "--apply=false"}},
		{name: "inapplicable zero grace", args: []string{"repack", "alice/demo", "--grace-period=0s"}},
		{name: "missing snapshot", args: []string{"restore", "alice/demo"}},
		{name: "invalid snapshot", args: []string{"restore", "alice/demo", "--snapshot=states/../state"}},
		{name: "snapshot generation only", args: []string{"restore", "alice/demo", "--snapshot=1"}},
		{name: "unlock missing offline", args: []string{"unlock", "alice/demo", "--token=" + strings.Repeat("a", 32)}},
		{name: "unlock false offline", args: []string{"unlock", "alice/demo", "--token=" + strings.Repeat("a", 32), "--offline=false"}},
		{name: "unlock missing token", args: []string{"unlock", "alice/demo", "--offline"}},
		{name: "unlock invalid token", args: []string{"unlock", "alice/demo", "--offline", "--token=guess"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"repository"}, test.args...)
			lookup := func(string) (string, bool) {
				t.Fatal("invalid arguments accessed environment configuration")
				return "", false
			}
			err := run(t.Context(), nil, args, lookup, io.Discard)
			if !errors.Is(err, errUsage) || exitCode(err) != 2 {
				t.Fatalf("run error = %v, code = %d; want usage error", err, exitCode(err))
			}
		})
	}
}

func TestWriteRepositoryResultPreservesPartialReport(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("conditional delete failed")
	result := app.RepositoryMaintenanceResult{
		Operation: "gc", Namespace: "alice", Repository: "demo", Error: wantErr.Error(),
		Report: repository.GCReport{Candidates: 2, Deleted: 1, DeletedBytes: 100},
	}
	var output bytes.Buffer
	if err := writeRepositoryResult(&output, result, wantErr); !errors.Is(err, wantErr) {
		t.Fatalf("writeRepositoryResult error = %v", err)
	}
	var decoded struct {
		Operation string              `json:"operation"`
		Error     string              `json:"error"`
		Report    repository.GCReport `json:"report"`
	}
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Operation != "gc" || decoded.Error != wantErr.Error() || decoded.Report.Deleted != 1 || decoded.Report.DeletedBytes != 100 {
		t.Fatalf("report = %+v", decoded)
	}
	writeErr := errors.New("output closed")
	err := writeRepositoryResult(failingWriter{err: writeErr}, result, wantErr)
	if !errors.Is(err, writeErr) || !errors.Is(err, wantErr) {
		t.Fatalf("lost operation or output error: %v", err)
	}
}

func TestRepositoryHelpWriteFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("output closed")
	err := run(t.Context(), nil, []string{"repository", "gc", "--help"}, nil, failingWriter{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("help error = %v", err)
	}
}

func TestExitCode(t *testing.T) {
	t.Parallel()
	if exitCode(nil) != 0 || exitCode(errUsage) != 2 || exitCode(errors.New("failed")) != 1 {
		t.Fatal("unexpected command exit code")
	}
}
