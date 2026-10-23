// Command elitea-admission-ctl shows and changes the persisted durable
// admission cap (elitea_runtime.execution_admission_policies) in one step
// (#966).
//
// Every elitea-main replica admits under its ELITEA_RUNTIME_MAX_OUTSTANDING
// and fails closed while that differs from the cap persisted for the
// capability. Changing the cap therefore takes two coordinated changes: the
// value on every replica, and the persisted row. This command is the second
// half. Run inside an elitea-main pod after the rollout, `set` with no -max
// takes that replica's own ELITEA_RUNTIME_MAX_OUTSTANDING, so the persisted
// cap follows the value the replicas were just given:
//
//	kubectl exec deploy/<release>-main -- /elitea-admission-ctl set
//	elitea-admission-ctl show
//	elitea-admission-ctl set -max 2048 [-capability agent.execute.application.v1]
//
// -capability changes a capability that already has a persisted cap and
// exits non-zero when none does: a typo must not mint a policy row for a
// capability nothing admits. It reads DATABASE_URL, as elitea-migrate does.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const (
	exitOK           = 0
	exitFailed       = 1
	exitInvalidUsage = 2
	commandTimeout   = 30 * time.Second
)

type lookupEnv func(string) (string, bool)

// policyStore is the database half, behind an interface so the argument and
// output handling is testable without PostgreSQL.
type policyStore interface {
	List(ctx context.Context) ([]repos.AdmissionPolicy, error)
	Set(ctx context.Context, capabilityID string, maxOutstanding int64) (int64, error)
	Close()
}

type openStore func(ctx context.Context, databaseURL string) (policyStore, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr, openPostgresStore))
}

func run(parent context.Context, arguments []string, lookup lookupEnv, stdout, stderr io.Writer, open openStore) int {
	if len(arguments) == 0 {
		usage(stderr)
		return exitInvalidUsage
	}
	command, rest := arguments[0], arguments[1:]
	flags := flag.NewFlagSet("elitea-admission-ctl "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	maxOutstanding := flags.Int64("max", 0, "the new cap (default: this environment's ELITEA_RUNTIME_MAX_OUTSTANDING)")
	capabilityID := flags.String("capability", "", "change one capability only (default: every persisted capability)")
	switch command {
	case "show", "set":
	default:
		usage(stderr)
		return exitInvalidUsage
	}
	if err := flags.Parse(rest); err != nil || flags.NArg() != 0 {
		usage(stderr)
		return exitInvalidUsage
	}
	if command == "show" && (*maxOutstanding != 0 || *capabilityID != "") {
		usage(stderr)
		return exitInvalidUsage
	}
	if command == "set" && *maxOutstanding == 0 {
		raw, ok := lookup("ELITEA_RUNTIME_MAX_OUTSTANDING")
		if !ok || raw == "" {
			sayln(stderr, "elitea-admission-ctl: set needs -max, or ELITEA_RUNTIME_MAX_OUTSTANDING in the environment")
			return exitInvalidUsage
		}
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			say(stderr, "elitea-admission-ctl: ELITEA_RUNTIME_MAX_OUTSTANDING=%q is not an integer\n", raw)
			return exitInvalidUsage
		}
		*maxOutstanding = parsed
	}
	if command == "set" && (*maxOutstanding < 1 || *maxOutstanding > repos.MaxSupportedOutstandingJobs) {
		say(stderr, "elitea-admission-ctl: cap %d is outside 1..%d\n", *maxOutstanding, repos.MaxSupportedOutstandingJobs)
		return exitInvalidUsage
	}
	databaseURL, ok := lookup("DATABASE_URL")
	if !ok || databaseURL == "" {
		sayln(stderr, "elitea-admission-ctl: DATABASE_URL is required")
		return exitInvalidUsage
	}

	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	store, err := open(ctx, databaseURL)
	if err != nil {
		say(stderr, "elitea-admission-ctl: open database: %v\n", err)
		return exitFailed
	}
	defer store.Close()

	if command == "set" {
		written, err := store.Set(ctx, *capabilityID, *maxOutstanding)
		if err != nil {
			say(stderr, "elitea-admission-ctl: %v\n", err)
			return exitFailed
		}
		if *capabilityID != "" && written == 0 {
			say(stderr, "elitea-admission-ctl: capability %q has no persisted admission cap; `show` lists the ones that do\n", *capabilityID)
			return exitFailed
		}
		scope := "every persisted capability"
		if *capabilityID != "" {
			scope = fmt.Sprintf("capability %q", *capabilityID)
		}
		say(stdout, "set the admission cap of %s to %d (%d row(s))\n", scope, *maxOutstanding, written)
	}
	policies, err := store.List(ctx)
	if err != nil {
		say(stderr, "elitea-admission-ctl: %v\n", err)
		return exitFailed
	}
	if len(policies) == 0 {
		sayln(stdout, "no capability has a persisted admission cap yet")
	}
	for _, policy := range policies {
		say(stdout, "%s\t%d\n", policy.CapabilityID, policy.MaxOutstanding)
	}
	return exitOK
}

// say and sayln write the command's output. A failed write to the
// terminal has nowhere better to be reported, so its error is dropped here,
// once, instead of at every call.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func sayln(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}

func usage(w io.Writer) {
	say(w, `usage:
  elitea-admission-ctl show
  elitea-admission-ctl set [-max N] [-capability ID]

set writes N (1..%d; default: ELITEA_RUNTIME_MAX_OUTSTANDING) as the persisted
admission cap of every persisted capability, or of -capability only (which
must already have one), then prints every persisted cap. Set the same value on every elitea-main replica
first: a replica whose value differs fails every start closed.
`, repos.MaxSupportedOutstandingJobs)
}

type postgresStore struct {
	pool *pgxpool.Pool
}

func openPostgresStore(ctx context.Context, databaseURL string) (policyStore, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("DATABASE_URL is invalid")
	}
	config.MaxConns = 1
	config.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return postgresStore{pool: pool}, nil
}

func (s postgresStore) List(ctx context.Context) ([]repos.AdmissionPolicy, error) {
	return repos.ListAdmissionPolicies(ctx, s.pool)
}

func (s postgresStore) Set(ctx context.Context, capabilityID string, maxOutstanding int64) (int64, error) {
	return repos.SetAdmissionPolicyMax(ctx, s.pool, capabilityID, maxOutstanding)
}

func (s postgresStore) Close() {
	s.pool.Close()
}
