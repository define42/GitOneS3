package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/s3check"
)

func TestRunHelpWithoutConfiguration(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"--help", "-h"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var output, diagnostics bytes.Buffer
			if err := run(t.Context(), []string{name}, &output, &diagnostics); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"--endpoint", "--bucket", "AWS_PROFILE", "-list-api", "-objects"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("help does not contain %q: %s", want, output.String())
				}
			}
			if diagnostics.Len() != 0 {
				t.Fatalf("help produced diagnostics: %s", diagnostics.String())
			}
		})
	}
}

func TestRunRejectsArgumentsBeforeConfiguration(t *testing.T) {
	t.Parallel()
	valid := []string{"--endpoint", "https://s3.example.test", "--bucket", "s3-test-bucket"}
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing arguments", args: []string{}},
		{name: "missing bucket", args: []string{"--endpoint", "https://s3.example.test"}},
		{name: "unknown flag", args: []string{"--unknown"}},
		{name: "positional argument", args: append(append([]string{}, valid...), "extra")},
		{name: "invalid API", args: append(append([]string{}, valid...), "--list-api", "v3")},
		{name: "zero timeout", args: append(append([]string{}, valid...), "--timeout", "0s")},
		{name: "negative timeout", args: append(append([]string{}, valid...), "--timeout", "-1s")},
		{name: "too few objects", args: append(append([]string{}, valid...), "--objects", "1000")},
		{name: "too many objects", args: append(append([]string{}, valid...), "--objects", "10001")},
		{name: "empty region", args: append(append([]string{}, valid...), "--region", "")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output, diagnostics bytes.Buffer
			err := run(t.Context(), test.args, &output, &diagnostics)
			if !errors.Is(err, errUsage) || exitCode(err) != 2 {
				t.Fatalf("run() error = %v, exit = %d, want usage error", err, exitCode(err))
			}
			if output.Len() != 0 || diagnostics.Len() != 0 {
				t.Fatalf("invalid arguments produced output: %q / %q", output.String(), diagnostics.String())
			}
		})
	}
}

func TestParseOptions(t *testing.T) {
	t.Parallel()
	opts, err := parseOptions([]string{
		"--endpoint", "https://nyc3.digitaloceanspaces.com",
		"--bucket", "s3-test-bucket", "--region", "nyc3",
		"--list-api", "v1", "--path-style", "--json", "--timeout", "3m", "--objects", "2505",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.check.Bucket != "s3-test-bucket" || opts.check.Prefix != "gitone-s3check/" {
		t.Fatalf("unexpected check options: %+v", opts.check)
	}
	if opts.check.ListAPI != "v1" || opts.check.Objects != 2505 {
		t.Fatalf("unexpected listing options: %+v", opts.check)
	}
	if opts.region != "nyc3" || opts.timeout != 3*time.Minute {
		t.Fatalf("unexpected region or timeout: %+v", opts)
	}
	if !opts.pathStyle || !opts.json {
		t.Fatalf("boolean flags were ignored: %+v", opts)
	}
}

func TestValidateEndpoint(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value string
		valid bool
	}{
		{name: "HTTPS", value: "https://s3.example.test", valid: true},
		{name: "trailing slash", value: "https://s3.example.test/", valid: true},
		{name: "HTTP with port", value: "http://127.0.0.1:9000", valid: true},
		{name: "IPv6", value: "http://[::1]:9000", valid: true},
		{name: "empty", value: ""},
		{name: "missing scheme", value: "s3.example.test"},
		{name: "wrong scheme", value: "file:///tmp/bucket"},
		{name: "missing host", value: "https:///"},
		{name: "missing IPv4 host", value: "http://:9000"},
		{name: "bucket in path", value: "https://s3.example.test/bucket"},
		{name: "encoded slash", value: "https://s3.example.test/%2f"},
		{name: "credentials", value: "https://access:secret@s3.example.test"}, //nolint:gosec // Invalid URL fixture verifies credentials are rejected.
		{name: "query", value: "https://s3.example.test?token=secret"},
		{name: "empty query", value: "https://s3.example.test?"},
		{name: "fragment", value: "https://s3.example.test#fragment"},
		{name: "empty fragment", value: "https://s3.example.test#"},
		{name: "invalid escaping", value: "https://s3.example.test/%zz"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateEndpoint(test.value)
			if (err == nil) != test.valid {
				t.Fatalf("validateEndpoint(%q) = %v, valid = %v", test.value, err, test.valid)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("validation disclosed endpoint credentials: %v", err)
			}
		})
	}
}

func TestWriteRunStart(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if err := writeRunStart(&output, "test-bucket", "gitone-s3check/random-run/"); err != nil {
		t.Fatal(err)
	}
	want := "Test location: s3://test-bucket/gitone-s3check/random-run/\n"
	if output.String() != want {
		t.Fatalf("start diagnostics = %q, want %q", output.String(), want)
	}
	wantErr := errors.New("diagnostics closed")
	err := writeRunStart(failingWriter{err: wantErr}, "test-bucket", "gitone-s3check/random-run/")
	if !errors.Is(err, wantErr) {
		t.Fatalf("start diagnostics error = %v, want %v", err, wantErr)
	}
}

func TestWriteReport(t *testing.T) {
	t.Parallel()
	report := s3check.Report{
		Bucket: "test-bucket", Prefix: "gitone-s3check/random-run/", ListAPI: "v1", Passed: false,
		Results: []s3check.Result{
			{Name: "conditional-write", Status: "pass", Detail: "stale ETag rejected", Duration: time.Second},
			{Name: "conditional-delete", Status: "fail", Detail: "condition ignored", Duration: time.Second},
			{Name: "list-v2", Status: "skip", Detail: "not selected"},
		},
	}
	t.Run("JSON includes failed checks", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		if err := writeReport(&output, report, true); err != nil {
			t.Fatal(err)
		}
		var got s3check.Report
		decoder := json.NewDecoder(&output)
		if err := decoder.Decode(&got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, report) {
			t.Fatalf("JSON report = %+v, want %+v", got, report)
		}
		if err := decoder.Decode(&got); !errors.Is(err, io.EOF) {
			t.Fatalf("unexpected output after JSON report: %v", err)
		}
	})
	t.Run("plain includes context and limitations", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		if err := writeReport(&output, report, false); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"test-bucket", "gitone-s3check/random-run/", "PASS  conditional-write", "FAIL  conditional-delete",
			"SKIP  list-v2", "Selected checks did not all pass", "provider consistency", "currently uses V2",
		} {
			if !strings.Contains(output.String(), want) {
				t.Errorf("report missing %q: %s", want, output.String())
			}
		}
	})
	t.Run("passed selection", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		passed := report
		passed.Passed = true
		passed.Results = report.Results[:1]
		if err := writeReport(&output, passed, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "Selected checks passed.") {
			t.Fatalf("missing passing summary: %s", output.String())
		}
	})
}

func TestOutputErrors(t *testing.T) {
	t.Parallel()
	want := errors.New("output closed")
	writer := failingWriter{err: want}
	if err := run(t.Context(), []string{"--help"}, writer, io.Discard); !errors.Is(err, want) {
		t.Fatalf("help error = %v, want %v", err, want)
	}
	for _, asJSON := range []bool{false, true} {
		if err := writeReport(writer, s3check.Report{}, asJSON); !errors.Is(err, want) {
			t.Fatalf("writeReport(json=%v) error = %v, want %v", asJSON, err, want)
		}
	}
}

func TestExitCode(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "success", want: 0},
		{name: "usage", err: errUsage, want: 2},
		{name: "probe failure", err: errors.New("probe failed"), want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCode(test.err); got != test.want {
				t.Fatalf("exitCode(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }
