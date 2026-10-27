package vault

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeKV is a small KV v2 + JWT-login server for unit tests.
type fakeKV struct {
	t *testing.T

	mu             sync.Mutex
	mountCAS       bool
	pathCAS        map[string]bool
	customMetadata map[string]string
	versions       map[string][]fakeVersion
	tokens         map[string]string // token -> role
	allowedRoles   map[string]bool
	rejectOnce     map[string]bool // token -> reject next authenticated call with 403

	logins      atomic.Int64
	loginDelay  time.Duration
	namespace   atomic.Value
	lastToken   atomic.Value
	override    func(w http.ResponseWriter, r *http.Request) bool
	authedCalls atomic.Int64
}

type fakeVersion struct {
	value     string
	created   time.Time
	destroyed bool
}

func newFakeKV(t *testing.T) *fakeKV {
	return &fakeKV{
		t:            t,
		mountCAS:     true,
		pathCAS:      map[string]bool{},
		versions:     map[string][]fakeVersion{},
		tokens:       map[string]string{},
		allowedRoles: map[string]bool{"elitea-agent-runtime": true, "elitea-readiness": true},
		rejectOnce:   map[string]bool{},
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func vaultError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string][]string{"errors": {message}})
}

func (f *fakeKV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.namespace.Store(r.Header.Get("X-Vault-Namespace"))
	if f.override != nil && f.override(w, r) {
		return
	}
	if r.URL.Path == "/v1/auth/jwt/login" && r.Method == http.MethodPost {
		f.login(w, r)
		return
	}
	token := r.Header.Get("X-Vault-Token")
	f.lastToken.Store(token)
	f.authedCalls.Add(1)
	f.mu.Lock()
	_, known := f.tokens[token]
	reject := f.rejectOnce[token]
	delete(f.rejectOnce, token)
	f.mu.Unlock()
	if !known || reject {
		vaultError(w, http.StatusForbidden, "permission denied")
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/secret/")
	if !ok {
		vaultError(w, http.StatusNotFound, "no handler for route "+r.URL.Path)
		return
	}
	kind, path, _ := strings.Cut(rest, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case kind == "config" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"cas_required": f.mountCAS, "max_versions": 0}})
	case kind == "metadata" && r.Method == http.MethodGet:
		versions, exists := f.versions[path]
		if !exists {
			vaultError(w, http.StatusNotFound, "")
			return
		}
		entries := map[string]any{}
		for index, version := range versions {
			entries[strconv.Itoa(index+1)] = map[string]any{
				"created_time": version.created.Format(time.RFC3339Nano), "deletion_time": "", "destroyed": version.destroyed,
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"cas_required": f.pathCAS[path], "current_version": len(versions), "versions": entries,
			"custom_metadata": f.customMetadata,
		}})
	case kind == "data" && r.Method == http.MethodGet:
		number, err := strconv.Atoi(r.URL.Query().Get("version"))
		versions := f.versions[path]
		if err != nil || number <= 0 {
			f.t.Errorf("a read without an explicit version reached the backend: %q", r.URL.RawQuery)
			vaultError(w, http.StatusBadRequest, "version required")
			return
		}
		if number > len(versions) {
			vaultError(w, http.StatusNotFound, "")
			return
		}
		version := versions[number-1]
		meta := map[string]any{"version": number, "created_time": version.created.Format(time.RFC3339Nano), "deletion_time": "", "destroyed": version.destroyed,
			"custom_metadata": f.customMetadata}
		if version.destroyed {
			writeJSON(w, http.StatusNotFound, map[string]any{"data": map[string]any{"data": nil, "metadata": meta}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"data": map[string]string{"value": version.value}, "metadata": meta}})
	case kind == "data" && r.Method == http.MethodPost:
		var body struct {
			Options map[string]int    `json:"options"`
			Data    map[string]string `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			vaultError(w, http.StatusBadRequest, "bad body")
			return
		}
		cas, hasCAS := body.Options["cas"]
		if (f.mountCAS || f.pathCAS[path]) && !hasCAS {
			vaultError(w, http.StatusBadRequest, "check-and-set parameter required for this call")
			return
		}
		if hasCAS && cas != len(f.versions[path]) {
			vaultError(w, http.StatusBadRequest, "check-and-set parameter did not match the current version")
			return
		}
		f.versions[path] = append(f.versions[path], fakeVersion{value: body.Data["value"], created: time.Now().UTC()})
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"version": len(f.versions[path])}})
	case kind == "destroy" && r.Method == http.MethodPost:
		var body struct {
			Versions []int `json:"versions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, number := range body.Versions {
			if number > 0 && number <= len(f.versions[path]) {
				f.versions[path][number-1].destroyed = true
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		vaultError(w, http.StatusNotFound, "")
	}
}

func (f *fakeKV) login(w http.ResponseWriter, r *http.Request) {
	f.logins.Add(1)
	if f.loginDelay > 0 {
		time.Sleep(f.loginDelay)
	}
	var body struct {
		Role string `json:"role"`
		JWT  string `json:"jwt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.JWT != "workload-jwt" {
		vaultError(w, http.StatusBadRequest, "missing jwt")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.allowedRoles[body.Role] {
		vaultError(w, http.StatusBadRequest, fmt.Sprintf("role %q could not be found", body.Role))
		return
	}
	token := fmt.Sprintf("tok-%d", f.logins.Load())
	f.tokens[token] = body.Role
	writeJSON(w, http.StatusOK, map[string]any{"auth": map[string]any{"client_token": token, "lease_duration": 3600, "renewable": true}})
}

func (f *fakeKV) seed(path string, values ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, value := range values {
		f.versions[path] = append(f.versions[path], fakeVersion{value: value, created: time.Now().UTC()})
	}
}

// harness starts a TLS fake and returns a Store trusting its certificate.
type harness struct {
	fake   *fakeKV
	server *httptest.Server
	store  *Store
	cfg    Config
	dir    string
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	fake := newFakeKV(t)
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("workload-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addr, _ := url.Parse(server.URL)
	cfg := Config{
		Addr: addr, CAPEM: caPEM, AuthMethod: AuthJWT, AuthMount: "jwt", RolePrefix: "elitea",
		KVMount: "secret", TokenFile: tokenFile, ReadinessScope: "readiness",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	store, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{fake: fake, server: server, store: store, cfg: cfg, dir: dir}
}

// env returns a lookup for a complete, valid configuration of this harness.
func (h *harness) env(overrides map[string]string) func(string) (string, bool) {
	values := map[string]string{
		EnvAddr: h.server.URL, EnvCAFile: filepath.Join(h.dir, "ca.pem"), EnvAuthMethod: "jwt",
		EnvAuthMount: "jwt", EnvRolePrefix: "elitea", EnvKVMount: "secret",
		EnvTokenFile: filepath.Join(h.dir, "token"), EnvReadinessScope: "readiness",
	}
	for key, value := range overrides {
		if value == "" {
			delete(values, key)
			continue
		}
		values[key] = value
	}
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
