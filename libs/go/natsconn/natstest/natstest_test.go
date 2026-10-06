package natstest_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
)

// Every identity in the permission table connects; nothing else does.
func TestOnlyMappedCertificatesConnect(t *testing.T) {
	s := natstest.Start(t)
	for _, id := range natsconn.Identities() {
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

// Each identity lands in the account the permission table declares it in.
func TestEveryIdentityIsInItsPlanesAccount(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	for _, id := range natsconn.Identities() {
		got, err := s.Account(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if want := natsconn.AccountOf(id); got != want {
			t.Errorf("%s is in account %q, want %q", id, got, want)
		}
	}
	// The bootstrap of one plane cannot see another plane's streams: the
	// stream does not exist in its account (and it holds no grant on it).
	if out, err := cliAs(t, s, natsconn.IdentityBootstrapMain)("stream", "info", "GATEWAY_BUDGET").CombinedOutput(); err == nil {
		t.Errorf("MAIN's bootstrap read GATEWAY's counter stream:\n%s", out)
	}
	if out, err := cliAs(t, s, natsconn.IdentityBootstrapGateway)("stream", "info", "KV_ELITEA_CANVAS_PRESENCE").CombinedOutput(); err == nil {
		t.Errorf("GATEWAY's bootstrap read MAIN's presence bucket:\n%s", out)
	}
	if out, err := cliAs(t, s, natsconn.IdentityBootstrapRuntime)("stream", "info", "GATEWAY_BUDGET").CombinedOutput(); err == nil {
		t.Errorf("RUNTIME's bootstrap read GATEWAY's counter stream:\n%s", out)
	}
	if out, err := cliAs(t, s, natsconn.IdentityBootstrapGateway)("stream", "info", "ELITEA_RT_V1_AGENT").CombinedOutput(); err == nil {
		t.Errorf("GATEWAY's bootstrap read RUNTIME's agent command stream:\n%s", out)
	}
}

// The bootstrap creates every asset as each account's own identity,
// re-running it is a no-op, and it puts a drifted setting back.
func TestBootstrapOwnsAndReconcilesTheAssets(t *testing.T) {
	s := natstest.Start(t)
	out := s.Bootstrap(t, nil) // also asserts no bootstrap identity hit a violation
	if !strings.Contains(out, "13 of 13 expected assertions") {
		t.Fatalf("bootstrap did not verify thirteen assets:\n%s", out)
	}
	for _, acct := range []string{"main", "gateway", "runtime"} {
		if !strings.Contains(out, "account "+acct+": reconciling") {
			t.Errorf("bootstrap never connected to account %s:\n%s", acct, out)
		}
	}
	s.Bootstrap(t, nil) // idempotent

	// Drift: shorten the delta stream's retention, as the gateway's own
	// CreateOrUpdate used to on every boot. A re-run must restore it.
	cli := cliAs(t, s, natsconn.IdentityBootstrapGateway)
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

	// The scheduler's consumer is the bootstrap's too, and a drift is put
	// back the same way.
	if out, err := cli("consumer", "edit", "GATEWAY_BUDGET_DELTAS", "budget-writeback", "--max-deliver", "3", "--force").CombinedOutput(); err != nil {
		t.Fatalf("drift the consumer: %v\n%s", err, out)
	}
	s.Bootstrap(t, nil)
	raw, err = cli("consumer", "info", "GATEWAY_BUDGET_DELTAS", "budget-writeback", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var ci struct {
		Config struct {
			MaxDeliver     int    `json:"max_deliver"`
			AckPolicy      string `json:"ack_policy"`
			DeliverSubject string `json:"deliver_subject"`
			FilterSubject  string `json:"filter_subject"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &ci); err != nil {
		t.Fatal(err)
	}
	if ci.Config.MaxDeliver != 10 || ci.Config.AckPolicy != "explicit" || ci.Config.DeliverSubject != "" || ci.Config.FilterSubject != "gateway.budget.delta" {
		t.Errorf("budget-writeback after the bootstrap = %+v; want a pull consumer on gateway.budget.delta, explicit acks, max_deliver 10", ci.Config)
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
		natsconn.IdentityMain, natsconn.IdentityGateway, natsconn.IdentityScheduler,
		natsconn.IdentityMainRuntime, natsconn.IdentityWorker,
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

// Nobody deletes or purges a stream: not the services, and not the
// bootstraps either (bootstrap.sh never does, so no identity holds the grant).
func TestNobodyMayDeleteOrPurge(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	for _, id := range natsconn.Identities() {
		cli := cliAs(t, s, id)
		for _, args := range [][]string{
			{"stream", "purge", "GATEWAY_BUDGET", "--force"},
			{"stream", "rm", "GATEWAY_BUDGET_DELTAS", "--force"},
			{"kv", "del", "ELITEA_CANVAS_PRESENCE", "--force"},
			{"stream", "purge", "ELITEA_RT_V1_AGENT", "--force"},
			{"stream", "rm", "ELITEA_RT_V1_INDEX", "--force"},
			{"kv", "del", "ELITEA_RT_V1_DEADLETTER", "--force"},
			{"consumer", "rm", "ELITEA_RT_V1_AGENT", "elitea-agent-worker-v1", "--force"},
		} {
			if out, err := cli(args...).CombinedOutput(); err == nil {
				t.Errorf("%s ran `nats %s`:\n%s", id, strings.Join(args, " "), out)
			}
		}
	}
	// The CLI reads a stream's info before it purges or deletes, so for an
	// identity without INFO on that stream (or without the stream in its
	// account) the refusal lands there. Assert the destructive subjects
	// directly for the identities that may read the info.
	s.RequireViolation(t, natsconn.IdentityGateway, "Publish", "$JS.API.STREAM.PURGE.GATEWAY_BUDGET")
	s.RequireViolation(t, natsconn.IdentityGateway, "Publish", "$JS.API.STREAM.DELETE.GATEWAY_BUDGET_DELTAS")
	s.RequireViolation(t, natsconn.IdentityBootstrapGateway, "Publish", "$JS.API.STREAM.PURGE.GATEWAY_BUDGET")
	s.RequireViolation(t, natsconn.IdentityBootstrapGateway, "Publish", "$JS.API.STREAM.DELETE.GATEWAY_BUDGET_DELTAS")
	s.RequireViolation(t, natsconn.IdentityMain, "Publish", "$JS.API.STREAM.DELETE.KV_ELITEA_CANVAS_PRESENCE")
	s.RequireViolation(t, natsconn.IdentityBootstrapMain, "Publish", "$JS.API.STREAM.DELETE.KV_ELITEA_CANVAS_PRESENCE")
	for _, id := range []string{natsconn.IdentityMainRuntime, natsconn.IdentityWorker, natsconn.IdentityBootstrapRuntime} {
		s.RequireViolation(t, id, "Publish", "$JS.API.STREAM.PURGE.ELITEA_RT_V1_AGENT")
		s.RequireViolation(t, id, "Publish", "$JS.API.STREAM.DELETE.ELITEA_RT_V1_INDEX")
		s.RequireViolation(t, id, "Publish", "$JS.API.CONSUMER.DELETE.ELITEA_RT_V1_AGENT.elitea-agent-worker-v1")
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

// The cluster's routes have their own identity (#1076 F5): a peer presenting
// a route certificate from the ROUTE CA joins; a certificate from the CLIENT
// CA — any service's NATS identity — is refused on the route port, though it
// is a valid TLS client and server certificate from a CA the deployment
// trusts for clients. Run on the HA profile's own cluster block.
func TestRoutesAcceptOnlyRouteCertificates(t *testing.T) {
	c := natstest.StartCluster(t)
	if n := c.Routes(t); n != 0 {
		t.Fatalf("a fresh node has %d routes", n)
	}

	c.Peer(t, natstest.RouteFromClientCA)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.Log(), "TLS route handshake error") && !strings.Contains(c.Log(), "TLS handshake error") {
		if time.Now().After(deadline) {
			t.Fatalf("the node logged no TLS refusal for a client-CA certificate on the route port:\n%s", c.Log())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := c.Routes(t); n != 0 {
		t.Fatalf("a certificate from the client CA joined the cluster as a route (%d routes):\n%s", n, c.Log())
	}

	// The control: a real route certificate does join (as a route pool, so
	// one or more route connections), so the refusal above is about the CA,
	// not a broken cluster block.
	c.Peer(t, natstest.RouteIdentity)
	deadline = time.Now().Add(10 * time.Second)
	for c.Routes(t) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("a route certificate from the route CA did not join (routes=%d):\n%s\npeer:\n%s", c.Routes(t), c.Log(), c.LogOf("peer-"+natstest.RouteIdentity))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The runtime command bus's streams and consumers are the contract's shape
// (docs/runtime-command-bus.md), and only an ack removes a command.
func TestBootstrapCreatesTheCommandBus(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	cli := cliAs(t, s, natsconn.IdentityBootstrapRuntime)
	for stream, want := range map[string]struct {
		route, durable string
		maxAge         int64
	}{
		"ELITEA_RT_V1_VALIDATE": {"validate", "elitea-configuration-worker-v1", int64(3 * 3600e9)},
		"ELITEA_RT_V1_AGENT":    {"agent", "elitea-agent-worker-v1", int64(26 * 3600e9)},
		"ELITEA_RT_V1_INDEX":    {"index", "elitea-index-worker-v1", int64(26 * 3600e9)},
	} {
		raw, err := cli("stream", "info", stream, "--json").Output()
		if err != nil {
			t.Fatalf("stream info %s: %v", stream, err)
		}
		var si struct {
			Config struct {
				Subjects             []string `json:"subjects"`
				Retention            string   `json:"retention"`
				Discard              string   `json:"discard"`
				DiscardNewPerSubject bool     `json:"discard_new_per_subject"`
				MaxMsgsPerSubject    int64    `json:"max_msgs_per_subject"`
				MaxMsgs              int64    `json:"max_msgs"`
				MaxBytes             int64    `json:"max_bytes"`
				MaxMsgSize           int64    `json:"max_msg_size"`
				MaxAge               int64    `json:"max_age"`
				Duplicates           int64    `json:"duplicate_window"`
				AllowDirect          bool     `json:"allow_direct"`
				DenyDelete           bool     `json:"deny_delete"`
				DenyPurge            bool     `json:"deny_purge"`
				Storage              string   `json:"storage"`
			} `json:"config"`
		}
		if err := json.Unmarshal(raw, &si); err != nil {
			t.Fatal(err)
		}
		c := si.Config
		if len(c.Subjects) != 1 || c.Subjects[0] != "elitea.rt.v1."+want.route+".d.*" || c.Retention != "workqueue" ||
			c.Discard != "new" || !c.DiscardNewPerSubject || c.MaxMsgsPerSubject != 1 || c.MaxMsgs != 1024 ||
			c.MaxBytes != 64<<20 || c.MaxMsgSize != 65536 || c.MaxAge != want.maxAge || c.Duplicates != int64(120e9) ||
			!c.AllowDirect || !c.DenyDelete || !c.DenyPurge || c.Storage != "file" {
			t.Errorf("%s is not the contract's shape: %+v", stream, c)
		}
		raw, err = cli("consumer", "info", stream, want.durable, "--json").Output()
		if err != nil {
			t.Fatalf("consumer info %s/%s: %v", stream, want.durable, err)
		}
		var ci struct {
			Config struct {
				Durable       string `json:"durable_name"`
				Filter        string `json:"filter_subject"`
				AckPolicy     string `json:"ack_policy"`
				AckWait       int64  `json:"ack_wait"`
				MaxDeliver    int64  `json:"max_deliver"`
				MaxAckPending int64  `json:"max_ack_pending"`
				MaxWaiting    int64  `json:"max_waiting"`
				MaxBatch      int64  `json:"max_batch"`
				MaxExpires    int64  `json:"max_expires"`
				DeliverPolicy string `json:"deliver_policy"`
			} `json:"config"`
		}
		if err := json.Unmarshal(raw, &ci); err != nil {
			t.Fatal(err)
		}
		cc := ci.Config
		if cc.Durable != want.durable || cc.Filter != "elitea.rt.v1."+want.route+".d.*" || cc.AckPolicy != "explicit" ||
			cc.AckWait != int64(60e9) || cc.MaxDeliver != -1 || cc.MaxAckPending != 1024 || cc.MaxWaiting != 512 ||
			cc.MaxBatch != 64 || cc.MaxExpires != int64(30e9) || cc.DeliverPolicy != "all" {
			t.Errorf("%s/%s is not the contract's consumer: %+v", stream, want.durable, cc)
		}
	}
	if out, err := cli("stream", "info", "KV_ELITEA_RT_V1_DEADLETTER", "--json").CombinedOutput(); err != nil {
		t.Fatalf("dead-letter bucket: %v\n%s", err, out)
	} else if !strings.Contains(string(out), `"max_age": 604800000000000`) {
		t.Errorf("the dead-letter bucket's TTL is not 7 days:\n%s", out)
	}
	s.RequireNoViolations(t, natsconn.IdentityBootstrapRuntime)
}

// The command bus's two identities each do their own half only: the producer
// publishes a command and cannot pull one; the worker pulls, acks and records
// a dead letter, and cannot publish a command. (The services' own tests run
// their real client code on the same table; this pins the shape with the CLI.)
func TestCommandBusIdentitiesDoTheirOwnHalfOnly(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	main := cliAs(t, s, natsconn.IdentityMainRuntime)
	worker := cliAs(t, s, natsconn.IdentityWorker)
	subject := "elitea.rt.v1.agent.d.ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164"

	if out, err := main("pub", "--jetstream", subject, "body").CombinedOutput(); err != nil {
		t.Fatalf("the producer could not publish a command: %v\n%s", err, out)
	}
	if out, err := worker("pub", "--jetstream", subject, "forged").CombinedOutput(); err == nil {
		t.Errorf("the worker published a command:\n%s", out)
	}
	s.RequireViolation(t, natsconn.IdentityWorker, "Publish", subject)
	if out, err := main("consumer", "next", "ELITEA_RT_V1_AGENT", "elitea-agent-worker-v1", "--count", "1").CombinedOutput(); err == nil {
		t.Errorf("the producer pulled a command:\n%s", out)
	}
	s.RequireViolation(t, natsconn.IdentityMainRuntime, "Publish", "$JS.API.CONSUMER.MSG.NEXT.ELITEA_RT_V1_AGENT.elitea-agent-worker-v1")
	if out, err := worker("consumer", "next", "ELITEA_RT_V1_AGENT", "elitea-agent-worker-v1", "--count", "1", "--ack").CombinedOutput(); err != nil || !strings.Contains(string(out), "body") {
		t.Errorf("the worker could not pull and ack the command: %v\n%s", err, out)
	}
	if out, err := worker("kv", "put", "ELITEA_RT_V1_DEADLETTER", "agent.ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164", "{}").CombinedOutput(); err != nil {
		t.Errorf("the worker could not record a dead letter: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"consumer", "add", "ELITEA_RT_V1_AGENT", "rogue", "--pull", "--defaults"},
		{"kv", "put", "ELITEA_CANVAS_PRESENCE", "x", "y"},
		{"pub", "elitea.rt.v1.replay.wake", "x"},
	} {
		if out, err := worker(args...).CombinedOutput(); err == nil && args[0] != "pub" {
			t.Errorf("the worker ran `nats %s`:\n%s", strings.Join(args, " "), out)
		}
	}
	s.RequireViolation(t, natsconn.IdentityWorker, "Publish", "elitea.rt.v1.replay.wake")

	// The execution-replay wake-up is the producer's own: elitea-main's
	// replicas publish and subscribe it, inside RUNTIME.
	if out, err := main("pub", "elitea.rt.v1.replay.wake", "x").CombinedOutput(); err != nil {
		t.Errorf("the producer could not publish the replay wake-up: %v\n%s", err, out)
	}
	for _, v := range s.Violations(natsconn.IdentityMainRuntime) {
		if v.Subject == "elitea.rt.v1.replay.wake" {
			t.Errorf("the producer was refused the replay wake-up: %+v", v)
		}
	}

	// Cross-account isolation: neither RUNTIME identity reaches a MAIN or
	// GATEWAY asset (the streams do not exist in RUNTIME, and no grant names
	// them), and MAIN's elitea-main identity cannot inject a command.
	for id, cli := range map[string]func(args ...string) *exec.Cmd{
		natsconn.IdentityMainRuntime: main,
		natsconn.IdentityWorker:      worker,
	} {
		for _, args := range [][]string{
			{"stream", "info", "GATEWAY_BUDGET_DELTAS"},
			{"stream", "info", "KV_ELITEA_CANVAS_PRESENCE"},
			{"consumer", "next", "GATEWAY_BUDGET_DELTAS", "budget-writeback", "--count", "1"},
			{"pub", "--jetstream", "gateway.budget.delta", "{}"},
		} {
			if out, err := cli(args...).CombinedOutput(); err == nil {
				t.Errorf("%s ran `nats %s` across accounts:\n%s", id, strings.Join(args, " "), out)
			}
		}
	}
	if out, err := cliAs(t, s, natsconn.IdentityMain)("pub", "--jetstream", subject, "forged").CombinedOutput(); err == nil {
		t.Errorf("MAIN's elitea-main published a runtime command:\n%s", out)
	}
	s.RequireViolation(t, natsconn.IdentityMain, "Publish", subject)

	// The ack really removed it: WorkQueue.
	raw, err := cliAs(t, s, natsconn.IdentityBootstrapRuntime)("stream", "info", "ELITEA_RT_V1_AGENT", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"messages": 0`) {
		t.Errorf("an acknowledged command is still in the WorkQueue stream:\n%s", raw)
	}
}

// The KEDA nats-jetstream scaler sizes the worker fleet from /jsz: it asks
// /jsz?acc=<account>&consumers=true&config=true and reads num_pending of the
// durable under account_details[name=<account>]. The chart's scaler names
// RUNTIME (deploy/helm/elitea worker.autoscaling.natsAccount); pin that the
// server reports the command bus there, and that the old global-account name
// sees none of it (a wrong account reads as zero lag: no scale-up, ever).
func TestKEDAReadsTheCommandBusLagInTheRuntimeAccount(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	main := cliAs(t, s, natsconn.IdentityMainRuntime)
	subject := "elitea.rt.v1.agent.d.ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164"
	if out, err := main("pub", "--jetstream", subject, "body").CombinedOutput(); err != nil {
		t.Fatalf("publish a command: %v\n%s", err, out)
	}
	pending := func(account string) (int, bool) {
		resp, err := http.Get(s.MonitorURL() + "/jsz?acc=" + url.QueryEscape(account) + "&consumers=true&config=true")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var jsz struct {
			Accounts []struct {
				Name    string `json:"name"`
				Streams []struct {
					Name      string `json:"name"`
					Consumers []struct {
						Name       string `json:"name"`
						NumPending int    `json:"num_pending"`
					} `json:"consumer_detail"`
				} `json:"stream_detail"`
			} `json:"account_details"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&jsz); err != nil {
			t.Fatal(err)
		}
		for _, a := range jsz.Accounts {
			if a.Name != account {
				continue
			}
			for _, st := range a.Streams {
				if st.Name != "ELITEA_RT_V1_AGENT" {
					continue
				}
				for _, c := range st.Consumers {
					if c.Name == "elitea-agent-worker-v1" {
						return c.NumPending, true
					}
				}
			}
		}
		return 0, false
	}
	if n, ok := pending(natsconn.AccountRuntime); !ok || n != 1 {
		t.Errorf("/jsz?acc=RUNTIME: elitea-agent-worker-v1 found=%t num_pending=%d, want found and 1", ok, n)
	}
	if _, ok := pending("$G"); ok {
		t.Error("/jsz?acc=$G reports the command bus; the streams belong to RUNTIME")
	}
}
