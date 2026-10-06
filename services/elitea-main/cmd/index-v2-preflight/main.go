package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/cutover"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/commandbus"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runtimeNATSPrefix selects ELITEA_RUNTIME_NATS_TLS_{CA,CERT,KEY}_FILE, the
// runtime plane's NATS identity (elitea-main-runtime), the same names
// elitea-main reads.
const runtimeNATSPrefix = "ELITEA_RUNTIME"

const (
	exitReady        = 0
	exitBlocked      = 1
	exitInvalidUsage = 2
	preflightTimeout = 30 * time.Second
)

type stringList []string

type preflightRuntimeConfig struct {
	databaseURL   string
	commandStream string
	nats          runtimecomposition.NATSConnConfig
}

func (values *stringList) String() string {
	return ""
}

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	signalContext, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()
	os.Exit(run(signalContext, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

func run(
	parent context.Context,
	arguments []string,
	lookup runtimecomposition.LookupEnv,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if parent == nil || lookup == nil || stdout == nil || stderr == nil {
		return exitInvalidUsage
	}
	spoolRoots, ok := parseArguments(arguments)
	if !ok {
		writeMessage(stderr, "usage: index-v2-preflight --spool-root <absolute-private-directory> [--spool-root ...]\n")
		return exitInvalidUsage
	}
	runtimeConfig, err := preflightConfigFromEnv(lookup)
	if err != nil {
		writeMessage(stderr, "runtime index cutover configuration is invalid\n")
		return exitBlocked
	}

	ctx, cancel := context.WithTimeout(parent, preflightTimeout)
	defer cancel()

	poolConfig, err := pgxpool.ParseConfig(runtimeConfig.databaseURL)
	if err != nil {
		writeMessage(stderr, "runtime index cutover database configuration is invalid\n")
		return exitBlocked
	}
	poolConfig.MaxConns = 2
	poolConfig.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		writeMessage(stderr, "runtime index cutover database is unavailable\n")
		return exitBlocked
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		writeMessage(stderr, "runtime index cutover database is unavailable\n")
		return exitBlocked
	}

	// The preflight dials as elitea-main's runtime identity, whose grants
	// include the stream and consumer INFO it reads and nothing it could
	// change (deploy/helm/nats/values.yaml).
	natsConn, err := runtimecomposition.NewRuntimeNATSConn(runtimeConfig.nats, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		writeMessage(stderr, "runtime index cutover control store is unavailable\n")
		return exitBlocked
	}
	defer natsConn.Close()
	js, err := runtimecomposition.NewRuntimeJetStream(natsConn)
	if err != nil {
		writeMessage(stderr, "runtime index cutover control store is unavailable\n")
		return exitBlocked
	}

	persisted, err := repos.NewCurrentIndexV2CutoverRepository(pool)
	if err != nil {
		writeMessage(stderr, "runtime index cutover persisted-state reader is unavailable\n")
		return exitBlocked
	}
	control, err := commandbus.NewIndexV2CutoverReader(js, runtimeConfig.commandStream)
	if err != nil {
		writeMessage(stderr, "runtime index cutover control-state reader is unavailable\n")
		return exitBlocked
	}
	preflight, err := cutover.NewIndexV2Preflight(persisted, control, spoolRoots)
	if err != nil {
		writeMessage(stderr, "runtime index cutover spool coverage is invalid\n")
		return exitBlocked
	}
	report, err := preflight.Check(ctx)
	if err != nil {
		if errors.Is(err, cutover.ErrIndexV2CutoverBlocked) {
			if encodeErr := json.NewEncoder(stdout).Encode(report); encodeErr != nil {
				writeMessage(stderr, "runtime index cutover report could not be written\n")
				return exitBlocked
			}
			writeMessage(stderr, "runtime index capability v2 cutover is blocked\n")
			return exitBlocked
		}
		writeMessage(stderr, "runtime index cutover dependency check failed\n")
		return exitBlocked
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		writeMessage(stderr, "runtime index cutover report could not be written\n")
		return exitBlocked
	}
	return exitReady
}

func preflightConfigFromEnv(lookup runtimecomposition.LookupEnv) (preflightRuntimeConfig, error) {
	if lookup == nil {
		return preflightRuntimeConfig{}, errors.New("runtime environment lookup is required")
	}
	if enabled, _ := lookup("ELITEA_RUNTIME_ENABLED"); enabled != "true" {
		return preflightRuntimeConfig{}, errors.New("runtime must be enabled")
	}
	if enabled, _ := lookup("ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"); enabled != "true" {
		return preflightRuntimeConfig{}, errors.New("index ingest dispatch must be enabled")
	}
	required := func(name string) (string, error) {
		value, ok := lookup(name)
		if !ok || value == "" {
			return "", errors.New(name + " is required")
		}
		return value, nil
	}
	databaseURL, err := required("DATABASE_URL")
	if err != nil {
		return preflightRuntimeConfig{}, err
	}
	commandStream, err := required("ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM")
	if err != nil {
		return preflightRuntimeConfig{}, err
	}
	natsURL, err := required("ELITEA_RUNTIME_NATS_URL")
	if err != nil {
		return preflightRuntimeConfig{}, err
	}
	material, err := natsconn.FromEnv(runtimeNATSPrefix, func(name string) (string, bool) { return lookup(name) })
	if err != nil {
		return preflightRuntimeConfig{}, err
	}
	if err := material.CheckURL(natsURL); err != nil {
		return preflightRuntimeConfig{}, err
	}
	return preflightRuntimeConfig{
		databaseURL:   databaseURL,
		commandStream: commandStream,
		nats: runtimecomposition.NATSConnConfig{
			URL:      natsURL,
			Material: material,
			Name:     "elitea-main-runtime/index-v2-preflight",
		},
	}, nil
}

func parseArguments(arguments []string) ([]string, bool) {
	flags := flag.NewFlagSet("index-v2-preflight", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var roots stringList
	flags.Var(&roots, "spool-root", "absolute private output-spool root for one worker replica")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || len(roots) == 0 {
		return nil, false
	}
	return []string(roots), true
}

func writeMessage(writer io.Writer, message string) {
	_, _ = io.WriteString(writer, message)
}
