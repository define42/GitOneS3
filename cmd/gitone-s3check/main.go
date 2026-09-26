// Command gitone-s3check probes an existing S3 bucket for GitOne storage requirements.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/define42/GitOneS3/internal/s3check"
)

var errUsage = errors.New("invalid arguments")

const usage = `Usage: gitone-s3check --endpoint URL --bucket NAME [options]

Probe an existing, never-versioned S3 bucket for GitOne storage requirements.
Creates a random subprefix, uploads about 10 MiB plus small listing objects,
then deletes its known objects and aborts its multipart uploads. Cleanup runs
after cancellation too. No bucket configuration or GitOne data is changed.

Credentials use the normal AWS SDK provider chain, including AWS_PROFILE,
AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and AWS_SESSION_TOKEN. The command
does not load GitOne configuration or start a server.

Exit codes: 0 = selected checks passed; 1 = check/runtime/cleanup failure;
2 = invalid arguments. A passing run observes behavior; it cannot certify a
provider's consistency guarantees. GitOne currently requires V2 pagination,
even if the V1 checks pass. See docs/s3-compatibility.md.

Options:
`

type options struct {
	check     s3check.Options
	endpoint  string
	region    string
	pathStyle bool
	timeout   time.Duration
	json      bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(exitCode(err))
}

func run(ctx context.Context, args []string, output, diagnostics io.Writer) error {
	opts, err := parseOptions(args, output)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	awsConfig, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(opts.region),
		config.WithRetryMaxAttempts(1),
	)
	if err != nil {
		return fmt.Errorf("load AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(awsConfig, func(cfg *s3.Options) {
		cfg.BaseEndpoint = aws.String(opts.endpoint)
		cfg.UsePathStyle = opts.pathStyle
	})
	if _, err := io.WriteString(diagnostics,
		"Running S3 compatibility checks; temporary test objects will be created and cleaned up.\n",
	); err != nil {
		return err
	}
	opts.check.OnStart = func(prefix string) error {
		return writeRunStart(diagnostics, opts.check.Bucket, prefix)
	}
	report, runErr := s3check.Run(ctx, client, opts.check)
	outputErr := writeReport(output, report, opts.json)
	if runErr == nil && !report.Passed {
		runErr = errors.New("one or more selected checks did not pass")
	}
	return errors.Join(runErr, outputErr)
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var opts options
	flags := flag.NewFlagSet("gitone-s3check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.endpoint, "endpoint", "", "S3 endpoint origin, including http:// or https:// (required)")
	flags.StringVar(&opts.check.Bucket, "bucket", "", "existing bucket with versioning never enabled (required)")
	flags.StringVar(&opts.region, "region", "us-east-1", "region used to sign requests")
	flags.BoolVar(&opts.pathStyle, "path-style", false, "use path-style bucket addressing")
	flags.StringVar(&opts.check.Prefix, "prefix", "gitone-s3check/", "parent prefix for a random isolated test run")
	flags.StringVar(&opts.check.ListAPI, "list-api", "both", "listing checks to run: both, v1, or v2")
	flags.IntVar(&opts.check.Objects, "objects", 1005, "number of tiny listing objects (1001 to 10000)")
	flags.DurationVar(&opts.timeout, "timeout", 10*time.Minute, "check timeout; cleanup has its own deadline")
	flags.BoolVar(&opts.json, "json", false, "write a JSON report to stdout")
	flags.Usage = func() {}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			var help strings.Builder
			help.WriteString(usage)
			flags.SetOutput(&help)
			flags.PrintDefaults()
			if _, err := io.WriteString(output, help.String()); err != nil {
				return opts, err
			}
			return opts, flag.ErrHelp
		}
		return opts, fmt.Errorf("%w: %w; use --help", errUsage, err)
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("%w: positional arguments are not supported; use --help", errUsage)
	}
	if err := validateEndpoint(opts.endpoint); err != nil {
		return opts, fmt.Errorf("%w: %w", errUsage, err)
	}
	if strings.TrimSpace(opts.region) == "" {
		return opts, fmt.Errorf("%w: --region must not be empty", errUsage)
	}
	if opts.timeout <= 0 {
		return opts, fmt.Errorf("%w: --timeout must be positive", errUsage)
	}
	if err := opts.check.Validate(); err != nil {
		return opts, fmt.Errorf("%w: %w", errUsage, err)
	}
	return opts, nil
}

func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("--endpoint must be a valid HTTP or HTTPS origin")
	}
	validScheme := u.Scheme == "http" || u.Scheme == "https"
	validHost := u.Host != "" && u.Hostname() != ""
	validPath := u.Path == "" || u.Path == "/"
	if !validScheme || !validHost || !validPath {
		return errors.New("--endpoint must be an HTTP or HTTPS origin without a path")
	}
	hasQuery := u.RawQuery != "" || u.ForceQuery
	hasExtras := u.User != nil || hasQuery || strings.Contains(endpoint, "#")
	if hasExtras || u.RawPath != "" || u.Opaque != "" {
		return errors.New("--endpoint must not contain credentials, query parameters, or fragments")
	}
	return nil
}

func writeRunStart(diagnostics io.Writer, bucket, prefix string) error {
	_, err := fmt.Fprintf(diagnostics, "Test location: s3://%s/%s\n", bucket, prefix)
	return err
}

func writeReport(output io.Writer, report s3check.Report, asJSON bool) error {
	if asJSON {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	var result strings.Builder
	fmt.Fprintf(&result, "Bucket: %s\nTest prefix: %s\nListing checks: %s\n\n",
		report.Bucket, report.Prefix, report.ListAPI,
	)
	for _, check := range report.Results {
		fmt.Fprintf(&result, "%s  %s (%s)\n", strings.ToUpper(check.Status), check.Name, check.Duration.Round(time.Millisecond))
		if check.Detail != "" {
			fmt.Fprintf(&result, "      %s\n", check.Detail)
		}
	}
	if report.Passed {
		result.WriteString("\nSelected checks passed.\n")
	} else {
		result.WriteString("\nSelected checks did not all pass.\n")
	}
	result.WriteString("Observed behavior only; verify provider consistency guarantees before production use.\n")
	if report.ListAPI == "v1" {
		result.WriteString("GitOne currently uses V2 pagination; passing V1 checks does not enable V1 in production.\n")
	}
	_, err := io.WriteString(output, result.String())
	return err
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, errUsage) {
		return 2
	}
	return 1
}
