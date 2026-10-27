package vault

// The adapter against a real OpenBao (deploy/docker-compose.secretstore.yml).
// The test provisions everything it uses through the root token: a JWT auth
// method that trusts a key generated here, a policy, a role, and two KV v2
// mounts, one with cas_required and one without. The adapter itself then logs
// in with a JWT signed by that key, exactly as a workload would.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

const (
	envOpenBaoAddr      = "ELITEA_TEST_OPENBAO_ADDR"
	envOpenBaoCAFile    = "ELITEA_TEST_OPENBAO_CA_FILE"
	envOpenBaoRootToken = "ELITEA_TEST_OPENBAO_ROOT_TOKEN"
)

type openBaoAdmin struct {
	t     *testing.T
	addr  string
	token string
	http  *http.Client
}

func (a openBaoAdmin) call(method, path string, body any) []byte {
	a.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, a.addr+path, reader)
	if err != nil {
		a.t.Fatal(err)
	}
	request.Header.Set("X-Vault-Token", a.token)
	response, err := a.http.Do(request)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 300 {
		a.t.Fatalf("%s %s = %d %s", method, path, response.StatusCode, raw)
	}
	return raw
}

func signJWT(t *testing.T, key *ecdsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	unsigned := encode(map[string]string{"alg": "ES256", "typ": "JWT"}) + "." + encode(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

type openBaoFixture struct {
	store     *Store
	cfg       Config
	casMount  string
	openMount string
}

func newOpenBaoFixture(t *testing.T) openBaoFixture {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv(envOpenBaoAddr))
	if addr == "" {
		// The secretstore-openbao job in ci-go.yml sets the require flag, so
		// a missing fixture fails there instead of skipping.
		if os.Getenv("ELITEA_REQUIRE_OPENBAO_TEST") == "true" {
			t.Fatalf("ELITEA_REQUIRE_OPENBAO_TEST=true but %s is unset", envOpenBaoAddr)
		}
		t.Skipf("set %s, %s and %s (deploy/docker-compose.secretstore.yml) to run the OpenBao integration test",
			envOpenBaoAddr, envOpenBaoCAFile, envOpenBaoRootToken)
	}
	caPEM, err := os.ReadFile(os.Getenv(envOpenBaoCAFile))
	if err != nil {
		t.Fatalf("read %s: %v", envOpenBaoCAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("%s holds no certificate", envOpenBaoCAFile)
	}
	admin := openBaoAdmin{t: t, addr: addr, token: os.Getenv(envOpenBaoRootToken), http: &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	authMount := "jwt-it-" + suffix
	casMount := "kv-cas-" + suffix
	openMount := "kv-open-" + suffix
	policy := "secretstore-it-" + suffix

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))

	admin.call(http.MethodPost, "/v1/sys/mounts/"+casMount, map[string]any{"type": "kv", "options": map[string]string{"version": "2"}})
	admin.call(http.MethodPost, "/v1/"+casMount+"/config", map[string]any{"cas_required": true})
	admin.call(http.MethodPost, "/v1/sys/mounts/"+openMount, map[string]any{"type": "kv", "options": map[string]string{"version": "2"}})
	var rules strings.Builder
	for _, mount := range []string{casMount, openMount} {
		fmt.Fprintf(&rules, `path "%[1]s/data/*" { capabilities = ["create", "read", "update"] }
path "%[1]s/metadata/*" { capabilities = ["read"] }
path "%[1]s/destroy/*" { capabilities = ["update"] }
path "%[1]s/config" { capabilities = ["read"] }
`, mount)
	}
	admin.call(http.MethodPut, "/v1/sys/policies/acl/"+policy, map[string]string{"policy": rules.String()})
	admin.call(http.MethodPost, "/v1/sys/auth/"+authMount, map[string]string{"type": "jwt"})
	admin.call(http.MethodPost, "/v1/auth/"+authMount+"/config", map[string]any{
		"jwt_validation_pubkeys": []string{publicPEM}, "bound_issuer": "elitea-it",
	})
	for _, scope := range []string{"agent-runtime", "readiness"} {
		admin.call(http.MethodPost, "/v1/auth/"+authMount+"/role/elitea-"+scope, map[string]any{
			"role_type": "jwt", "bound_audiences": []string{"elitea-secretstore"}, "user_claim": "sub",
			"bound_subject":  "system:serviceaccount:elitea:elitea-main",
			"token_policies": []string{policy}, "token_ttl": "10m",
		})
	}
	t.Cleanup(func() {
		admin.call(http.MethodDelete, "/v1/sys/mounts/"+casMount, nil)
		admin.call(http.MethodDelete, "/v1/sys/mounts/"+openMount, nil)
		admin.call(http.MethodDelete, "/v1/sys/auth/"+authMount, nil)
		admin.call(http.MethodDelete, "/v1/sys/policies/acl/"+policy, nil)
	})

	now := time.Now()
	jwt := signJWT(t, key, map[string]any{
		"iss": "elitea-it", "sub": "system:serviceaccount:elitea:elitea-main", "aud": "elitea-secretstore",
		"iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(30 * time.Minute).Unix(),
	})
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(jwt), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(addr)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Addr: parsed, CAPEM: caPEM, AuthMethod: AuthJWT, AuthMount: authMount, RolePrefix: "elitea",
		KVMount: casMount, TokenFile: tokenFile, ReadinessScope: "readiness",
	}
	store, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return openBaoFixture{store: store, cfg: cfg, casMount: casMount, openMount: openMount}
}

func TestOpenBaoKVv2Lifecycle(t *testing.T) {
	f := newOpenBaoFixture(t)
	ctx := context.Background()
	location := secretstore.SecretRef{Backend: secretstore.BackendVault, Mount: f.casMount, Path: "tenant7/database"}

	if err := NewPrerequisite(f.store).Check(ctx, profile.PolicyRevision{Document: profile.PolicyDocument{
		VaultRequiredCredentialClasses: []string{"database"},
	}}); err != nil {
		t.Fatalf("readiness = %v", err)
	}
	v1, err := f.store.Write(ctx, sc(), location, secret("first-value"), 0)
	if err != nil || v1 != 1 {
		t.Fatalf("write v1 = %d, %v", v1, err)
	}
	v2, err := f.store.Write(ctx, sc(), location, secret("second-value"), 1)
	if err != nil || v2 != 2 {
		t.Fatalf("write v2 = %d, %v", v2, err)
	}
	if _, err := f.store.Write(ctx, sc(), location, secret(valueCanary), 1); !errors.Is(err, secretstore.ErrVersionConflict) {
		t.Fatalf("stale cas = %v", err)
	} else {
		assertClean(t, err)
	}
	for version, want := range map[int64]string{1: "first-value", 2: "second-value"} {
		got, err := f.store.Read(ctx, sc(), location, version)
		if err != nil || got.Version != version || string(got.Value.UnsafeBytes()) != want {
			t.Fatalf("read v%d = %+v, %v", version, got, err)
		}
	}
	if err := f.store.Retire(ctx, sc(), location, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Read(ctx, sc(), location, 1); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("read retired v1 = %v", err)
	}
	if _, err := f.store.Read(ctx, sc(), location, 3); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("read v3 = %v", err)
	}
	meta, err := f.store.Metadata(ctx, sc(), location)
	if err != nil || meta.CurrentVersion != 2 || len(meta.Versions) != 1 || meta.Versions[0] != 2 || !meta.CASRequired {
		t.Fatalf("metadata = %+v, %v", meta, err)
	}
	if _, err := f.store.Read(ctx, sc(), location, 0); !errors.Is(err, secretstore.ErrInvalidRequest) {
		t.Fatalf("version 0 = %v", err)
	}
}

func TestOpenBaoMountWithoutCASRequiredRefusesWrites(t *testing.T) {
	f := newOpenBaoFixture(t)
	cfg := f.cfg
	cfg.KVMount = f.openMount
	store, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	location := secretstore.SecretRef{Backend: secretstore.BackendVault, Mount: f.openMount, Path: pathCanary}
	_, err = store.Write(context.Background(), sc(), location, secret(valueCanary), 0)
	if !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("write without cas_required = %v", err)
	}
	assertClean(t, err)
	if err := NewPrerequisite(store).Check(context.Background(), profile.PolicyRevision{Document: profile.PolicyDocument{
		VaultRequiredCredentialClasses: []string{"database"},
	}}); !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("readiness on a mount without cas_required = %v", err)
	}
}

func TestOpenBaoUnknownScopeIsDenied(t *testing.T) {
	f := newOpenBaoFixture(t)
	context := sc()
	context.WorkloadScope = "no-such-role"
	location := secretstore.SecretRef{Backend: secretstore.BackendVault, Mount: f.casMount, Path: "tenant7/database"}
	if _, err := f.store.Read(t.Context(), context, location, 1); !errors.Is(err, secretstore.ErrDenied) {
		t.Fatalf("unknown role = %v", err)
	}
}
