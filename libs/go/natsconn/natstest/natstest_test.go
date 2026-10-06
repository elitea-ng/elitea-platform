package natstest_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
)

// Every identity in the permission table connects; nothing else does.
func TestOnlyMappedCertificatesConnect(t *testing.T) {
	s := natstest.Start(t)
	for _, id := range []string{
		natsconn.IdentityMain, natsconn.IdentityGateway, natsconn.IdentityScheduler,
		natsconn.IdentityBootstrap, natsconn.IdentityWorker,
	} {
		if err := s.Probe(id); err != nil {
			t.Errorf("%s was refused: %v", id, err)
		}
	}
	refused := map[string]string{
		"":                         "no client certificate",
		natstest.IdentityNoSAN:     "a certificate from the NATS CA with CN=elitea-main and no URI SAN",
		natstest.IdentityUnknown:   "a URI SAN that names no user",
		natstest.IdentityForeignCA: "elitea-main's URI SAN signed by another CA",
	}
	for id, what := range refused {
		if err := s.Probe(id); err == nil {
			t.Errorf("the server accepted %s", what)
		}
	}
	if err := s.ProbePlain(); err == nil {
		t.Error("the server accepted a plaintext connection")
	}
}

// The bootstrap creates every asset as its own identity, re-running it is a
// no-op, and it puts a drifted setting back.
func TestBootstrapOwnsAndReconcilesTheAssets(t *testing.T) {
	s := natstest.Start(t)
	out := s.Bootstrap(t, nil)
	s.RequireNoViolations(t, natsconn.IdentityBootstrap)
	if !strings.Contains(out, "5 of 5 expected assertions") {
		t.Fatalf("bootstrap did not verify five assets:\n%s", out)
	}
	s.Bootstrap(t, nil) // idempotent

	// Drift: shorten the delta stream's retention, as the gateway's own
	// CreateOrUpdate used to on every boot. A re-run must restore it.
	cli := cliAs(t, s, natsconn.IdentityBootstrap)
	if out, err := cli("stream", "edit", "GATEWAY_BUDGET_DELTAS", "--max-msgs", "500000", "--force").CombinedOutput(); err != nil {
		t.Fatalf("drift the stream: %v\n%s", err, out)
	}
	s.Bootstrap(t, nil)
	raw, err := cli("stream", "info", "GATEWAY_BUDGET_DELTAS", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Config struct {
			MaxMsgs int64 `json:"max_msgs"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	if info.Config.MaxMsgs != 5000000 {
		t.Errorf("bootstrap left max_msgs=%d after drift, want 5000000", info.Config.MaxMsgs)
	}

	// Counter streams really are counters.
	for _, name := range []string{"GATEWAY_BUDGET", "GATEWAY_RATELIMIT"} {
		raw, err := cli("stream", "info", name, "--json").Output()
		if err != nil {
			t.Fatal(err)
		}
		var si struct {
			Config struct {
				AllowMsgCounter bool `json:"allow_msg_counter"`
			} `json:"config"`
		}
		if err := json.Unmarshal(raw, &si); err != nil {
			t.Fatal(err)
		}
		if !si.Config.AllowMsgCounter {
			t.Errorf("%s is not a counter stream", name)
		}
	}
}

// No service identity can run the bootstrap: creating a stream is the
// bootstrap's alone.
func TestServicesCannotCreateStreams(t *testing.T) {
	s := natstest.Start(t)
	for _, id := range []string{
		natsconn.IdentityMain, natsconn.IdentityGateway, natsconn.IdentityScheduler, natsconn.IdentityWorker,
	} {
		out, err := s.RunBootstrap(id, map[string]string{"NATS_ARGS": "--timeout 1s"})
		if err == nil {
			t.Errorf("bootstrap.sh succeeded as %s:\n%s", id, out)
		}
		// Refused by the server, not failed for some other reason. The CLI
		// reads stream info first, so the refusal can land before CREATE;
		// each service's own test asserts the CREATE refusal with nats.go.
		if len(s.Violations(id)) == 0 {
			t.Errorf("bootstrap.sh failed as %s but the server logged no permissions violation for it:\n%s", id, out)
		}
	}
}

// Nobody but the bootstrap may delete or purge a stream.
func TestOnlyTheBootstrapMayDeleteOrPurge(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	for _, id := range []string{
		natsconn.IdentityMain, natsconn.IdentityGateway, natsconn.IdentityScheduler, natsconn.IdentityWorker,
	} {
		cli := cliAs(t, s, id)
		for _, args := range [][]string{
			{"stream", "purge", "GATEWAY_BUDGET", "--force"},
			{"stream", "rm", "GATEWAY_BUDGET_DELTAS", "--force"},
			{"kv", "del", "ELITEA_CANVAS_PRESENCE", "--force"},
		} {
			if out, err := cli(args...).CombinedOutput(); err == nil {
				t.Errorf("%s ran `nats %s`:\n%s", id, strings.Join(args, " "), out)
			}
		}
	}
	// The CLI reads a stream's info before it purges or deletes, so for an
	// identity without INFO on that stream the refusal lands there. Assert the
	// destructive subjects directly for the identity that may read the info.
	s.RequireViolation(t, natsconn.IdentityGateway, "Publish", "$JS.API.STREAM.PURGE.GATEWAY_BUDGET")
	s.RequireViolation(t, natsconn.IdentityGateway, "Publish", "$JS.API.STREAM.DELETE.GATEWAY_BUDGET_DELTAS")
	s.RequireViolation(t, natsconn.IdentityMain, "Publish", "$JS.API.STREAM.DELETE.KV_ELITEA_CANVAS_PRESENCE")
	cli := cliAs(t, s, natsconn.IdentityBootstrap)
	if out, err := cli("stream", "purge", "GATEWAY_BUDGET", "--force").CombinedOutput(); err != nil {
		t.Errorf("the bootstrap identity could not purge: %v\n%s", err, out)
	}
}

func cliAs(t *testing.T, s *natstest.Server, identity string) func(args ...string) *exec.Cmd {
	t.Helper()
	m := s.Material(identity)
	return func(args ...string) *exec.Cmd {
		full := append([]string{
			"--server", s.URL(),
			"--tlsca", m.CAFile, "--tlscert", m.CertFile, "--tlskey", m.KeyFile,
			"--inbox-prefix", natsconn.InboxPrefix(identity),
			"--timeout", "1s",
		}, args...)
		cmd := exec.Command(natstest.CLI(), full...)
		cmd.Env = append(cmd.Environ(), "HOME="+s.Dir())
		return cmd
	}
}
