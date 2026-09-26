package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/define42/GitOneS3/internal/app"
	"github.com/define42/GitOneS3/internal/config"
)

const repositoryUsage = `Usage: gitone repository <operation> <namespace>/<repository> [flags]

Operations:
  check         Verify the current generation's digests and Git object graph.
  generations   List every retained immutable state snapshot.
  restore       Publish a verified retained snapshot as a new generation.
                Requires --snapshot <key> from generations.
  gc            Report orphan artifacts; --apply enables deletion.
                --grace-period <duration> defaults to 24h.
  repack        Publish the current object set as a canonical immutable pack.
  lock          Inspect the current durable writer/maintenance lock.
  unlock        Recover a terminated process's lock. Requires --token <token>
                and --offline after stopping ALL processes using the repository.

Flags may appear before or after the target. Targets are canonical names without
a .git suffix. Commands use the configured shard's environment, immutable cluster
identity and AWS credentials. They open no listeners and perform no OIDC discovery.
Output is JSON; failures return nonzero and can include a partial report.

Drain ALL older readers and writers before any new push or repack publishes
schema 2; mixed-version serving and rollback after packed publication are unsupported.
GC additionally requires every writer to honor this release's durable lock.
GC retains all generation snapshots and their objects, including deleted history.
See docs/repository-maintenance.md for rollout and recovery procedures.
`

func runRepositoryCommand(ctx context.Context, args []string, lookup config.LookupEnv, output io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: repository operation is required; use gitone repository --help", errUsage)
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := io.WriteString(output, repositoryUsage)
		return err
	}
	options, err := parseRepositoryCommand(args)
	if errors.Is(err, flag.ErrHelp) {
		_, err := io.WriteString(output, repositoryUsage)
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: %w; use gitone repository --help", errUsage, err)
	}
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}
	result, operationErr := app.MaintainRepository(ctx, cfg, options)
	return writeRepositoryResult(output, result, operationErr)
}

func writeRepositoryResult(output io.Writer, result app.RepositoryMaintenanceResult, operationErr error) error {
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return errors.Join(operationErr, fmt.Errorf("write repository report: %w", err))
	}
	return operationErr
}

func parseRepositoryCommand(args []string) (app.RepositoryMaintenanceOptions, error) {
	var options app.RepositoryMaintenanceOptions
	if len(args) == 0 {
		return options, errors.New("repository operation is required")
	}
	options.Operation = args[0]
	flags := flag.NewFlagSet("repository "+options.Operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	switch options.Operation {
	case "check", "generations", "repack", "lock":
	case "restore":
		flags.StringVar(&options.Snapshot, "snapshot", "", "exact retained state snapshot key")
	case "gc":
		flags.BoolVar(&options.Apply, "apply", false, "delete verified orphan candidates")
		flags.DurationVar(&options.GracePeriod, "grace-period", 0, "minimum orphan age (default 24h)")
	case "unlock":
		flags.StringVar(&options.Token, "token", "", "exact token reported by lock")
		flags.BoolVar(&options.Offline, "offline", false, "all processes using this repository are stopped")
	default:
		return options, fmt.Errorf("unknown repository operation %q", options.Operation)
	}
	ordered, err := repositoryFlagArgs(flags, args[1:])
	if err != nil {
		return options, err
	}
	if err := flags.Parse(ordered); err != nil {
		return options, err
	}
	if flags.NArg() != 1 {
		return options, errors.New("exactly one namespace/repository target is required")
	}
	var found bool
	options.Namespace, options.Repository, found = strings.Cut(flags.Arg(0), "/")
	if !found {
		return options, errors.New("target must be namespace/repository")
	}
	if err := options.Validate(); err != nil {
		return options, err
	}
	return options, nil
}

// repositoryFlagArgs supports flags on either side of the target while using
// the standard flag package's value parsing. Reject duplicate flags so a later
// occurrence cannot silently reverse an operator's requested action.
func repositoryFlagArgs(flags *flag.FlagSet, args []string) ([]string, error) {
	var options, targets []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			targets = append(targets, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			targets = append(targets, arg)
			continue
		}
		if arg == "--help" || arg == "-h" {
			return nil, flag.ErrHelp
		}
		name, _, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		definition := flags.Lookup(name)
		if definition == nil {
			return nil, fmt.Errorf("unknown flag --%s", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate flag --%s", name)
		}
		seen[name] = true
		options = append(options, arg)
		boolean, isBoolean := definition.Value.(interface{ IsBoolFlag() bool })
		if hasValue || isBoolean && boolean.IsBoolFlag() {
			continue
		}
		i++
		if i == len(args) {
			return nil, fmt.Errorf("flag --%s requires a value", name)
		}
		options = append(options, args[i])
	}
	options = append(options, "--")
	return append(options, targets...), nil
}
