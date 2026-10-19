package system_test

import (
	"context"
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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type runtimePKI struct {
	caPath                      string
	workerCertPath              string
	workerKeyPath               string
	wrongIdentityWorkerCertPath string
	wrongIdentityWorkerKeyPath  string
	untrustedWorkerCertPath     string
	untrustedWorkerKeyPath      string
	controlCertPath             string
	controlKeyPath              string
	outputCertPath              string
	outputKeyPath               string
	contentCertPath             string
	contentKeyPath              string
}

func writeFile(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func generateRuntimePKI(t *testing.T, root string) runtimePKI {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public := &private.PublicKey
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ELITEA runtime system-test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(root, "runtime-ca.pem")
	writeFile(t, caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644)

	issue := func(name string, serial int64, client bool, spiffe string) (string, string) {
		certificatePrivate, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		certificatePublic := &certificatePrivate.PublicKey
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: name},
			NotBefore:    now.Add(-time.Minute),
			NotAfter:     now.Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		if client {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
			identity, err := url.Parse(spiffe)
			if err != nil {
				t.Fatal(err)
			}
			template.URIs = []*url.URL{identity}
		} else {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			template.DNSNames = []string{"localhost"}
			template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		certificateDER, err := x509.CreateCertificate(rand.Reader, template, ca, certificatePublic, private)
		if err != nil {
			t.Fatal(err)
		}
		privateDER, err := x509.MarshalPKCS8PrivateKey(certificatePrivate)
		if err != nil {
			t.Fatal(err)
		}
		certificatePath := filepath.Join(root, name+".pem")
		privatePath := filepath.Join(root, name+".key")
		writeFile(t, certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), 0o644)
		writeFile(t, privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600)
		return certificatePath, privatePath
	}

	workerCert, workerKey := issue("worker-client", 2, true, workloadID)
	wrongIdentityWorkerCert, wrongIdentityWorkerKey := issue(
		"worker-client-wrong-identity",
		7,
		true,
		"spiffe://elitea.test/runtime/wrong-worker",
	)
	untrustedWorkerCert, untrustedWorkerKey := issueSelfSignedClient(t, root, now)
	controlCert, controlKey := issue("control-server", 4, false, "")
	outputCert, outputKey := issue("output-server", 5, false, "")
	contentCert, contentKey := issue("content-server", 6, false, "")
	return runtimePKI{
		caPath: caPath, workerCertPath: workerCert, workerKeyPath: workerKey,
		wrongIdentityWorkerCertPath: wrongIdentityWorkerCert,
		wrongIdentityWorkerKeyPath:  wrongIdentityWorkerKey,
		untrustedWorkerCertPath:     untrustedWorkerCert,
		untrustedWorkerKeyPath:      untrustedWorkerKey,
		controlCertPath:             controlCert, controlKeyPath: controlKey,
		outputCertPath: outputCert, outputKeyPath: outputKey,
		contentCertPath: contentCert, contentKeyPath: contentKey,
	}
}

func issueSelfSignedClient(t *testing.T, root string, now time.Time) (string, string) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := url.Parse(workloadID)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(8),
		Subject:      pkix.Name{CommonName: "worker-client-untrusted"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{identity},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certificatePath := filepath.Join(root, "worker-client-untrusted.pem")
	privatePath := filepath.Join(root, "worker-client-untrusted.key")
	writeFile(t, certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), 0o644)
	writeFile(t, privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600)
	return certificatePath, privatePath
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	created, err := os.MkdirTemp("", "elitea-runtime-system-")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(created)
	if err != nil {
		_ = os.RemoveAll(created)
		t.Fatal(err)
	}
	if err := os.Chmod(canonical, 0o700); err != nil {
		_ = os.RemoveAll(canonical)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(canonical) })
	return canonical
}

func mustMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}

func requireCommand(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("required system-test command %q is unavailable: %v", name, err)
	}
}

func findRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate system-test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../../../.."))
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		t.Fatalf("locate repository root %s: %v", root, err)
	}
	return root
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

type containerSet struct {
	mu    sync.Mutex
	names []string
}

func (s *containerSet) start(t *testing.T, role, image string, runOptions []string, commandArgs ...string) string {
	t.Helper()
	name := fmt.Sprintf("elitea-runtime-system-%s-%d-%d", role, os.Getpid(), time.Now().UnixNano())
	args := []string{"run", "--rm", "-d", "--name", name}
	args = append(args, runOptions...)
	args = append(args, image)
	args = append(args, commandArgs...)
	output, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("start %s container: %v\n%s", role, err, output)
	}
	s.mu.Lock()
	s.names = append(s.names, name)
	s.mu.Unlock()
	return name
}

func (s *containerSet) logs(name string) string {
	output, _ := exec.Command("docker", "logs", name).CombinedOutput()
	return string(output)
}

func (s *containerSet) stopAll() {
	s.mu.Lock()
	names := append([]string(nil), s.names...)
	s.names = nil
	s.mu.Unlock()
	for index := len(names) - 1; index >= 0; index-- {
		_ = exec.Command("docker", "rm", "-f", names[index]).Run()
	}
}

func waitForPostgres(t *testing.T, ctx context.Context, databaseURL string, containers *containerSet, containerName string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return pool.Ping(pingCtx) == nil, nil
	}); err != nil {
		pool.Close()
		t.Fatalf("PostgreSQL did not become ready: %v\n%s", err, containers.logs(containerName))
	}
	return pool
}

func bootstrapDatabase(t *testing.T, ctx context.Context, repositoryRoot string, pool *pgxpool.Pool) {
	t.Helper()
	migrationPath := filepath.Join(repositoryRoot, "services", "elitea-main", "internal", "infra", "db", "migrations", "001_initial.sql")
	migration, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply legacy bootstrap migration: %v", err)
	}
}

func buildGoBinary(t *testing.T, repositoryRoot, output, packagePath string) {
	t.Helper()
	runCommand(t, filepath.Join(repositoryRoot, "services", "elitea-main"), nil, "go", "build", "-trimpath", "-o", output, packagePath)
}

func runCommand(t *testing.T, directory string, environment []string, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), environment...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run %s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
}

const (
	workloadSession = "system-session-1"
	workloadID      = "spiffe://elitea.test/runtime/worker"
)

func eventually(ctx context.Context, interval time.Duration, check func() (bool, error)) error {
	if interval <= 0 || check == nil {
		return errors.New("invalid eventual assertion")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		ok, err := check()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
