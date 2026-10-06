package natstest

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
)

// EnvSecureHAConf names the nats.conf rendered from the chart's HA profile
// (scripts/nats/render-secure-conf.sh OUT deploy/helm/nats/values-ha.yaml).
const EnvSecureHAConf = "ELITEA_TEST_NATS_SECURE_HA_CONF"

// Route identities for the cluster tests.
const (
	// RouteIdentity holds a route certificate from the ROUTE CA: what a real
	// cluster peer presents.
	RouteIdentity = "route"
	// RouteFromClientCA holds a certificate from the CLIENT CA that is valid
	// as a TLS client and server: any service's NATS identity, presented on
	// the route port.
	RouteFromClientCA = "route-from-client-ca"
)

// Cluster is one NATS server started with the HA profile's OWN cluster block
// (the route TLS settings under test), and the peers a test points at it.
type Cluster struct {
	dir         string
	bin         string
	name        string
	clusterPort int
	httpPort    int
	routeCA     []byte

	mu    sync.Mutex
	procs []*exec.Cmd
	logs  map[string]*bytes.Buffer
}

// StartCluster starts a node from the chart's HA rendered config: its client
// TLS block and its cluster block, with the chart's container paths swapped
// for local ones. A missing variable skips or fails exactly like Start.
func StartCluster(t testing.TB) *Cluster {
	t.Helper()
	bin, conf := os.Getenv(EnvServerBin), os.Getenv(EnvSecureHAConf)
	if strings.TrimSpace(bin) == "" || strings.TrimSpace(conf) == "" {
		msg := fmt.Sprintf("the route identity test needs %s and %s (scripts/nats/render-secure-conf.sh OUT deploy/helm/nats/values-ha.yaml)", EnvServerBin, EnvSecureHAConf)
		if os.Getenv(EnvRequire) == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	rendered, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	c := &Cluster{dir: t.TempDir(), bin: bin, logs: map[string]*bytes.Buffer{}}
	t.Cleanup(c.stop)
	if err := c.writePKI(); err != nil {
		t.Fatal(err)
	}
	clusterBlock, tlsBlock, err := chartBlocks(string(rendered))
	if err != nil {
		t.Fatal(err)
	}
	var cluster struct {
		Name string `json:"name"`
		TLS  struct {
			CAFile string `json:"ca_file"`
			Verify bool   `json:"verify"`
		} `json:"tls"`
	}
	if err := json.Unmarshal([]byte(clusterBlock), &cluster); err != nil {
		t.Fatalf("the HA profile's cluster block does not parse: %v", err)
	}
	if !cluster.TLS.Verify || cluster.TLS.CAFile == "" {
		t.Fatalf("the HA profile's routes do not verify against a CA: %s", clusterBlock)
	}
	c.name = cluster.Name
	c.clusterPort, c.httpPort = freePort(t), freePort(t)

	// The chart's blocks, localised. Every substitution must apply, so a
	// chart change that moves a path fails here instead of testing nothing.
	for _, sub := range []struct{ from, to string }{
		{`"/etc/nats-certs/cluster/`, `"` + filepath.Join(c.dir, "node-route") + `/`},
		{`"port": 6222`, `"port": ` + strconv.Itoa(c.clusterPort)},
	} {
		if !strings.Contains(clusterBlock, sub.from) {
			return failf(t, "the HA cluster block no longer contains %s; natstest's substitutions must follow the chart", sub.from)
		}
		clusterBlock = strings.ReplaceAll(clusterBlock, sub.from, sub.to)
	}
	// A cluster block that (wrongly) trusts the client CA names its mount;
	// localise it too, so such a chart fails the test rather than the start.
	clusterBlock = strings.ReplaceAll(clusterBlock, `"/etc/nats-ca-cert/`, `"`+filepath.Join(c.dir, "client-ca")+`/`)
	clusterBlock = replaceRoutes(clusterBlock)
	for _, sub := range []struct{ from, to string }{
		{`"/etc/nats-certs/nats/`, `"` + filepath.Join(c.dir, "node-client") + `/`},
		{`"/etc/nats-ca-cert/`, `"` + filepath.Join(c.dir, "client-ca") + `/`},
	} {
		if !strings.Contains(tlsBlock, sub.from) {
			return failf(t, "the HA client TLS block no longer contains %s", sub.from)
		}
		tlsBlock = strings.ReplaceAll(tlsBlock, sub.from, sub.to)
	}
	node := fmt.Sprintf("port: %d\nhttp_port: %d\nserver_name: node-a\ntls: %s\ncluster: %s\n",
		freePort(t), c.httpPort, tlsBlock, clusterBlock)
	c.start(t, "node-a", node, c.httpPort)
	return c
}

func failf(t testing.TB, format string, args ...any) *Cluster {
	t.Helper()
	t.Fatalf(format, args...)
	return nil
}

// Peer starts a second server that dials the node's route port presenting
// identity's certificate (RouteIdentity or RouteFromClientCA) and trusting the
// route CA, so the only question is whether the NODE accepts it.
func (c *Cluster) Peer(t testing.TB, identity string) {
	t.Helper()
	d := filepath.Join(c.dir, identity)
	conf := fmt.Sprintf(`port: %d
http_port: %d
server_name: peer-%s
cluster {
  name: %q
  port: %d
  routes: ["tls://127.0.0.1:%d"]
  tls {
    cert_file: %q
    key_file: %q
    ca_file: %q
    verify: true
    timeout: 3
  }
}
`, freePort(t), freePort(t), identity, c.name, freePort(t), c.clusterPort,
		filepath.Join(d, "tls.crt"), filepath.Join(d, "tls.key"), filepath.Join(c.dir, "route-ca.crt"))
	c.start(t, "peer-"+identity, conf, 0)
}

// Routes is the number of routes the node has, from its own /routez.
func (c *Cluster) Routes(t testing.TB) int {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/routez", c.httpPort)) //nolint:gosec,noctx // a local test server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var routez struct {
		NumRoutes int `json:"num_routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&routez); err != nil {
		t.Fatal(err)
	}
	return routez.NumRoutes
}

// Log is the node's log so far.
func (c *Cluster) Log() string { return c.LogOf("node-a") }

// LogOf is one server's log so far: "node-a", or "peer-<identity>".
func (c *Cluster) LogOf(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.logs[name]; b != nil {
		return b.String()
	}
	return ""
}

func (c *Cluster) start(t testing.TB, name, conf string, httpPort int) {
	t.Helper()
	path := filepath.Join(c.dir, name+".conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	cmd := exec.Command(c.bin, "-c", path)
	cmd.Stdout = &syncWriter{mu: &c.mu, w: &log}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	c.mu.Lock()
	c.procs = append(c.procs, cmd)
	c.logs[name] = &log
	c.mu.Unlock()
	if httpPort == 0 {
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", httpPort)) //nolint:gosec,noctx // a local test server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not become healthy:\n%s\nconfig:\n%s", name, log.String(), conf)
}

func (c *Cluster) stop() {
	c.mu.Lock()
	procs := c.procs
	c.procs = nil
	c.mu.Unlock()
	for _, p := range procs {
		if p.Process != nil {
			_ = p.Process.Kill()
			_ = p.Wait()
		}
	}
}

// writePKI issues a client CA (with the node's client-port certificate and a
// client identity usable as a route) and a separate ROUTE CA (with the
// node's and a legitimate peer's route certificates).
func (c *Cluster) writePKI() error {
	clientCA, err := newAuthority("elitea-nats-ca (test)")
	if err != nil {
		return err
	}
	routeCA, err := newAuthority("elitea-nats-route-ca (test)")
	if err != nil {
		return err
	}
	c.routeCA = routeCA.pem
	if err := os.MkdirAll(filepath.Join(c.dir, "client-ca"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.dir, "client-ca", "ca.crt"), clientCA.pem, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.dir, "route-ca.crt"), routeCA.pem, 0o600); err != nil {
		return err
	}
	local := []net.IP{net.ParseIP("127.0.0.1")}
	both := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	issues := []struct {
		by    *authority
		dir   string
		l     leaf
		trust []byte
	}{
		{clientCA, "node-client", leaf{cn: "elitea-nats", ips: local, dns: []string{"localhost"}, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, clientCA.pem},
		// cert-manager writes the issuing CA into the route Secret's ca.crt,
		// which is what the chart's ca_file names.
		{routeCA, "node-route", leaf{cn: "elitea-nats-route", ips: local, eku: both}, routeCA.pem},
		{routeCA, RouteIdentity, leaf{cn: "elitea-nats-route", ips: local, eku: both}, routeCA.pem},
		// The client CA signing a certificate a route could use: what any
		// identity minted against the client issuer looks like on 6222.
		{clientCA, RouteFromClientCA, leaf{
			cn: natsconn.IdentityMain, ips: local, eku: both,
			uris: []string{natsconn.IdentityURI(natsconn.DefaultTrustDomain, natsconn.IdentityMain)},
		}, clientCA.pem},
	}
	for _, i := range issues {
		if err := i.by.issue(filepath.Join(c.dir, i.dir), i.l, i.trust); err != nil {
			return err
		}
	}
	return nil
}

// chartBlocks returns the rendered config's "cluster" and top-level "tls"
// objects as text.
func chartBlocks(conf string) (cluster, tlsBlock string, err error) {
	cluster, err = objectAfter(conf, `"cluster": `)
	if err != nil {
		return "", "", err
	}
	// The top-level tls block is the LAST "tls" key at depth 1; the cluster's
	// own sits inside the cluster object. Search after the cluster block.
	rest := conf[strings.Index(conf, `"cluster": `)+len(cluster):]
	tlsBlock, err = objectAfter(rest, `"tls": `)
	return cluster, tlsBlock, err
}

func objectAfter(s, key string) (string, error) {
	i := strings.Index(s, key)
	if i < 0 {
		return "", fmt.Errorf("the rendered config has no %s object", key)
	}
	start := i + len(key)
	if start >= len(s) || s[start] != '{' {
		return "", fmt.Errorf("%s is not an object", key)
	}
	depth := 0
	for j := start; j < len(s); j++ {
		switch s[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : j+1], nil
			}
		}
	}
	return "", errors.New("unbalanced braces in the rendered config")
}

// replaceRoutes empties the chart's route list (its DNS names do not resolve
// here): the node under test only accepts routes; the peers dial it.
func replaceRoutes(cluster string) string {
	i := strings.Index(cluster, `"routes": [`)
	if i < 0 {
		return cluster
	}
	j := strings.Index(cluster[i:], "]")
	return cluster[:i] + `"routes": []` + cluster[i+j+1:]
}
