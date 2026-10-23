// Package admission puts the admission plane IN FORCE on the request path.
//
// Migration 0107 and internal/providerhub gave this deployment a record of
// what it admits: the facades' registrar files an `inactive` revision at
// boot, the administration surface can revoke one, and the admin page shows
// both. NOTHING READ IT. A revoked provider kept serving every invoke, so
// the one operator action the surface offers — turn this provider off —
// changed a row and nothing else. This package is the read.
//
// WHAT IT IS NOT. It is not authentication and it is not a permission: the
// routes table asks it AFTER both, and it answers a question neither of
// those can — whether THIS DEPLOYMENT still admits the provider at all. A
// caller with every grant is refused by a revocation, which is the point of
// a revocation.
//
// THE DEGRADED PATH ALLOWS, DELIBERATELY. Absence of the plane (no
// database, no public project, migration 0107 not applied) and failure to
// read it both resolve to "allow", and the second one logs. This is the
// same decision the LLM gateway made for an unreadable model set
// (services/elitea-llm-gateway/DECISIONS.md): a control that fails closed on
// its own storage turns one database blip into a total outage of every
// provider, while the thing it is protecting against — a provider an
// operator has actively revoked — is a state someone had to create on
// purpose and which the plane will report correctly again the moment it is
// readable. Refusing on absence would additionally break every deployment
// that has never migrated, which is most of them.
package admission

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhub"
)

// The refusal codes. Stable strings: a client asserts on them and an E2E
// journey pins them.
const (
	// ReasonRevoked — an operator revoked this provider's admission.
	ReasonRevoked = "provider_admission_revoked"
	// ReasonInactive — the provider is recorded but not in force, and this
	// deployment is enforcing.
	ReasonInactive = "provider_admission_inactive"
)

// DefaultTTL is how long one decision is reused.
//
// SHORT ON PURPOSE. Revoking is an operator's emergency stop, and the
// interval between the click and the first refusal is the window the
// operator is trying to close. Fifteen seconds keeps the query off the hot
// path (an invoke is a proxied round trip, not a tight loop) without making
// a revocation feel like it did not take.
const DefaultTTL = 15 * time.Second

// Store is the admission plane as this gate reads it.
type Store interface {
	// Present reports whether the plane exists to be read at all.
	Present(ctx context.Context) bool
	// Latest is the newest revision for one (project, provider); false when
	// nobody has admitted one.
	Latest(ctx context.Context, projectID int64, providerID string) (providerhub.Admitted, bool, error)
}

// PoolStore reads the plane over a pgx pool. A nil pool is a deployment
// without a database, and reports the plane absent rather than panicking —
// the composition root can hand one over without a branch.
type PoolStore struct{ Pool *pgxpool.Pool }

// Present reports whether migration 0107 has been applied.
func (s PoolStore) Present(ctx context.Context) bool {
	if s.Pool == nil {
		return false
	}
	return providerhub.Present(ctx, s.Pool)
}

// Latest reads the newest admitted revision.
func (s PoolStore) Latest(ctx context.Context, projectID int64, providerID string) (providerhub.Admitted, bool, error) {
	if s.Pool == nil {
		return providerhub.Admitted{}, false, nil
	}
	return providerhub.LatestAdmission(ctx, s.Pool, projectID, providerID)
}

// Config builds one gate.
type Config struct {
	// Store is the plane. Nil means there is none: every request is allowed.
	Store Store
	// ProjectID is the project the REGISTRATION is filed under — the public
	// project, not the one in the request path.
	//
	// THAT IS NOT AN OVERSIGHT. A facade's registrar files its provider once,
	// under ELITEA_AI_PROJECT_ID, so that every project's catalogue can see
	// it; admission is therefore a fact about the DEPLOYMENT, and keying the
	// gate on the caller's project would look up a row that is never written
	// and allow everything. Zero or less means no public project, which is
	// the E2E stack, and allows.
	ProjectID int64
	// ProviderID is the provider's own name from its descriptor — read
	// lazily, because the facade does not know it until the registrar has
	// fetched the descriptor, which happens after composition. Empty (nothing
	// registered yet) allows.
	ProviderID func() string
	// Posture decides what an `inactive` provider means here.
	Posture facade.AdmissionPosture
	// TTL overrides DefaultTTL.
	TTL time.Duration
	// Clock overrides time.Now (tests).
	Clock  func() time.Time
	Logger *slog.Logger
}

type decision struct {
	allow  bool
	reason string
	at     time.Time
}

// Gate answers whether one provider may still be invoked.
type Gate struct {
	cfg Config

	mu    sync.Mutex
	cache map[string]decision
}

// New builds a gate. It is usable with everything unset: that gate allows.
func New(cfg Config) *Gate {
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Posture == "" {
		cfg.Posture = facade.AdmissionRecord
	}
	return &Gate{cfg: cfg, cache: make(map[string]decision)}
}

// Hook is the gate as the routes table takes it. A nil gate yields a nil
// hook, so a composition that built none forwards unchanged rather than
// installing a check that always says yes — the two are the same at runtime
// and very different to read.
func (g *Gate) Hook() facade.AdmissionHook {
	if g == nil {
		return nil
	}
	return g.Allow
}

// Allow answers for one request. See the package comment for why every
// unreadable state answers true.
func (g *Gate) Allow(r *http.Request) (bool, string) {
	if g == nil || r == nil {
		return true, ""
	}
	providerID := ""
	if g.cfg.ProviderID != nil {
		providerID = g.cfg.ProviderID()
	}
	if g.cfg.Store == nil || g.cfg.ProjectID <= 0 || providerID == "" {
		return true, ""
	}
	key := strconv.FormatInt(g.cfg.ProjectID, 10) + "/" + providerID
	now := g.cfg.Clock()

	g.mu.Lock()
	cached, ok := g.cache[key]
	g.mu.Unlock()
	if ok && now.Sub(cached.at) < g.cfg.TTL {
		return cached.allow, cached.reason
	}

	allow, reason := g.resolve(r.Context(), providerID)

	g.mu.Lock()
	g.cache[key] = decision{allow: allow, reason: reason, at: now}
	g.mu.Unlock()
	return allow, reason
}

// resolve reads the plane once. The result is cached by Allow, including the
// degraded one — which is what makes "log once per TTL" true without a
// second timestamp to keep in step with the first.
func (g *Gate) resolve(ctx context.Context, providerID string) (bool, string) {
	if !g.cfg.Store.Present(ctx) {
		return true, ""
	}
	latest, found, err := g.cfg.Store.Latest(ctx, g.cfg.ProjectID, providerID)
	if err != nil {
		g.cfg.Logger.Warn("provider admission is unreadable; the provider is allowed through",
			"provider", providerID, "project", g.cfg.ProjectID, "error", err)
		return true, ""
	}
	if !found {
		// Registered a moment ago and not yet admitted, or never registered
		// at all. Neither is a revocation.
		return true, ""
	}
	switch latest.Status {
	case "revoked":
		// ALWAYS, in either posture. `record` is about what this deployment
		// does with a provider nobody has decided on; a revocation is a
		// decision, and a posture that could ignore it would make the
		// administration surface's one action advisory.
		return false, ReasonRevoked
	case "active":
		return true, ""
	default:
		// `inactive`, and anything a later migration adds that this build
		// does not recognise: recorded, not decided.
		if g.cfg.Posture == facade.AdmissionEnforce {
			return false, ReasonInactive
		}
		return true, ""
	}
}
