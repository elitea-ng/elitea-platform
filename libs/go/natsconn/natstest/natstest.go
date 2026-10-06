// Package natstest starts a nats-server from the NATS chart's OWN rendered
// configuration, with test certificates, so a service's real NATS code can be
// run against the permission table it will meet in a cluster (#1076).
//
// The permission table lives in deploy/helm/nats/values.yaml. Copying it into
// a test would prove the copy; this package instead reads the nats.conf that
// `helm template` renders (scripts/nats/render-secure-conf.sh writes it), swaps
// the file paths and ports for local ones, and runs the real server binary on
// it. A grant missing from the chart is a permissions violation here, in the
// service's own test, before it is a broken budget path in a cluster.
//
// The JetStream assets are created the way a cluster creates them: by running
// deploy/helm/nats-bootstrap/files/bootstrap.sh with the nats CLI, as the
// bootstrap identity. So the bootstrap's own grants are exercised by every
// test that uses this package.
//
// Environment (all three are required; see Start for what a missing one does):
//
//	ELITEA_TEST_NATS_SERVER_BIN    nats-server 2.12+ binary
//	ELITEA_TEST_NATS_SECURE_CONF   nats.conf rendered from the chart's scale-1 profile
//	ELITEA_TEST_NATS_CLI_BIN       nats CLI (natscli 0.3+, for --allow-counter)
//	ELITEA_REQUIRE_NATS_SECURE_TEST  1 turns a missing variable into a failure
package natstest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
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

// Environment variable names.
const (
	EnvServerBin  = "ELITEA_TEST_NATS_SERVER_BIN"
	EnvSecureConf = "ELITEA_TEST_NATS_SECURE_CONF"
	EnvCLIBin     = "ELITEA_TEST_NATS_CLI_BIN"
	EnvRequire    = "ELITEA_REQUIRE_NATS_SECURE_TEST"
)

// Identities that are not in the permission table, for refusal tests.
const (
	// IdentityNoSAN holds a certificate from the right CA with CN=elitea-main
	// and no URI SAN: what any other certificate from a shared CA looks like.
	IdentityNoSAN = "no-san"
	// IdentityForeignCA holds elitea-main's URI SAN, signed by another CA.
	IdentityForeignCA = "foreign-ca"
	// IdentityUnknown holds a URI SAN from the right CA that names no user.
	IdentityUnknown = "unknown"
)

// Server is a running secured nats-server.
type Server struct {
	dir      string
	conf     string
	bin      string
	cliBin   string
	repoRoot string
	port     int
	httpPort int

	mu  sync.Mutex
	cmd *exec.Cmd
	log *bytes.Buffer
}

// Start renders the test PKI, writes the chart's config with local paths and
// starts the server. The server is stopped when the test ends.
//
// A missing environment variable skips the test with the variable named, or
// fails it when ELITEA_REQUIRE_NATS_SECURE_TEST=1 (CI sets that, so losing the
// rendered config or a binary cannot read as a pass).
func Start(t testing.TB) *Server {
	t.Helper()
	bin, conf, cli := os.Getenv(EnvServerBin), os.Getenv(EnvSecureConf), os.Getenv(EnvCLIBin)
	var missing []string
	for name, v := range map[string]string{EnvServerBin: bin, EnvSecureConf: conf, EnvCLIBin: cli} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		msg := fmt.Sprintf("secured NATS permission test needs %s (scripts/nats/render-secure-conf.sh writes the config)", strings.Join(missing, ", "))
		if os.Getenv(EnvRequire) == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := os.ReadFile(conf)
	if err != nil {
		t.Fatalf("read %s: %v", EnvSecureConf, err)
	}

	s := &Server{dir: t.TempDir(), bin: bin, cliBin: cli, repoRoot: root}
	if err := s.writePKI(); err != nil {
		t.Fatalf("test PKI: %v", err)
	}
	s.port, s.httpPort = freePort(t), freePort(t)
	localConf, err := s.localise(string(rendered))
	if err != nil {
		t.Fatal(err)
	}
	s.conf = filepath.Join(s.dir, "nats.conf")
	if err := os.WriteFile(s.conf, []byte(localConf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "js"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.start(t)
	t.Cleanup(s.Stop)
	return s
}

// URL is the client URL: tls://, no credential, as the chart renders it.
func (s *Server) URL() string { return fmt.Sprintf("tls://127.0.0.1:%d", s.port) }

// PlainURL is the same address with a nats:// scheme, for refusal tests.
func (s *Server) PlainURL() string { return fmt.Sprintf("nats://127.0.0.1:%d", s.port) }

// Dir is the server's scratch directory.
func (s *Server) Dir() string { return s.dir }

// Material is identity's TLS material, as natsconn reads it.
func (s *Server) Material(identity string) natsconn.Material {
	d := s.clientDir(identity)
	return natsconn.Material{
		CAFile:   filepath.Join(d, "ca.crt"),
		CertFile: filepath.Join(d, "tls.crt"),
		KeyFile:  filepath.Join(d, "tls.key"),
	}
}

// Lookup is an environment for a service that reads <prefix>_NATS_TLS_* and
// a URL variable: what the chart sets on its pod. extra adds or overrides
// variables.
func (s *Server) Lookup(prefix, identity string, extra map[string]string) func(string) (string, bool) {
	m := s.Material(identity)
	names := natsconn.EnvNames(prefix)
	env := map[string]string{
		names[0]: m.CAFile,
		names[1]: m.CertFile,
		names[2]: m.KeyFile,
	}
	for k, v := range extra {
		env[k] = v
	}
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

// Bootstrap runs deploy/helm/nats-bootstrap/files/bootstrap.sh as the
// bootstrap identity, exactly as the Job does, and fails the test if it
// fails. env overrides the script's tuning variables.
func (s *Server) Bootstrap(t testing.TB, env map[string]string) string {
	t.Helper()
	out, err := s.RunBootstrap(natsconn.IdentityBootstrap, env)
	if err != nil {
		t.Fatalf("bootstrap.sh as %s: %v\n%s\nserver log:\n%s", natsconn.IdentityBootstrap, err, out, s.Log())
	}
	return out
}

// RunBootstrap runs bootstrap.sh presenting identity's certificate and
// returns its output.
func (s *Server) RunBootstrap(identity string, env map[string]string) (string, error) {
	script := filepath.Join(s.repoRoot, "deploy", "helm", "nats-bootstrap", "files", "bootstrap.sh")
	m := s.Material(identity)
	binDir := filepath.Join(s.dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	// The script calls `nats` by name.
	link := filepath.Join(binDir, "nats")
	if _, err := os.Lstat(link); err != nil {
		if err := os.Symlink(s.cliBin, link); err != nil {
			return "", err
		}
	}
	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+s.dir, // keep the CLI away from the developer's contexts
		"NATS_URL="+s.URL(),
		"NATS_TLS_CA_FILE="+m.CAFile,
		"NATS_TLS_CERT_FILE="+m.CertFile,
		"NATS_TLS_KEY_FILE="+m.KeyFile,
		"NATS_INBOX_PREFIX="+natsconn.InboxPrefix(identity),
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Log is the server's output so far.
func (s *Server) Log() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return ""
	}
	return s.log.String()
}

// Stop stops the server. Safe to call twice.
func (s *Server) Stop() {
	s.mu.Lock()
	cmd := s.cmd
	s.cmd = nil
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// Restart stops the server and starts it again on the same port and store.
func (s *Server) Restart(t testing.TB) {
	t.Helper()
	s.Stop()
	s.start(t)
}

func (s *Server) start(t testing.TB) {
	t.Helper()
	var log bytes.Buffer
	cmd := exec.Command(s.bin, "-c", s.conf)
	cmd.Env = append(os.Environ(), "SERVER_NAME=natstest")
	cmd.Stdout = &syncWriter{mu: &s.mu, w: &log}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nats-server: %v", err)
	}
	s.mu.Lock()
	s.cmd, s.log = cmd, &log
	s.mu.Unlock()

	health := fmt.Sprintf("http://127.0.0.1:%d/healthz?js-enabled-only=true", s.httpPort)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(health) //nolint:gosec,noctx // a local test server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Stop()
	t.Fatalf("nats-server did not become healthy:\n%s", s.Log())
}

type syncWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// localise rewrites the cluster paths and ports in the rendered config. Every
// substitution must apply exactly as written: a chart change that moves a
// path makes this fail loudly instead of running a server that, say, never
// loaded the CA.
func (s *Server) localise(conf string) (string, error) {
	if strings.Contains(conf, `"cluster"`) {
		return "", errors.New("the rendered config is clustered; render the scale-1 profile for the single-node test server")
	}
	subs := []struct{ from, to string }{
		{`"/etc/nats-certs/nats/`, `"` + filepath.Join(s.dir, "server") + `/`},
		{`"/etc/nats-ca-cert/`, `"` + filepath.Join(s.dir, "ca") + `/`},
		{`"store_dir": "/data"`, `"store_dir": ` + strconv.Quote(filepath.Join(s.dir, "js"))},
		{`"pid_file": "/var/run/nats/nats.pid"`, `"pid_file": ` + strconv.Quote(filepath.Join(s.dir, "nats.pid"))},
		{`"port": 4222`, `"port": ` + strconv.Itoa(s.port)},
		{`"http_port": 8222`, `"http_port": ` + strconv.Itoa(s.httpPort)},
		// max_file_store is kept: GATEWAY_BUDGET_DELTAS reserves its MaxBytes
		// (1 GiB) against it, so a smaller test budget refuses the stream —
		// and the chart's own budget is the one that has to fit.
	}
	for _, sub := range subs {
		if !strings.Contains(conf, sub.from) {
			return "", fmt.Errorf("the rendered NATS config no longer contains %s; natstest's path substitutions must follow the chart", sub.from)
		}
		conf = strings.ReplaceAll(conf, sub.from, sub.to)
	}
	for _, needle := range []string{`"verify_and_map": true`, `"authorization"`} {
		if !strings.Contains(conf, needle) {
			return "", fmt.Errorf("the rendered NATS config lacks %s; it is not the secured chart profile", needle)
		}
	}
	return conf, nil
}

func (s *Server) clientDir(identity string) string {
	return filepath.Join(s.dir, "clients", identity)
}

// ── PKI ──────────────────────────────────────────────────────────────────

type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newAuthority(cn string) (*authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &authority{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

type leaf struct {
	cn   string
	uris []string
	dns  []string
	ips  []net.IP
	eku  []x509.ExtKeyUsage
}

func (a *authority) issue(dir string, l leaf, trust []byte) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: l.cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  l.eku,
		DNSNames:     l.dns,
		IPAddresses:  l.ips,
	}
	for _, raw := range l.uris {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		tmpl.URIs = append(tmpl.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	files := map[string][]byte{
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		"ca.crt":  trust,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) writePKI() error {
	ca, err := newAuthority("elitea-nats-ca (test)")
	if err != nil {
		return err
	}
	foreign, err := newAuthority("some-other-ca (test)")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "ca"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir, "ca", "ca.crt"), ca.pem, 0o600); err != nil {
		return err
	}
	server := leaf{
		cn:  "elitea-nats",
		dns: []string{"localhost"},
		ips: []net.IP{net.ParseIP("127.0.0.1")},
		eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	if err := ca.issue(filepath.Join(s.dir, "server"), server, ca.pem); err != nil {
		return err
	}
	client := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	for _, id := range []string{
		natsconn.IdentityMain, natsconn.IdentityGateway, natsconn.IdentityScheduler,
		natsconn.IdentityBootstrap, natsconn.IdentityWorker,
	} {
		l := leaf{cn: id, uris: []string{natsconn.IdentityURI(natsconn.DefaultTrustDomain, id)}, eku: client}
		if err := ca.issue(s.clientDir(id), l, ca.pem); err != nil {
			return err
		}
	}
	extras := []struct {
		id string
		by *authority
		l  leaf
	}{
		{IdentityNoSAN, ca, leaf{cn: natsconn.IdentityMain, eku: client}},
		{IdentityUnknown, ca, leaf{cn: "unknown", uris: []string{natsconn.IdentityURI(natsconn.DefaultTrustDomain, "nobody")}, eku: client}},
		{IdentityForeignCA, foreign, leaf{cn: natsconn.IdentityMain, uris: []string{natsconn.IdentityURI(natsconn.DefaultTrustDomain, natsconn.IdentityMain)}, eku: client}},
	}
	for _, e := range extras {
		// The client trusts the real CA in every case: the refusal under test
		// is the server's, not the client's.
		if err := e.by.issue(s.clientDir(e.id), e.l, ca.pem); err != nil {
			return err
		}
	}
	return nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		panic(err)
	}
	return n
}

func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// findRepoRoot walks up from the working directory to the directory holding
// the bootstrap script. Tests run in their package directory, which is
// somewhere below it for every module in this repository.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "deploy", "helm", "nats-bootstrap", "files", "bootstrap.sh")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("natstest: no deploy/helm/nats-bootstrap/files/bootstrap.sh above the working directory")
		}
		dir = parent
	}
}

// CLI is the nats CLI binary the tests were given.
func CLI() string { return os.Getenv(EnvCLIBin) }

// Violation is one permissions violation the server logged.
type Violation struct {
	// Kind is "Publish" or "Subscription".
	Kind    string
	Subject string
}

// Violations returns the permissions violations the server logged for
// identity, in order. The server logs every refused publish and subscribe
// with the mapped user name, which is what makes a refusal assertable rather
// than inferred from a client timeout.
func (s *Server) Violations(identity string) []Violation {
	user := `"$G/user:` + natsconn.IdentityURI(natsconn.DefaultTrustDomain, identity) + `"`
	var out []Violation
	for _, line := range strings.Split(s.Log(), "\n") {
		if !strings.Contains(line, user) {
			continue
		}
		for _, kind := range []string{"Publish", "Subscription"} {
			marker := " - " + kind + " Violation - Subject \""
			i := strings.Index(line, marker)
			if i < 0 {
				continue
			}
			rest := line[i+len(marker):]
			if j := strings.Index(rest, `"`); j >= 0 {
				out = append(out, Violation{Kind: kind, Subject: rest[:j]})
			}
		}
	}
	return out
}

// RequireNoViolations fails the test when the server logged any permissions
// violation for identity. A positive test calls it after the service's real
// code ran: a client library that swallows a refused request (and falls back,
// or retries) would otherwise pass while the cluster logs errors forever.
func (s *Server) RequireNoViolations(t testing.TB, identity string) {
	t.Helper()
	if v := s.Violations(identity); len(v) > 0 {
		t.Errorf("%s hit %d permissions violation(s) on its real code path; the chart's permission table is missing a grant: %+v", identity, len(v), v)
	}
}

// RequireViolation fails the test unless the server logged a violation of
// kind ("Publish" or "Subscription") for identity on subject.
func (s *Server) RequireViolation(t testing.TB, identity, kind, subject string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, v := range s.Violations(identity) {
			if v.Kind == kind && v.Subject == subject {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("no %s violation logged for %s on %q; logged: %+v", kind, identity, subject, s.Violations(identity))
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
