package vault

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

const (
	pathCanary  = "tenant7/pathcanary-db-password"
	valueCanary = "valuecanary-s3cr3t-9f2"
)

func sc() secretstore.StoreContext {
	return secretstore.StoreContext{
		TenantProjectID: 7, CredentialRefID: "cref-1", DecisionID: "dec-1",
		WorkloadScope: "agent-runtime", PolicyRevisionID: "2026-10-r1",
	}
}

func ref(path string) secretstore.SecretRef {
	return secretstore.SecretRef{Backend: secretstore.BackendVault, Mount: "secret", Path: path}
}

func secret(value string) secretstore.RuntimeSecret {
	return secretstore.NewRuntimeSecret([]byte(value))
}

// assertClean fails when an error string carries the path or the value.
func assertClean(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	text := err.Error()
	for _, canary := range []string{pathCanary, "pathcanary", valueCanary, "valuecanary", "/v1/"} {
		if strings.Contains(text, canary) {
			t.Fatalf("error text leaks %q: %s", canary, text)
		}
	}
}

func TestRoundTripExplicitVersions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v1, err := h.store.Write(ctx, sc(), ref(pathCanary), secret("first"), 0)
	if err != nil || v1 != 1 {
		t.Fatalf("write v1 = %d, %v", v1, err)
	}
	v2, err := h.store.Write(ctx, sc(), ref(pathCanary), secret("second"), 1)
	if err != nil || v2 != 2 {
		t.Fatalf("write v2 = %d, %v", v2, err)
	}
	for version, want := range map[int64]string{1: "first", 2: "second"} {
		got, err := h.store.Read(ctx, sc(), ref(pathCanary), version)
		if err != nil || got.Version != version || string(got.Value.UnsafeBytes()) != want || got.CreatedAt.IsZero() {
			t.Fatalf("read v%d = %+v, %v", version, got, err)
		}
	}
	if err := h.store.Retire(ctx, sc(), ref(pathCanary), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Read(ctx, sc(), ref(pathCanary), 1); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("read retired v1 = %v", err)
	}
	meta, err := h.store.Metadata(ctx, sc(), ref(pathCanary))
	if err != nil || meta.CurrentVersion != 2 || len(meta.Versions) != 1 || meta.Versions[0] != 2 || !meta.CASRequired {
		t.Fatalf("metadata = %+v, %v", meta, err)
	}
}

func TestReadVersionZeroIsRefusedWithoutACall(t *testing.T) {
	h := newHarness(t)
	h.fake.seed(pathCanary, valueCanary)
	_, err := h.store.Read(context.Background(), sc(), ref(pathCanary), 0)
	if !errors.Is(err, secretstore.ErrInvalidRequest) {
		t.Fatalf("version 0 = %v", err)
	}
	if h.fake.logins.Load() != 0 || h.fake.authedCalls.Load() != 0 {
		t.Fatal("a refused read must not reach the backend")
	}
	assertClean(t, err)
}

func TestReadWithoutDecisionIsRefused(t *testing.T) {
	h := newHarness(t)
	context := sc()
	context.DecisionID = ""
	if _, err := h.store.Read(t.Context(), context, ref(pathCanary), 1); !errors.Is(err, secretstore.ErrInvalidRequest) {
		t.Fatalf("read without decision = %v", err)
	}
}

func TestWriteWithoutCASRequiredIsMisconfigured(t *testing.T) {
	h := newHarness(t)
	h.fake.mountCAS = false
	_, err := h.store.Write(context.Background(), sc(), ref(pathCanary), secret(valueCanary), 0)
	if !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("write without cas_required = %v", err)
	}
	assertClean(t, err)
	// A path-level cas_required is enough.
	h.fake.seed(pathCanary, "v1")
	h.fake.pathCAS[pathCanary] = true
	if v, err := h.store.Write(context.Background(), sc(), ref(pathCanary), secret(valueCanary), 1); err != nil || v != 2 {
		t.Fatalf("write with path cas_required = %d, %v", v, err)
	}
}

func TestWriteWithStaleCASIsVersionConflict(t *testing.T) {
	h := newHarness(t)
	h.fake.seed(pathCanary, "v1", "v2")
	_, err := h.store.Write(context.Background(), sc(), ref(pathCanary), secret(valueCanary), 1)
	if !errors.Is(err, secretstore.ErrVersionConflict) {
		t.Fatalf("stale cas = %v", err)
	}
	assertClean(t, err)
}

// A write the backend answers with a version other than cas+1 created that
// version: the adapter returns it with ErrUnexpectedVersion (not a refusal),
// so the ledger can record and retire it.
func TestWriteWithUnexpectedVersionReturnsTheCreatedVersion(t *testing.T) {
	h := newHarness(t)
	h.fake.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/secret/data/") {
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"version": 5}})
			return true
		}
		return false
	}
	version, err := h.store.Write(context.Background(), sc(), ref(pathCanary), secret(valueCanary), 0)
	if !errors.Is(err, secretstore.ErrUnexpectedVersion) || errors.Is(err, secretstore.ErrMisconfigured) || version != 5 {
		t.Fatalf("unexpected version = %d, %v", version, err)
	}
	assertClean(t, err)
}

// custom_metadata carries no authority: a key that claims cas_required or a
// different current version changes nothing.
func TestCustomMetadataNeverChangesADecision(t *testing.T) {
	h := newHarness(t)
	h.fake.mountCAS = false
	h.fake.seed(pathCanary, "v1")
	h.fake.customMetadata = map[string]string{
		"cas_required": "true", "current_version": "99", "elitea_allow": "true", "versions": "1,2,3",
	}
	if _, err := h.store.Write(context.Background(), sc(), ref(pathCanary), secret(valueCanary), 1); !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("write with misleading custom_metadata = %v", err)
	}
	meta, err := h.store.Metadata(context.Background(), sc(), ref(pathCanary))
	if err != nil || meta.CASRequired || meta.CurrentVersion != 1 || len(meta.Versions) != 1 {
		t.Fatalf("metadata read custom_metadata: %+v, %v", meta, err)
	}
	got, err := h.store.Read(context.Background(), sc(), ref(pathCanary), 1)
	if err != nil || string(got.Value.UnsafeBytes()) != "v1" {
		t.Fatalf("read = %+v, %v", got, err)
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"not found", http.StatusNotFound, `{"errors":[]}`, secretstore.ErrNotFound},
		{"forbidden", http.StatusForbidden, `{"errors":["permission denied on ` + pathCanary + `"]}`, secretstore.ErrDenied},
		{"unauthorized", http.StatusUnauthorized, `{}`, secretstore.ErrDenied},
		{"cas mismatch", http.StatusBadRequest, `{"errors":["check-and-set parameter did not match the current version"]}`, secretstore.ErrVersionConflict},
		{"cas required", http.StatusBadRequest, `{"errors":["check-and-set parameter required for this call"]}`, secretstore.ErrMisconfigured},
		{"bad request", http.StatusBadRequest, `{"errors":["bad"]}`, secretstore.ErrMisconfigured},
		{"internal", http.StatusInternalServerError, `{"errors":["` + valueCanary + `"]}`, secretstore.ErrUnavailable},
		{"sealed", http.StatusServiceUnavailable, `{"errors":["Vault is sealed"]}`, secretstore.ErrUnavailable},
		{"rate limited", http.StatusTooManyRequests, `{}`, secretstore.ErrUnavailable},
		{"redirect", http.StatusTemporaryRedirect, ``, secretstore.ErrMisconfigured},
	}
	operations := map[string]func(*Store) error{
		"read": func(s *Store) error { _, err := s.Read(context.Background(), sc(), ref(pathCanary), 1); return err },
		"write": func(s *Store) error {
			_, err := s.Write(context.Background(), sc(), ref(pathCanary), secret(valueCanary), 0)
			return err
		},
		"metadata": func(s *Store) error { _, err := s.Metadata(context.Background(), sc(), ref(pathCanary)); return err },
		"retire":   func(s *Store) error { return s.Retire(context.Background(), sc(), ref(pathCanary), 1) },
	}
	for _, tc := range cases {
		for opName, operation := range operations {
			t.Run(tc.name+"/"+opName, func(t *testing.T) {
				h := newHarness(t)
				h.fake.override = func(w http.ResponseWriter, r *http.Request) bool {
					// Login, the mount config and the path metadata behave;
					// the operation's own request answers tc.status.
					if strings.HasPrefix(r.URL.Path, "/v1/auth/") || r.URL.Path == "/v1/secret/config" {
						return false
					}
					if opName == "write" && strings.HasPrefix(r.URL.Path, "/v1/secret/metadata/") {
						return false
					}
					if tc.status == http.StatusTemporaryRedirect {
						w.Header().Set("Location", "https://elsewhere.invalid/")
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return true
				}
				err := operation(h.store)
				if !errors.Is(err, tc.want) {
					t.Fatalf("status %d = %v, want %v", tc.status, err, tc.want)
				}
				assertClean(t, err)
			})
		}
	}
}

func TestTimeoutIsUnavailable(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.fake.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/v1/secret/data/") {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return true
		}
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := h.store.Read(ctx, sc(), ref(pathCanary), 1)
	if !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("timeout = %v", err)
	}
	assertClean(t, err)
}

func TestTransportFailureCarriesNoURL(t *testing.T) {
	h := newHarness(t)
	h.server.Close()
	_, err := h.store.Read(context.Background(), sc(), ref(pathCanary), 1)
	if !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("closed server = %v", err)
	}
	assertClean(t, err)
}

func TestUntrustedCertificateIsRefused(t *testing.T) {
	h := newHarness(t)
	// httptest servers share one certificate, so trust an unrelated CA.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unrelated CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.cfg
	cfg.CAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	store, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Read(context.Background(), sc(), ref(pathCanary), 1)
	if !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("untrusted certificate = %v", err)
	}
	assertClean(t, err)
	if h.fake.logins.Load() != 0 {
		t.Fatal("no request may complete over an unverified connection")
	}
}

func TestEmptyCABundleIsRefused(t *testing.T) {
	cfg := newHarness(t).cfg
	cfg.CAPEM = []byte("not a certificate")
	if _, err := New(cfg); err == nil {
		t.Fatal("a CA bundle without a certificate must refuse")
	}
}

func TestLoginRefusedIsDenied(t *testing.T) {
	h := newHarness(t)
	context := sc()
	context.WorkloadScope = "unknown-scope"
	_, err := h.store.Read(t.Context(), context, ref(pathCanary), 1)
	if !errors.Is(err, secretstore.ErrDenied) {
		t.Fatalf("unknown role = %v", err)
	}
	assertClean(t, err)
}

func TestReferenceToAnotherBackendOrMountIsRefused(t *testing.T) {
	h := newHarness(t)
	other := ref(pathCanary)
	other.Backend = secretstore.BackendPGVault
	if _, err := h.store.Read(t.Context(), sc(), other, 1); !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("pgvault ref = %v", err)
	}
	other = ref(pathCanary)
	other.Mount = "sys"
	if _, err := h.store.Read(t.Context(), sc(), other, 1); !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("other mount = %v", err)
	}
	for _, bad := range []string{"", "a/../b", "/abs", "a//b", "a/b/", "a?version=1", "a%2fb"} {
		if _, err := h.store.Read(t.Context(), sc(), ref(bad), 1); !errors.Is(err, secretstore.ErrInvalidRequest) {
			t.Fatalf("path %q = %v", bad, err)
		}
	}
	if h.fake.logins.Load() != 0 {
		t.Fatal("a refused reference must not log in")
	}
}

// Ten concurrent first logins for one role produce one login call.
func TestConcurrentFirstLoginsAreSingleFlight(t *testing.T) {
	h := newHarness(t)
	h.fake.seed(pathCanary, "v1")
	h.fake.loginDelay = 100 * time.Millisecond
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	start := make(chan struct{})
	for range 10 {
		wg.Go(func() {
			<-start
			_, err := h.store.Read(context.Background(), sc(), ref(pathCanary), 1)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := h.fake.logins.Load(); got != 1 {
		t.Fatalf("logins = %d, want 1", got)
	}
}

func TestForbiddenReLogsInOnce(t *testing.T) {
	h := newHarness(t)
	h.fake.seed(pathCanary, "v1")
	if _, err := h.store.Read(t.Context(), sc(), ref(pathCanary), 1); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	h.fake.rejectOnce["tok-1"] = true
	h.fake.mu.Unlock()
	if _, err := h.store.Read(t.Context(), sc(), ref(pathCanary), 1); err != nil {
		t.Fatalf("read after a revoked token = %v", err)
	}
	if got := h.fake.logins.Load(); got != 2 {
		t.Fatalf("logins = %d, want 2", got)
	}
	// A token that keeps being refused is a denial after one retry.
	h.fake.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/v1/secret/") {
			vaultError(w, http.StatusForbidden, "permission denied")
			return true
		}
		return false
	}
	before := h.fake.logins.Load()
	if _, err := h.store.Read(t.Context(), sc(), ref(pathCanary), 1); !errors.Is(err, secretstore.ErrDenied) {
		t.Fatalf("persistent 403 = %v", err)
	}
	if got := h.fake.logins.Load() - before; got != 1 {
		t.Fatalf("re-logins on 403 = %d, want 1", got)
	}
}

func TestRolesAreSeparateTokens(t *testing.T) {
	h := newHarness(t)
	h.fake.seed(pathCanary, "v1")
	if _, err := h.store.Read(t.Context(), sc(), ref(pathCanary), 1); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CheckReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.fake.logins.Load(); got != 2 {
		t.Fatalf("two scopes must log in as two roles, logins = %d", got)
	}
}

func TestNamespaceHeader(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Namespace = "team-a" })
	h.fake.seed(pathCanary, "v1")
	if _, err := h.store.Read(t.Context(), sc(), ref(pathCanary), 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.fake.namespace.Load().(string); got != "team-a" {
		t.Fatalf("namespace header = %q", got)
	}
	plain := newHarness(t)
	plain.fake.seed(pathCanary, "v1")
	if _, err := plain.store.Read(t.Context(), sc(), ref(pathCanary), 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := plain.fake.namespace.Load().(string); got != "" {
		t.Fatalf("namespace header without a namespace = %q", got)
	}
}

func TestReadRefusesAMismatchedOrMalformedPayload(t *testing.T) {
	h := newHarness(t)
	h.fake.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/v1/secret/data/") {
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
				"data": map[string]string{"value": valueCanary}, "metadata": map[string]any{"version": 3},
			}})
			return true
		}
		return false
	}
	if _, err := h.store.Read(t.Context(), sc(), ref(pathCanary), 1); !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("another version answered = %v", err)
	}
	h.fake.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/v1/secret/data/") {
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
				"data": map[string]any{"value": 12, "other": "x"}, "metadata": map[string]any{"version": 1},
			}})
			return true
		}
		return false
	}
	_, err := h.store.Read(t.Context(), sc(), ref(pathCanary), 1)
	if !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("unexpected shape = %v", err)
	}
	assertClean(t, err)
}

func revision(classes ...string) profile.PolicyRevision {
	return profile.PolicyRevision{ID: "2026-10-r1", Document: profile.PolicyDocument{VaultRequiredCredentialClasses: classes}}
}

func TestPrerequisite(t *testing.T) {
	if err := NewPrerequisite(nil).Check(t.Context(), revision()); err != nil {
		t.Fatalf("no class requires Vault, unconfigured = %v", err)
	}
	if err := NewPrerequisite(nil).Check(t.Context(), revision("database")); !errors.Is(err, ErrRequiredButAbsent) {
		t.Fatalf("required and unconfigured = %v", err)
	}
	h := newHarness(t)
	if err := NewPrerequisite(h.store).Check(t.Context(), revision()); err != nil || h.fake.logins.Load() != 0 {
		t.Fatalf("not required must not dial: %v, logins %d", err, h.fake.logins.Load())
	}
	if err := NewPrerequisite(h.store).Check(t.Context(), revision("database")); err != nil {
		t.Fatalf("required and ready = %v", err)
	}
	h.fake.mountCAS = false
	if err := NewPrerequisite(h.store).Check(t.Context(), revision("database")); !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("mount without cas_required = %v", err)
	}
	h.server.Close()
	if err := NewPrerequisite(h.store).Check(t.Context(), revision("database")); err == nil {
		t.Fatal("an unreachable Vault must refuse a revision that requires it")
	}
	if NewPrerequisite(nil).Name() == "" {
		t.Fatal("name is required")
	}
}

func TestConfigFromEnv(t *testing.T) {
	h := newHarness(t)
	cfg, err := ConfigFromEnv(h.env(nil))
	if err != nil {
		t.Fatalf("complete config = %v", err)
	}
	if cfg.Role("agent-runtime") != "elitea-agent-runtime" || cfg.Namespace != "" {
		t.Fatalf("config = %+v", cfg)
	}
	refusals := map[string]map[string]string{
		"http address":        {EnvAddr: "http://vault.internal:8200"},
		"no address":          {EnvAddr: ""},
		"address with path":   {EnvAddr: "https://vault.internal/v1/secret"},
		"address with user":   {EnvAddr: "https://root:x@vault.internal"},
		"no role prefix":      {EnvRolePrefix: ""},
		"bad role prefix":     {EnvRolePrefix: "a/b"},
		"no CA":               {EnvCAFile: ""},
		"missing CA file":     {EnvCAFile: "/nonexistent/ca.pem"},
		"no method":           {EnvAuthMethod: ""},
		"unknown method":      {EnvAuthMethod: "token"},
		"no auth mount":       {EnvAuthMount: ""},
		"no kv mount":         {EnvKVMount: ""},
		"bad kv mount":        {EnvKVMount: "../sys"},
		"no token file":       {EnvTokenFile: ""},
		"missing token file":  {EnvTokenFile: "/nonexistent/token"},
		"no readiness scope":  {EnvReadinessScope: ""},
		"bad namespace":       {EnvNamespace: "a b"},
		"bad readiness scope": {EnvReadinessScope: "Upper"},
	}
	for name, overrides := range refusals {
		t.Run(name, func(t *testing.T) {
			if _, err := ConfigFromEnv(h.env(overrides)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("ConfigFromEnv = %v, want ErrInvalidConfig", err)
			}
		})
	}
	if _, err := ConfigFromEnv(h.env(map[string]string{EnvAuthMethod: "kubernetes", EnvAuthMount: "kubernetes", EnvNamespace: "team-a/sub"})); err != nil {
		t.Fatalf("kubernetes with namespace = %v", err)
	}
}

func TestConfigured(t *testing.T) {
	empty := func(string) (string, bool) { return "", false }
	if Configured(empty) {
		t.Fatal("no variable set is unconfigured")
	}
	blank := func(name string) (string, bool) { return "  ", true }
	if Configured(blank) {
		t.Fatal("blank values are unconfigured")
	}
	one := func(name string) (string, bool) {
		if name == EnvKVMount {
			return "secret", true
		}
		return "", false
	}
	if !Configured(one) {
		t.Fatal("one variable set is configured")
	}
}
