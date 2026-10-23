package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI is the slice of the Kubernetes API the tool calls: a deletecollection
// that marks objects for deletion, a list that shows them until the
// "operator" (finishAfter list calls) removes them, and a merge patch that
// drops the finalizers.
type fakeAPI struct {
	mu          sync.Mutex
	objects     map[string][]string // resource -> names
	served      map[string]bool
	deleted     map[string]bool
	finishAfter int // lists before the operator finishes; <0: never
	lists       int
	patched     []string
	selectors   []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/apis/keda.sh/v1alpha1/namespaces/elitea/"), "/")
	resource := parts[0]
	if !f.served[resource] {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		f.selectors = append(f.selectors, r.URL.Query().Get("labelSelector"))
		f.deleted[resource] = true
		_, _ = io.WriteString(w, `{}`)
	case http.MethodGet:
		f.lists++
		if f.deleted[resource] && f.finishAfter >= 0 && f.lists > f.finishAfter {
			f.objects[resource] = nil
		}
		items := []map[string]any{}
		for _, name := range f.objects[resource] {
			items = append(items, map[string]any{"metadata": map[string]any{"name": name}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case http.MethodPatch:
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/merge-patch+json" || !strings.Contains(string(body), `"finalizers":null`) {
			http.Error(w, "bad patch", http.StatusBadRequest)
			return
		}
		name := parts[1]
		f.patched = append(f.patched, resource+"/"+name)
		kept := []string{}
		for _, n := range f.objects[resource] {
			if n != name {
				kept = append(kept, n)
			}
		}
		f.objects[resource] = kept
		_, _ = io.WriteString(w, `{}`)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func run(t *testing.T, api *fakeAPI, timeout time.Duration) error {
	t.Helper()
	server := httptest.NewTLSServer(api)
	t.Cleanup(server.Close)
	c := &client{base: server.URL, token: "test-token", http: server.Client()}
	pollInterval = 10 * time.Millisecond
	cfg := config{namespace: "elitea", selector: "app.kubernetes.io/instance=elitea", timeout: timeout}
	return cleanup(context.Background(), c, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestCleanupWaitsForTheOperatorToFinish(t *testing.T) {
	api := &fakeAPI{
		objects:     map[string][]string{"scaledobjects": {"elitea-worker", "elitea-deepwiki"}, "triggerauthentications": {"elitea-deepwiki-keda-postgresql"}},
		served:      map[string]bool{"scaledobjects": true, "triggerauthentications": true},
		deleted:     map[string]bool{},
		finishAfter: 4,
	}
	if err := run(t, api, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(api.patched) != 0 {
		t.Fatalf("finalizers were removed although the operator finished: %v", api.patched)
	}
	if len(api.selectors) != 2 {
		t.Fatalf("deletecollection calls = %d, want one per kind", len(api.selectors))
	}
	for _, selector := range api.selectors {
		if selector != "app.kubernetes.io/instance=elitea" {
			t.Fatalf("selector = %q: the deletion must name the release", selector)
		}
	}
}

func TestCleanupRemovesFinalizersWhenTheOperatorIsGone(t *testing.T) {
	api := &fakeAPI{
		objects:     map[string][]string{"scaledobjects": {"elitea-worker"}},
		served:      map[string]bool{"scaledobjects": true, "triggerauthentications": true},
		deleted:     map[string]bool{},
		finishAfter: -1,
	}
	if err := run(t, api, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if len(api.patched) != 1 || api.patched[0] != "scaledobjects/elitea-worker" {
		t.Fatalf("patched = %v, want the one stuck ScaledObject", api.patched)
	}
}

func TestCleanupWithoutKedaIsANoOp(t *testing.T) {
	api := &fakeAPI{objects: map[string][]string{}, served: map[string]bool{}, deleted: map[string]bool{}}
	if err := run(t, api, time.Second); err != nil {
		t.Fatalf("a cluster that does not serve keda.sh must not fail the uninstall: %v", err)
	}
}

func TestConfigRefusesAnEmptySelector(t *testing.T) {
	env := map[string]string{"ELITEA_KEDA_CLEANUP_NAMESPACE": "elitea", "ELITEA_KEDA_CLEANUP_SELECTOR": " "}
	if _, err := configFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err == nil {
		t.Fatal("an empty selector would delete other releases' objects; it must be refused")
	}
	env["ELITEA_KEDA_CLEANUP_SELECTOR"] = "app.kubernetes.io/instance=elitea"
	env["ELITEA_KEDA_CLEANUP_TIMEOUT"] = "nope"
	if _, err := configFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err == nil {
		t.Fatal("a bad timeout must be refused")
	}
	env["ELITEA_KEDA_CLEANUP_TIMEOUT"] = "90s"
	cfg, err := configFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil || cfg.timeout != 90*time.Second {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
}
