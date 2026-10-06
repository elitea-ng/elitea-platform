// Command natstest-serve runs the secured NATS test server — the NATS chart's
// own rendered nats.conf, the test PKI, and the real bootstrap.sh — for test
// suites that are not Go: the Rust and Python workers' command-bus
// integration tests run their real NATS code against the chart's permission
// table through it.
//
//	natstest-serve -out /path/env.json
//
// It reads ELITEA_TEST_NATS_SERVER_BIN, ELITEA_TEST_NATS_SECURE_CONF and
// ELITEA_TEST_NATS_CLI_BIN (scripts/nats/ci-secure-test-env.sh sets all
// three), starts the server, runs bootstrap.sh once per account as that
// account's bootstrap identity (as the Job does),
// writes the environment document below to -out (atomically, so a poller
// never reads half of it), and serves until SIGINT or SIGTERM.
//
//	{
//	  "url": "tls://127.0.0.1:<port>",
//	  "log": "<the server's log file>",
//	  "identities": {
//	    "elitea-worker": {"ca": "...", "cert": "...", "key": "...",
//	                      "inbox_prefix": "_INBOX_elitea-worker",
//	                      "user": "spiffe://elitea.internal/nats/elitea-worker"},
//	    ...
//	  }
//	}
//
// A test asserts that its identity hit no permissions violation by reading
// the log file for lines carrying `/user:<user>"` (the server prefixes the
// user with its account, `"RUNTIME/user:<user>"`) and ` - Publish
// Violation - ` or ` - Subscription Violation - `.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
)

type identity struct {
	CA          string `json:"ca"`
	Cert        string `json:"cert"`
	Key         string `json:"key"`
	InboxPrefix string `json:"inbox_prefix"`
	User        string `json:"user"`
}

type environment struct {
	URL        string              `json:"url"`
	Log        string              `json:"log"`
	Identities map[string]identity `json:"identities"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "natstest-serve:", err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", "", "write the environment document here")
	flag.Parse()
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	need := func(name string) (string, error) {
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("%s is not set (scripts/nats/ci-secure-test-env.sh sets it)", name)
		}
		return v, nil
	}
	bin, err := need(natstest.EnvServerBin)
	if err != nil {
		return err
	}
	confPath, err := need(natstest.EnvSecureConf)
	if err != nil {
		return err
	}
	cli, err := need(natstest.EnvCLIBin)
	if err != nil {
		return err
	}
	conf, err := os.ReadFile(confPath)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "natstest-serve-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	server, err := natstest.Launch(dir, bin, string(conf), cli)
	if err != nil {
		return err
	}
	defer server.Stop()
	if output, err := server.BootstrapAll(nil); err != nil {
		return fmt.Errorf("bootstrap.sh: %w\n%s", err, output)
	}

	env := environment{URL: server.URL(), Log: server.LogPath(), Identities: map[string]identity{}}
	for _, id := range natsconn.Identities() {
		m := server.Material(id)
		env.Identities[id] = identity{
			CA: m.CAFile, Cert: m.CertFile, Key: m.KeyFile,
			InboxPrefix: natsconn.InboxPrefix(id),
			User:        natsconn.IdentityURI(natsconn.DefaultTrustDomain, id),
		}
	}
	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(*out), "."+filepath.Base(*out)+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, *out); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "natstest-serve: serving %s; environment in %s\n", server.URL(), *out)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	_ = os.Remove(*out)
	return nil
}
