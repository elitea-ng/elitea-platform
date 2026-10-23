package admission_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/admission"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhub"
)

// plane is the admission plane a test can steer, and count.
type plane struct {
	present bool
	status  string
	found   bool
	err     error

	presentCalls, latestCalls int
	lastProject               int64
	lastProvider              string
}

func (p *plane) Present(context.Context) bool {
	p.presentCalls++
	return p.present
}

func (p *plane) Latest(_ context.Context, projectID int64, providerID string) (providerhub.Admitted, bool, error) {
	p.latestCalls++
	p.lastProject, p.lastProvider = projectID, providerID
	if p.err != nil {
		return providerhub.Admitted{}, false, p.err
	}
	if !p.found {
		return providerhub.Admitted{}, false, nil
	}
	return providerhub.Admitted{RevisionID: "1:wikis:abc", Status: p.status, Reason: "because"}, true, nil
}

func request() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/x/tools/90200/Wikis/generate_wiki/invoke", nil)
}

func gate(t *testing.T, store admission.Store, posture facade.AdmissionPosture, clock func() time.Time) *admission.Gate {
	t.Helper()
	return admission.New(admission.Config{
		Store:      store,
		ProjectID:  1,
		ProviderID: func() string { return "wikis" },
		Posture:    posture,
		Clock:      clock,
	})
}

func TestTheFourStatesInBothPostures(t *testing.T) {
	for _, c := range []struct {
		name       string
		status     string
		found      bool
		posture    facade.AdmissionPosture
		wantAllow  bool
		wantReason string
	}{
		// REVOKED IS CLOSED IN BOTH POSTURES. `record` is about what this
		// deployment does with a provider nobody has decided on; a revocation
		// is a decision, and a posture that could ignore it would make the
		// administration surface's one action advisory.
		{"revoked, recording", "revoked", true, facade.AdmissionRecord, false, admission.ReasonRevoked},
		{"revoked, enforcing", "revoked", true, facade.AdmissionEnforce, false, admission.ReasonRevoked},

		// INACTIVE IS WHERE THE POSTURE LIVES, and the only place it does.
		{"inactive, recording", "inactive", true, facade.AdmissionRecord, true, ""},
		{"inactive, enforcing", "inactive", true, facade.AdmissionEnforce, false, admission.ReasonInactive},

		// ACTIVE is admitted, in both. Unreachable today (0107's CHECK
		// constraint needs an overlay revision nobody issues) and asserted
		// anyway, because the branch is what an overlay issuer will land on.
		{"active, recording", "active", true, facade.AdmissionRecord, true, ""},
		{"active, enforcing", "active", true, facade.AdmissionEnforce, true, ""},

		// No revision at all: registered a moment ago, or never. Not a
		// revocation, and refusing it would refuse every boot's first seconds.
		{"no revision, recording", "", false, facade.AdmissionRecord, true, ""},
		{"no revision, enforcing", "", false, facade.AdmissionEnforce, true, ""},

		// A status a later migration adds that this build does not know is
		// treated as undecided, not as admitted.
		{"unknown status, enforcing", "quarantined", true, facade.AdmissionEnforce, false, admission.ReasonInactive},
		{"unknown status, recording", "quarantined", true, facade.AdmissionRecord, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &plane{present: true, status: c.status, found: c.found}
			allow, reason := gate(t, store, c.posture, nil).Allow(request())
			if allow != c.wantAllow || reason != c.wantReason {
				t.Fatalf("allow=%v reason=%q, want allow=%v reason=%q", allow, reason, c.wantAllow, c.wantReason)
			}
			// The gate asks about the REGISTRATION's project (the public
			// project it was built with), never the 90200 in the path.
			if c.found || store.err != nil {
				if store.lastProject != 1 || store.lastProvider != "wikis" {
					t.Fatalf("asked about project %d provider %q", store.lastProject, store.lastProvider)
				}
			}
		})
	}
}

func TestOneDecisionIsReusedForTheTTLAndThenReread(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	store := &plane{present: true, status: "inactive", found: true}
	g := gate(t, store, facade.AdmissionRecord, clock)

	for i := 0; i < 5; i++ {
		if allow, _ := g.Allow(request()); !allow {
			t.Fatalf("call %d refused an inactive provider while recording", i)
		}
	}
	if store.latestCalls != 1 {
		t.Fatalf("the plane was read %d times inside one TTL, want 1", store.latestCalls)
	}

	// Revoked now — and still allowed, because the cached answer has not
	// expired. This is the deliberate cost of the cache and it is asserted so
	// that shrinking the TTL is a decision somebody makes on purpose.
	store.status = "revoked"
	if allow, _ := g.Allow(request()); !allow {
		t.Fatal("the cached decision was not reused")
	}

	// One tick past the TTL: reread, and the revocation takes.
	now = now.Add(admission.DefaultTTL + time.Millisecond)
	allow, reason := g.Allow(request())
	if allow || reason != admission.ReasonRevoked {
		t.Fatalf("after the TTL: allow=%v reason=%q", allow, reason)
	}
	if store.latestCalls != 2 {
		t.Fatalf("the plane was read %d times, want 2", store.latestCalls)
	}
}

func TestAnUnreadablePlaneAllowsAndIsLoggedOncePerTTL(t *testing.T) {
	now := time.Now()
	store := &plane{present: true, err: errors.New("connection refused")}
	g := gate(t, store, facade.AdmissionEnforce, func() time.Time { return now })

	// Enforcing, and it still allows: a control that fails closed on its own
	// storage turns one database blip into a total outage of every provider.
	for i := 0; i < 4; i++ {
		if allow, reason := g.Allow(request()); !allow || reason != "" {
			t.Fatalf("call %d: allow=%v reason=%q", i, allow, reason)
		}
	}
	// The degraded decision is cached like any other, which is what makes the
	// warning fire once per TTL rather than once per invoke.
	if store.latestCalls != 1 {
		t.Fatalf("the failing query ran %d times inside one TTL, want 1", store.latestCalls)
	}
	now = now.Add(admission.DefaultTTL + time.Millisecond)
	if allow, _ := g.Allow(request()); !allow {
		t.Fatal("a still-unreadable plane refused")
	}
	if store.latestCalls != 2 {
		t.Fatalf("the failing query ran %d times over two TTLs, want 2", store.latestCalls)
	}
}

func TestAnAbsentPlaneAllowsWithoutEverBeingQueried(t *testing.T) {
	// Every shape of "there is nothing to read", including the one the E2E
	// stack is in (no public project). Each must allow, and none may reach a
	// revision query.
	for name, cfg := range map[string]admission.Config{
		"no store": {ProjectID: 1, ProviderID: func() string { return "wikis" }},
		"no public project": {
			Store: &plane{present: true, status: "revoked", found: true}, ProjectID: 0,
			ProviderID: func() string { return "wikis" },
		},
		"nothing registered yet": {
			Store: &plane{present: true, status: "revoked", found: true}, ProjectID: 1,
			ProviderID: func() string { return "" },
		},
		"no provider id resolver": {
			Store: &plane{present: true, status: "revoked", found: true}, ProjectID: 1,
		},
		"0107 not migrated": {
			Store: &plane{present: false, status: "revoked", found: true}, ProjectID: 1,
			ProviderID: func() string { return "wikis" },
		},
	} {
		cfg.Posture = facade.AdmissionEnforce
		allow, reason := admission.New(cfg).Allow(request())
		if !allow || reason != "" {
			t.Errorf("%s: allow=%v reason=%q", name, allow, reason)
		}
		if store, ok := cfg.Store.(*plane); ok && store.latestCalls != 0 {
			t.Errorf("%s: the plane was queried %d times", name, store.latestCalls)
		}
	}

	// A nil gate, and a nil request, answer the same way rather than panicking.
	var none *admission.Gate
	if allow, _ := none.Allow(request()); !allow {
		t.Fatal("a nil gate refused")
	}
	if none.Hook() != nil {
		t.Fatal("a nil gate produced a hook; a composition that built no gate must forward unchanged")
	}
	if allow, _ := gate(t, &plane{present: true, status: "revoked", found: true}, facade.AdmissionRecord, nil).Allow(nil); !allow {
		t.Fatal("a nil request refused")
	}
}

func TestTheHookIsTheGate(t *testing.T) {
	store := &plane{present: true, status: "revoked", found: true}
	hook := gate(t, store, facade.AdmissionRecord, nil).Hook()
	if hook == nil {
		t.Fatal("a built gate produced no hook")
	}
	if allow, reason := hook(request()); allow || reason != admission.ReasonRevoked {
		t.Fatalf("the hook answered allow=%v reason=%q", allow, reason)
	}
}
