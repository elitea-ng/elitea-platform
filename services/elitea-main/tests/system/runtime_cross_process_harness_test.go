package system_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	redis "github.com/redis/go-redis/v9"
)

type runtimePKI struct {
	caPath                      string
	workerCertPath              string
	workerKeyPath               string
	wrongIdentityWorkerCertPath string
	wrongIdentityWorkerKeyPath  string
	untrustedWorkerCertPath     string
	untrustedWorkerKeyPath      string
	redisCertPath               string
	redisKeyPath                string
	controlCertPath             string
	controlKeyPath              string
	outputCertPath              string
	outputKeyPath               string
	contentCertPath             string
	contentKeyPath              string
}

type signingMaterial struct {
	privateKeyPath  string
	goodKeyringPath string
	badKeyringPath  string
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
	redisCert, redisKey := issue("redis-server", 3, false, "")
	controlCert, controlKey := issue("control-server", 4, false, "")
	outputCert, outputKey := issue("output-server", 5, false, "")
	contentCert, contentKey := issue("content-server", 6, false, "")
	return runtimePKI{
		caPath: caPath, workerCertPath: workerCert, workerKeyPath: workerKey,
		wrongIdentityWorkerCertPath: wrongIdentityWorkerCert,
		wrongIdentityWorkerKeyPath:  wrongIdentityWorkerKey,
		untrustedWorkerCertPath:     untrustedWorkerCert,
		untrustedWorkerKeyPath:      untrustedWorkerKey,
		redisCertPath:               redisCert, redisKeyPath: redisKey,
		controlCertPath: controlCert, controlKeyPath: controlKey,
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

func generateSigningMaterial(t *testing.T, root string) signingMaterial {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(root, "command-signing.key")
	writeFile(t, privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600)
	_, wrongPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongPublic := wrongPrivate.Public().(ed25519.PublicKey)
	writeKeyring := func(name string, key ed25519.PublicKey) string {
		encoded, err := json.Marshal(map[string]any{
			"schema_version": "elitea.runtime-ed25519-keyring.v1",
			"keys": []map[string]string{{
				"key_id":            signingKeyID,
				"public_key_base64": base64.StdEncoding.EncodeToString(key),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, name)
		writeFile(t, path, encoded, 0o644)
		return path
	}
	return signingMaterial{
		privateKeyPath:  privatePath,
		goodKeyringPath: writeKeyring("keyring-good.json", public),
		badKeyringPath:  writeKeyring("keyring-wrong.json", wrongPublic),
	}
}

func prepareTLSRedisConfig(t *testing.T, directory string, pki runtimePKI) {
	t.Helper()
	copyPublic := func(source, target string) {
		contents, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(directory, target), contents, 0o644)
	}
	copyPublic(pki.caPath, "ca.pem")
	copyPublic(pki.redisCertPath, "redis.pem")
	copyPublic(pki.redisKeyPath, "redis.key")
	writeFile(t, filepath.Join(directory, "users.acl"), []byte(strings.Join([]string{
		"user default off",
		"user producer on >" + producerPassword + " ~" + commandStream + " ~" + commandStream + ":delivery-index.v1 +@connection +eval +evalsha +xlen +xadd +hget +xrange +hdel +hlen +hset",
		"user worker on >" + workerPassword + " ~" + commandStream + " ~" + commandStream + ":delivery-index.v1 +@connection +eval +xreadgroup +xclaim +xautoclaim +hget +xrange +xpending +xack +xdel +hdel",
		"user observer on >" + observerPassword + " ~" + commandStream + " ~" + commandStream + ":delivery-index.v1 +@connection +xgroup +xrange +xlen +xpending +xinfo +hget +hlen",
		"user auth on >" + authPassword + " ~runtime-system-auth:* +@all",
	}, "\n")+"\n"), 0o644)
	writeFile(t, filepath.Join(directory, "redis.conf"), []byte(strings.Join([]string{
		"bind 0.0.0.0",
		"protected-mode yes",
		"port 0",
		"tls-port 6379",
		"tls-cert-file /runtime/redis.pem",
		"tls-key-file /runtime/redis.key",
		"tls-ca-cert-file /runtime/ca.pem",
		"tls-auth-clients no",
		"aclfile /runtime/users.acl",
		"save \"\"",
		"dir /data",
		"appendonly yes",
		"appendfsync always",
	}, "\n")+"\n"), 0o644)
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

func (s *containerSet) restart(t *testing.T, name string) {
	t.Helper()
	output, err := exec.Command("docker", "restart", "--time", "10", name).CombinedOutput()
	if err != nil {
		t.Fatalf("restart container %s: %v\n%s", name, err, output)
	}
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

func requireCommand(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("required system-test command %q is unavailable: %v", name, err)
	}
}

func systemPython(t *testing.T, repositoryRoot string) string {
	t.Helper()
	if configured := os.Getenv("ELITEA_SYSTEM_PYTHON"); configured != "" {
		return configured
	}
	if candidate := filepath.Join(filepath.Dir(repositoryRoot), "venv", "bin", "python"); fileExists(candidate) {
		return candidate
	}
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("ELITEA_SYSTEM_PYTHON is required when python3 is unavailable")
	}
	return path
}

func requirePythonRuntime(t *testing.T, python, repositoryRoot string) {
	t.Helper()
	environment := append(os.Environ(), "PYTHONPATH="+pythonPath(repositoryRoot))
	command := exec.Command(python, "-c", "import elitea_sdk, grpc, h2, httpx, pydantic, redis")
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("system-test Python runtime is incomplete: %v\n%s\nInstall the worker's pinned dependencies or set ELITEA_SYSTEM_PYTHON/PYTHONPATH.", err, output)
	}
}

func pythonPath(repositoryRoot string) string {
	paths := []string{
		filepath.Join(repositoryRoot, "services", "elitea-worker-python", "src"),
		filepath.Join(repositoryRoot, "libs", "proto", "gen", "python"),
	}
	if admittedSDK := os.Getenv("ELITEA_SYSTEM_SDK_PATH"); admittedSDK != "" {
		paths = append(paths, admittedSDK)
	} else {
		localSDK := filepath.Join(filepath.Dir(repositoryRoot), "elitea-sdk")
		if fileExists(filepath.Join(localSDK, "elitea_sdk", "__init__.py")) {
			paths = append(paths, localSDK)
		}
	}
	if extra := os.Getenv("ELITEA_SYSTEM_PYTHONPATH"); extra != "" {
		paths = append(paths, extra)
	}
	if inherited := os.Getenv("PYTHONPATH"); inherited != "" {
		paths = append(paths, inherited)
	}
	return strings.Join(paths, string(os.PathListSeparator))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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

func waitForPostgresPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool, containers *containerSet, containerName string) {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return pool.Ping(pingCtx) == nil, nil
	}); err != nil {
		t.Fatalf("PostgreSQL pool did not recover after restart: %v\n%s", err, containers.logs(containerName))
	}
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

func seedRuntimeFixtures(t *testing.T, ctx context.Context, pool *pgxpool.Pool, settings []byte) {
	t.Helper()
	settingsDigest := sha256.Sum256(settings)
	decodeDigest := func(value string) []byte {
		decoded, err := hex.DecodeString(value)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if _, err := transaction.Exec(ctx, `
INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, status_logs, source, author_id
) VALUES (
    1, '00000000-0000-0000-0000-000000000001', 1, 'System runtime',
    'System runtime', 'openapi', 'credentials', '{}'::jsonb, '{}'::jsonb,
    false, false, NULL, 'user', 1
)
ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `
INSERT INTO p_1.configuration_revisions (
    revision_id, configuration_id, configuration_type, settings_entry_id,
    settings_entry_version, settings_content_digest, input_bundle_id,
    catalog_revision, catalog_digest, schema_id, schema_revision,
    schema_digest, created_by
) VALUES ($1, 1, 'openapi', 'settings-system-1', '1', $2,
          'seed-placeholder-bundle', $3, $4, $5, $6, $7, '1')`,
		revisionID, settingsDigest[:], catalogRevision, decodeDigest(catalogDigest), schemaID, schemaRevision, decodeDigest(schemaDigest)); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `
INSERT INTO elitea_runtime.workload_sessions (
    workload_session_id, workload_identity, producer_id, issued_at, expires_at
) VALUES ($1, $2, $3, clock_timestamp(), clock_timestamp() + interval '1 hour')`, workloadSession, workloadID, producerID); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func newControlRedisClient(t *testing.T, port int, username, password, caPath string) *redis.Client {
	t.Helper()
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("parse runtime CA")
	}
	return redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("localhost:%d", port),
		Username: username,
		Password: password,
		Protocol: 2,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			ServerName: "localhost",
			RootCAs:    roots,
		},
	})
}

func waitForRedis(t *testing.T, ctx context.Context, client *redis.Client, containers *containerSet, containerName string) {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return client.Ping(pingCtx).Err() == nil, nil
	}); err != nil {
		t.Fatalf("TLS/ACL Redis did not become ready: %v\n%s", err, containers.logs(containerName))
	}
}

func runtimeMainEnvironment(databaseURL string, legacyRedisPort, controlRedisPort, publicPort, controlPort, outputPort, contentPort int, producerPasswordPath, authConfigPath string, pki runtimePKI, signing signingMaterial) []string {
	return []string{
		"DATABASE_URL=" + databaseURL,
		"SKIP_MIGRATIONS=1",
		fmt.Sprintf("REDIS_URL=127.0.0.1:%d", legacyRedisPort),
		"APPLICATION_SECRET_KEY=" + publicSecret,
		"ELITEA_AUTH_CONFIG_FILE=" + authConfigPath,
		"ELITEA_RUNTIME_ENABLED=true",
		fmt.Sprintf("ELITEA_HTTP_ADDRESS=127.0.0.1:%d", publicPort),
		"ELITEA_RUNTIME_COMMAND_STREAM=" + commandStream,
		"ELITEA_RUNTIME_MAX_OUTSTANDING=16",
		"ELITEA_RUNTIME_STREAM_MAX_ENTRIES=16",
		fmt.Sprintf("ELITEA_RUNTIME_REDIS_URL=rediss://producer@localhost:%d/0", controlRedisPort),
		"ELITEA_RUNTIME_REDIS_PASSWORD_FILE=" + producerPasswordPath,
		"ELITEA_RUNTIME_REDIS_CA_FILE=" + pki.caPath,
		"ELITEA_RUNTIME_REDIS_POOL_SIZE=4",
		"ELITEA_RUNTIME_SIGNING_KEY_ID=" + signingKeyID,
		"ELITEA_RUNTIME_SIGNING_KEY_FILE=" + signing.privateKeyPath,
		"ELITEA_RUNTIME_VERIFICATION_KEYRING_FILE=" + signing.goodKeyringPath,
		fmt.Sprintf("ELITEA_RUNTIME_CONTROL_ADDRESS=127.0.0.1:%d", controlPort),
		fmt.Sprintf("ELITEA_RUNTIME_OUTPUT_ADDRESS=127.0.0.1:%d", outputPort),
		fmt.Sprintf("ELITEA_RUNTIME_CONTENT_ADDRESS=127.0.0.1:%d", contentPort),
		"ELITEA_RUNTIME_CONTROL_TLS_CERT_FILE=" + pki.controlCertPath,
		"ELITEA_RUNTIME_CONTROL_TLS_KEY_FILE=" + pki.controlKeyPath,
		"ELITEA_RUNTIME_CONTROL_TLS_CLIENT_CA_FILE=" + pki.caPath,
		"ELITEA_RUNTIME_OUTPUT_TLS_CERT_FILE=" + pki.outputCertPath,
		"ELITEA_RUNTIME_OUTPUT_TLS_KEY_FILE=" + pki.outputKeyPath,
		"ELITEA_RUNTIME_OUTPUT_TLS_CLIENT_CA_FILE=" + pki.caPath,
		"ELITEA_RUNTIME_CONTENT_TLS_CERT_FILE=" + pki.contentCertPath,
		"ELITEA_RUNTIME_CONTENT_TLS_KEY_FILE=" + pki.contentKeyPath,
		"ELITEA_RUNTIME_CONTENT_TLS_CLIENT_CA_FILE=" + pki.caPath,
	}
}

func writeRuntimeAuthConfig(t *testing.T, root string, controlRedisPort, publicPort int, pki runtimePKI) string {
	t.Helper()
	authPasswordPath := filepath.Join(root, "auth-redis.password")
	attemptKeyPath := filepath.Join(root, "auth-attempt.key")
	patKeyPath := filepath.Join(root, "auth-pat.key")
	formUsersPath := filepath.Join(root, "auth-form-users.json")
	configPath := filepath.Join(root, "auth-form-v1.yaml")

	writeFile(t, authPasswordPath, []byte(authPassword), 0o600)
	writeFile(t, attemptKeyPath, bytes.Repeat([]byte{0xa7}, 32), 0o600)
	writeFile(t, patKeyPath, []byte("system-auth-pat-signing-key-5681"), 0o600)
	writeFile(
		t,
		formUsersPath,
		[]byte(`{"users":[{"login":"system-auth-user","password":"system-auth-form-password-5681","attributes":{"email":"system-auth@example.test"}}]}`),
		0o600,
	)

	config := fmt.Sprintf(`schema_version: elitea.auth.form.v1
public_origin: https://localhost:%d
trusted_proxy_cidrs:
  - 127.0.0.1/32
redirects:
  direct_access_denied: /access_denied
  main_access_denied: /app/access_denied
  default_login: /
  default_logout: /
cookie:
  name: runtime_system_auth
  same_site: lax
  lifetime_seconds: 3600
redis:
  topology: single_primary_endpoint
  url: rediss://auth@localhost:%d/0
  password_file: %q
  ca_file: %q
  key_prefix: "runtime-system-auth:"
  attempt_key_file: %q
credentials:
  pat_signing_key_file: %q
  credential_headers: []
mappers:
  contract: elitea.auth_mappers.tracked.v1
authorization:
  main_configured_public_rules: []
identity:
  initial_global_admins: []
provider:
  kind: form
  form:
    users_json_file: %q
`, publicPort, controlRedisPort, authPasswordPath, pki.caPath, attemptKeyPath, patKeyPath, formUsersPath)
	writeFile(t, configPath, []byte(config), 0o600)
	return configPath
}

func writeWorkerConfig(t *testing.T, root, name string, redisPort, controlPort, outputPort, contentPort, platformPort int, redisPasswordPath string, pki runtimePKI, keyringPath, spoolRoot, spoolKeyPath string) string {
	t.Helper()
	value := map[string]any{
		"schema_version":       "elitea.runtime-deploy.v1",
		"limits_revision":      "elitea.runtime.limits.conformance.v2",
		"workload_session_id":  workloadSession,
		"producer_id":          producerID,
		"consumer_id":          "worker-" + name,
		"redis_url":            fmt.Sprintf("rediss://worker@localhost:%d/0", redisPort),
		"redis_password_path":  redisPasswordPath,
		"redis_stream":         commandStream,
		"redis_group":          consumerGroup,
		"control_target":       fmt.Sprintf("localhost:%d", controlPort),
		"output_target":        fmt.Sprintf("localhost:%d", outputPort),
		"content_origin":       fmt.Sprintf("https://localhost:%d", contentPort),
		"platform_origin":      fmt.Sprintf("https://localhost:%d", platformPort),
		"ca_path":              pki.caPath,
		"certificate_path":     pki.workerCertPath,
		"private_key_path":     pki.workerKeyPath,
		"ed25519_keyring_path": keyringPath,
		"spool_root":           spoolRoot,
		"spool_key_path":       spoolKeyPath,
		"limits": map[string]any{
			"redis_read_batch":               4,
			"redis_block_millis":             250,
			"redis_reclaim_idle_millis":      testReclaimIdle.Milliseconds(),
			"redis_reclaim_interval_millis":  250,
			"dependency_retry_millis":        250,
			"delivery_max_concurrency":       2,
			"delivery_queue_capacity":        4,
			"sync_max_workers":               2,
			"sync_max_in_flight":             2,
			"admission_timeout_millis":       5000,
			"grpc_deadline_millis":           5000,
			"content_timeout_millis":         5000,
			"http_max_connections":           4,
			"http_max_keepalive_connections": 2,
			"output_max_queued_frames":       4,
			"output_max_queued_bytes":        256 * 1024,
			"output_max_sessions":            2,
			"output_ack_timeout_millis":      5000,
			"output_stream_deadline_millis":  30000,
			"lease_poll_interval_millis":     1000,
			"shutdown_timeout_millis":        10000,
		},
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "worker-"+name+".json")
	writeFile(t, path, encoded, 0o600)
	return path
}

func startWorker(t *testing.T, python, repositoryRoot, configPath, logPath string) *childProcess {
	t.Helper()
	environment := []string{
		"PYTHONPATH=" + pythonPath(repositoryRoot),
		"PYTHONDONTWRITEBYTECODE=1",
	}
	return startChild(t, "elitea-worker", logPath, repositoryRoot, environment, python, "-m", "elitea_worker", "serve", "--config", configPath)
}

type childProcess struct {
	name     string
	command  *exec.Cmd
	done     chan struct{}
	logPath  string
	mu       sync.Mutex
	waitErr  error
	stopOnce sync.Once
}

func startChild(t *testing.T, name, logPath, directory string, environment []string, executable string, args ...string) *childProcess {
	t.Helper()
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), environment...)
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start %s: %v", name, err)
	}
	child := &childProcess{name: name, command: command, done: make(chan struct{}), logPath: logPath}
	go func() {
		err := command.Wait()
		_ = logFile.Close()
		child.mu.Lock()
		child.waitErr = err
		child.mu.Unlock()
		close(child.done)
	}()
	return child
}

func (p *childProcess) ensureRunning(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		p.mu.Lock()
		err := p.waitErr
		p.mu.Unlock()
		t.Fatalf("%s exited early: %v\n%s", p.name, err, p.logs())
	default:
	}
}

func (p *childProcess) stop(t *testing.T) {
	t.Helper()
	p.stopOnce.Do(func() {
		select {
		case <-p.done:
			return
		default:
		}
		if err := p.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("signal %s: %v", p.name, err)
			return
		}
		select {
		case <-p.done:
		case <-time.After(15 * time.Second):
			_ = p.command.Process.Kill()
			<-p.done
			t.Errorf("%s did not stop within its drain deadline\n%s", p.name, p.logs())
		}
	})
}

func (p *childProcess) logs() string {
	contents, _ := os.ReadFile(p.logPath)
	return string(contents)
}

func waitForMain(t *testing.T, ctx context.Context, publicBaseURL string, process *childProcess) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		process.ensureRunning(t)
		response, err := client.Get(publicBaseURL + "/healthz")
		if err != nil {
			return false, nil
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK, nil
	}); err != nil {
		t.Fatalf("elitea-main did not become ready: %v\n%s", err, process.logs())
	}
}

func waitForWorkerConsumer(t *testing.T, ctx context.Context, client *redis.Client, consumer string, process *childProcess) {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		process.ensureRunning(t)
		consumers, err := client.XInfoConsumers(ctx, commandStream, consumerGroup).Result()
		if err != nil {
			return false, nil
		}
		for _, item := range consumers {
			if item.Name == consumer {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("worker did not join the Redis consumer group: %v\n%s", err, process.logs())
	}
}

func waitForPendingDelivery(t *testing.T, ctx context.Context, client *redis.Client, pool *pgxpool.Pool, executionID, consumer string, process *childProcess) {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		process.ensureRunning(t)
		length, err := client.XLen(ctx, commandStream).Result()
		if err != nil || length != 1 {
			return false, nil
		}
		pending, err := client.XPendingExt(ctx, &redis.XPendingExtArgs{
			Stream: commandStream,
			Group:  consumerGroup,
			Start:  "-",
			End:    "+",
			Count:  1,
		}).Result()
		if err != nil || len(pending) != 1 || pending[0].Consumer != consumer {
			return false, nil
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, executionID).Scan(&state); err != nil {
			return false, nil
		}
		return state == "DISPATCHED", nil
	}); err != nil {
		t.Fatalf("unauthorized worker %q did not leave one pending reference: %v\n%s", consumer, err, process.logs())
	}
}

func agePendingDelivery(t *testing.T, ctx context.Context, port int, caPath, consumer string) {
	t.Helper()
	worker := newControlRedisClient(t, port, "worker", workerPassword, caPath)
	defer func() { _ = worker.Close() }()
	pending, err := worker.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: commandStream,
		Group:  consumerGroup,
		Start:  "-",
		End:    "+",
		Count:  1,
	}).Result()
	if err != nil || len(pending) != 1 || pending[0].Consumer != consumer {
		t.Fatalf("locate pending delivery owned by %q before accelerated reclaim: pending=%v err=%v", consumer, pending, err)
	}
	result, err := worker.Do(
		ctx,
		"XCLAIM",
		commandStream,
		consumerGroup,
		consumer,
		0,
		pending[0].ID,
		"IDLE",
		testReclaimIdle.Milliseconds(),
		"JUSTID",
	).Result()
	if err != nil || result == nil {
		t.Fatalf("age pending delivery for accelerated reclaim: result=%v err=%v", result, err)
	}
}

func waitForSettlementAndRetirement(t *testing.T, ctx context.Context, pool *pgxpool.Pool, client *redis.Client, executionID string, process *childProcess) {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		process.ensureRunning(t)
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, executionID).Scan(&state); err != nil {
			return false, nil
		}
		if state != "SUCCEEDED" {
			return false, nil
		}
		length, err := client.XLen(ctx, commandStream).Result()
		if err != nil || length != 0 {
			return false, nil
		}
		pending, err := client.XPending(ctx, commandStream, consumerGroup).Result()
		if err != nil || pending.Count != 0 {
			return false, nil
		}
		mappings, err := client.HLen(ctx, commandStream+":delivery-index.v1").Result()
		return err == nil && mappings == 0, nil
	}); err != nil {
		t.Fatalf("runtime did not durably settle and atomically XACK+XDEL+HDEL the command: %v\n%s", err, process.logs())
	}
	entries, err := client.XRangeN(ctx, commandStream, "-", "+", 1).Result()
	if err != nil || len(entries) != 0 {
		t.Fatalf("Redis retained output or settings after settlement: entries=%v err=%v", entries, err)
	}
}

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
