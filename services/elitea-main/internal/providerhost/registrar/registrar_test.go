package registrar_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/proxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/proxy/providertest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/registrar"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhub"
)

type fakeStore struct {
	mu            sync.Mutex
	registrations []providerhub.Registration
	health        []struct {
		project  int64
		provider string
		healthy  bool
		detail   string
	}
}

func (f *fakeStore) Register(_ context.Context, in providerhub.Registration) (providerhub.Admitted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registrations = append(f.registrations, in)
	return providerhub.Admitted{RevisionID: "rev", ManifestDigest: providerhub.Digest(in.Manifest), Status: "inactive"}, nil
}

func (f *fakeStore) RecordHealth(_ context.Context, project int64, provider string, healthy bool, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health = append(f.health, struct {
		project  int64
		provider string
		healthy  bool
		detail   string
	}{project, provider, healthy, detail})
	return nil
}

// provider is a fake provider service behind the real gRPC hop with mutual
// TLS: a descriptor and a health answer, both swappable, plus a switch that
// makes it fail.
type provider struct {
	mu         sync.Mutex
	seen       []http.Header
	descriptor string
	health     string
	healthCode int
	down       bool
	cfg        facade.Config
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	p := &provider{descriptor: `{"name": "deepwiki", "provided_toolkits": []}`, health: `{"status": "UP"}`, healthCode: 200}
	p.cfg = providertest.Config(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.seen = append(p.seen, r.Header.Clone())
		if p.down {
			http.Error(w, "gateway down", http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/descriptor":
			_, _ = w.Write([]byte(p.descriptor))
		case "/health":
			w.WriteHeader(p.healthCode)
			_, _ = w.Write([]byte(p.health))
		default:
			http.NotFound(w, r)
		}
	}))
	return p
}

func newRegistrar(t *testing.T, p *provider, store registrar.Store, interval time.Duration) *registrar.Registrar {
	t.Helper()
	hop, err := proxy.New(p.cfg, "TEST_PLATFORM_GRPC_TARGET", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hop.Close() })
	r, err := registrar.New(p.cfg, "TEST_PLATFORM_GRPC_TARGET", store, registrar.Options{
		ProjectID: 7, Actor: "facade:test", Interval: interval, Timeout: 2 * time.Second,
		Provider: hop.Client(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOnceRegistersTheDescriptorUnderThePublicProjectAndRecordsHealth(t *testing.T) {
	p := newProvider(t)
	store := &fakeStore{}
	r := newRegistrar(t, p, store, time.Hour)
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.registrations) != 1 {
		t.Fatalf("registrations %d", len(store.registrations))
	}
	reg := store.registrations[0]
	if reg.ProjectID != 7 || reg.ProviderID != "deepwiki" || reg.Actor != "facade:test" || string(reg.Manifest) != p.descriptor {
		t.Fatalf("%+v", reg)
	}
	// The origin is the gRPC listener, grpcs://host:port.
	if reg.Origin != "grpcs://"+p.cfg.Target {
		t.Fatalf("origin %q", reg.Origin)
	}
	if r.ProviderID() != "deepwiki" {
		t.Fatal(r.ProviderID())
	}
	if len(store.health) != 1 || !store.health[0].healthy || store.health[0].provider != "deepwiki" || store.health[0].project != 7 || !strings.HasPrefix(store.health[0].detail, "health UP in ") {
		t.Fatalf("health %+v", store.health)
	}
	// Both calls were signed as the facade, under the registration's
	// project: the provider's identity gate admits only Health unsigned.
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) != 2 {
		t.Fatalf("%d requests", len(p.seen))
	}
	for _, h := range p.seen {
		if !strings.HasPrefix(h.Get("X-Elitea-Identity-Signature"), "sha256=") || h.Get("X-Elitea-Project-Id") != "7" {
			t.Fatalf("unsigned read: %v", h)
		}
	}
}

func TestTheSameDescriptorIsNotRegisteredTwiceButAChangedOneIs(t *testing.T) {
	p := newProvider(t)
	store := &fakeStore{}
	r := newRegistrar(t, p, store, time.Hour)
	for i := 0; i < 3; i++ {
		if err := r.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.registrations) != 1 || len(store.health) != 3 {
		t.Fatalf("registrations %d, health %d", len(store.registrations), len(store.health))
	}
	p.mu.Lock()
	p.descriptor = `{"name": "deepwiki", "provided_toolkits": [{"name": "Wikis"}]}`
	p.mu.Unlock()
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.registrations) != 2 || providerhub.Digest(store.registrations[1].Manifest) == providerhub.Digest(store.registrations[0].Manifest) {
		t.Fatalf("a changed descriptor was not re-registered: %d", len(store.registrations))
	}
}

func TestAnUnhealthyOrUnreachableProviderIsRecordedNotHidden(t *testing.T) {
	p := newProvider(t)
	store := &fakeStore{}
	r := newRegistrar(t, p, store, time.Hour)
	p.mu.Lock()
	p.health, p.healthCode = `{"status": "DOWN"}`, 200
	p.mu.Unlock()
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.health[0].healthy || !strings.Contains(store.health[0].detail, `status="DOWN"`) {
		t.Fatalf("%+v", store.health[0])
	}
	p.mu.Lock()
	p.health, p.healthCode = `{"status": "UP"}`, 503
	p.mu.Unlock()
	_ = r.Once(context.Background())
	if store.health[1].healthy || !strings.Contains(store.health[1].detail, "health probe failed (Unavailable)") {
		t.Fatalf("%+v", store.health[1])
	}
	// Down entirely: the descriptor cannot be read, so nothing is registered
	// and the error names the descriptor, not the health.
	p.mu.Lock()
	p.down = true
	p.mu.Unlock()
	err := r.Once(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read the provider descriptor") || !strings.Contains(err.Error(), "gateway down") {
		t.Fatalf("%v", err)
	}
	if len(store.health) != 2 {
		t.Fatal("health was recorded for a provider whose descriptor could not be read")
	}
}

func TestADescriptorWithoutANameIsRefused(t *testing.T) {
	p := newProvider(t)
	p.descriptor = `{"provided_toolkits": []}`
	store := &fakeStore{}
	r := newRegistrar(t, p, store, time.Hour)
	if err := r.Once(context.Background()); err == nil || !strings.Contains(err.Error(), "carries no name") {
		t.Fatalf("%v", err)
	}
	if len(store.registrations) != 0 {
		t.Fatal("a nameless descriptor was registered")
	}
}

func TestRunRetriesAProviderThatIsDownAtBoot(t *testing.T) {
	p := newProvider(t)
	p.down = true
	store := &fakeStore{}
	r := newRegistrar(t, p, store, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	p.mu.Lock()
	p.down = false
	p.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		n := len(store.registrations)
		store.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.registrations) != 1 {
		t.Fatalf("registrations %d", len(store.registrations))
	}
}

func TestNewRefusesWhatItCannotRegisterWith(t *testing.T) {
	if _, err := registrar.New(facade.Config{Target: "p:9443"}, "X", nil, registrar.Options{ProjectID: 1}, nil); err == nil {
		t.Fatal("no store accepted")
	}
	if _, err := registrar.New(facade.Config{Target: "p:9443"}, "X", &fakeStore{}, registrar.Options{ProjectID: 0}, nil); err == nil {
		t.Fatal("no project accepted")
	}
	if _, err := registrar.New(facade.Config{Target: "https://p:8080"}, "X", &fakeStore{}, registrar.Options{ProjectID: 1}, nil); err == nil {
		t.Fatal("a URL target (the retired BASE_URL) accepted")
	}
	// The registrar dials nothing of its own: it rides the facade's client.
	if _, err := registrar.New(facade.Config{Target: "p:9443"}, "X", &fakeStore{}, registrar.Options{ProjectID: 1}, nil); err == nil {
		t.Fatal("no provider client accepted")
	}
}

// A provider that starts after elitea-main (the usual order in a fresh
// install) is registered within the first retry pause, not a full Interval
// later: failures retry from 2s, doubling, capped at Interval.
func TestRunRetriesAFailedProbeLongBeforeTheInterval(t *testing.T) {
	p := newProvider(t)
	p.down = true
	store := &fakeStore{}
	r := newRegistrar(t, p, store, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	p.mu.Lock()
	p.down = false
	p.mu.Unlock()
	deadline := time.Now().Add(6 * time.Second)
	registered := false
	for time.Now().Before(deadline) && !registered {
		store.mu.Lock()
		registered = len(store.registrations) == 1
		store.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if !registered {
		t.Fatal("a provider that came up after boot was not registered before the next hourly probe")
	}
}
