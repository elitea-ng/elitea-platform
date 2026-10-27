package credentialbroker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/cache"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
)

const tExternal = "0192f0aa-0000-7000-8000-0000000000e1"

// fakeEpochs answers every pinned key at its own epoch (current) unless
// overridden; an Unpinned key at 0. err fails every read.
type fakeEpochs struct {
	mu        sync.Mutex
	overrides map[epoch.Kind]int64
	err       error
	reads     int
	lastKeys  []epoch.SecurityEpoch
}

func (f *fakeEpochs) Read(_ context.Context, keys []epoch.SecurityEpoch) ([]epoch.SecurityEpoch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.lastKeys = keys
	if f.err != nil {
		return nil, f.err
	}
	out := make([]epoch.SecurityEpoch, len(keys))
	for i, key := range keys {
		value := key.Value
		if value == epoch.Unpinned {
			value = 0
		}
		if v, ok := f.overrides[key.Kind]; ok {
			value = v
		}
		out[i] = epoch.SecurityEpoch{Kind: key.Kind, ID: key.ID, Value: value}
	}
	return out, nil
}

func (f *fakeEpochs) set(kind epoch.Kind, value int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.overrides == nil {
		f.overrides = map[epoch.Kind]int64{}
	}
	f.overrides[kind] = value
}

type fakeRevisions struct{ doc profile.PolicyDocument }

func (f fakeRevisions) Revision(_ context.Context, id string) (profile.PolicyRevision, error) {
	return profile.PolicyRevision{ID: id, Document: f.doc}, nil
}

func (fakeRevisions) PutRevision(context.Context, profile.PolicyRevision) error { return nil }

func oauthWorld() *world {
	w := newWorld()
	w.context.MandateID, w.context.MandateEpoch = "", 0
	w.context.DelegationID, w.context.DelegationEpoch = tDelegation, 1
	ref := w.references.refs[tReference]
	ref.Class = credentialgrant.ClassOAuthDelegated
	w.references.refs[tReference] = ref
	w.bindings.binding.ExternalAgentIdentityID = tExternal
	return w
}

type revocationRig struct {
	epochs *fakeEpochs
	cache  *cache.Cache
	hub    *epoch.Hub
	broker *Broker
}

func newRevocationRig(t *testing.T, w *world, options ...func(*Config)) *revocationRig {
	t.Helper()
	c, err := cache.New(cache.Limits{MaxEntries: 16, MaxBytes: 1 << 16, Skew: 30 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := epoch.NewHub(epoch.HubOptions{Now: func() time.Time { return tNow }})
	if err != nil {
		t.Fatal(err)
	}
	r := &revocationRig{epochs: &fakeEpochs{}, cache: c, hub: hub}
	r.broker = w.broker(t, append([]func(*Config){func(cfg *Config) {
		cfg.Epochs, cfg.Cache, cfg.Invalidations = r.epochs, c, hub
	}}, options...)...)
	return r
}

// nth is the world's request with its own invocation id.
func nth(t *testing.T, w *world, n int) RedeemRequest {
	t.Helper()
	req := w.request(t)
	req.Invocation.InvocationID = fmt.Sprintf("0192f0aa-0000-7000-8000-%012x", 0x100+n)
	return req
}

func permitted(t *testing.T, result Result) *Envelope {
	t.Helper()
	if result.Envelope == nil {
		t.Fatalf("denied: %s (%s)", result.Denial.Code, result.Denial.reason)
	}
	return result.Envelope
}

// The redemption reads every epoch it depends on in one read: the context's
// binding and delegation, the decision's grant and credential reference,
// and the binding's external identity.
func TestRedeemReadsEveryEpochItDependsOn(t *testing.T) {
	w := oauthWorld()
	r := newRevocationRig(t, w)
	permitted(t, redeem(t, r.broker, nth(t, w, 1))).Destroy()
	want := map[epoch.Kind]int64{
		epoch.KindBinding: 2, epoch.KindDelegation: 1, epoch.KindGrant: 4, epoch.KindCredentialRef: 1,
		epoch.KindExternalIdentity: epoch.Unpinned,
	}
	if r.epochs.reads != 1 || len(r.epochs.lastKeys) != len(want) {
		t.Fatalf("reads %d keys %+v", r.epochs.reads, r.epochs.lastKeys)
	}
	for _, key := range r.epochs.lastKeys {
		if v, ok := want[key.Kind]; !ok || v != key.Value {
			t.Fatalf("key %+v, want %v", key, want)
		}
	}
}

// An OAuth access token is exchanged once and reused while the epochs hold.
func TestOAuthAccessTokenIsCachedUnderItsEpochs(t *testing.T) {
	w := oauthWorld()
	r := newRevocationRig(t, w)
	for i := range 3 {
		envelope := permitted(t, redeem(t, r.broker, nth(t, w, i)))
		if string(envelope.Material["access_token"]) != tOAuthValue || envelope.NotAfter.After(tNow.Add(2*time.Minute)) {
			t.Fatalf("envelope %d: not_after %s", i, envelope.NotAfter)
		}
		envelope.Destroy()
	}
	if w.exchanger.calls != 1 || r.cache.Len() != 1 {
		t.Fatalf("exchanges %d, entries %d", w.exchanger.calls, r.cache.Len())
	}
}

// Acceptance: an epoch bump makes the next hit a miss, and a revoked
// authority denies the redemption before any cache lookup or exchange.
func TestEpochBumpMissesAndRevocationDenies(t *testing.T) {
	t.Run("external identity moved on: miss, re-exchange", func(t *testing.T) {
		w := oauthWorld()
		r := newRevocationRig(t, w)
		permitted(t, redeem(t, r.broker, nth(t, w, 1))).Destroy()
		r.epochs.set(epoch.KindExternalIdentity, 1)
		permitted(t, redeem(t, r.broker, nth(t, w, 2))).Destroy()
		if w.exchanger.calls != 2 {
			t.Fatalf("exchanges %d: the stale entry was served", w.exchanger.calls)
		}
	})
	for kind, value := range map[epoch.Kind]int64{
		epoch.KindDelegation: 2, epoch.KindBinding: epoch.Revoked, epoch.KindGrant: 5, epoch.KindCredentialRef: epoch.Revoked,
		epoch.KindExternalIdentity: epoch.Revoked,
	} {
		t.Run(string(kind)+" revoked: denied", func(t *testing.T) {
			w := oauthWorld()
			r := newRevocationRig(t, w)
			permitted(t, redeem(t, r.broker, nth(t, w, 1))).Destroy()
			r.epochs.set(kind, value)
			denial := requireDenial(t, redeem(t, r.broker, nth(t, w, 2)), CodeDenied)
			if denial.Reason() != "authority_revoked" || w.exchanger.calls != 1 || w.custody.loads != 1 {
				t.Fatalf("reason %s, exchanges %d, custody loads %d", denial.Reason(), w.exchanger.calls, w.custody.loads)
			}
			if r.cache.Len() != 0 {
				t.Fatal("the revoked partition stayed cached")
			}
		})
	}
}

// Outage: PostgreSQL unavailable denies new redemptions, even with a warm
// cache; after repeated failures the breaker opens and no read is tried.
func TestPostgresDownDeniesRedemption(t *testing.T) {
	w := oauthWorld()
	r := newRevocationRig(t, w)
	permitted(t, redeem(t, r.broker, nth(t, w, 0))).Destroy()
	r.epochs.err = errors.New("connection refused")
	for i := 1; i <= breakerTrips; i++ {
		denial := requireDenial(t, redeem(t, r.broker, nth(t, w, i)), CodeBackendUnavailable)
		if denial.Reason() != "epoch_unavailable" {
			t.Fatalf("attempt %d reason %s", i, denial.Reason())
		}
	}
	reads := r.epochs.reads
	denial := requireDenial(t, redeem(t, r.broker, nth(t, w, 99)), CodeBackendUnavailable)
	if denial.Reason() != "epoch_circuit_open" || r.epochs.reads != reads || w.exchanger.calls != 1 {
		t.Fatalf("reason %s, reads %d→%d, exchanges %d", denial.Reason(), reads, r.epochs.reads, w.exchanger.calls)
	}
}

// Outage: the identity provider unavailable trips its own breaker.
func TestIdentityProviderBreakerOpens(t *testing.T) {
	w := oauthWorld()
	w.exchanger.err = fmt.Errorf("%w: 503", obo.ErrUnavailable)
	r := newRevocationRig(t, w)
	for i := range breakerTrips {
		if denial := requireDenial(t, redeem(t, r.broker, nth(t, w, i)), CodeBackendUnavailable); denial.Reason() != "oauth_unavailable" {
			t.Fatalf("attempt %d reason %s", i, denial.Reason())
		}
	}
	denial := requireDenial(t, redeem(t, r.broker, nth(t, w, 50)), CodeBackendUnavailable)
	if denial.Reason() != "oauth_circuit_open" || w.exchanger.calls != breakerTrips {
		t.Fatalf("reason %s, exchanges %d", denial.Reason(), w.exchanger.calls)
	}
}

// An interaction requirement is the IdP answering: it never trips.
func TestInteractionRequiredDoesNotTripTheBreaker(t *testing.T) {
	w := oauthWorld()
	w.exchanger.err = obo.ErrInteractionRequired
	r := newRevocationRig(t, w)
	for i := range breakerTrips + 2 {
		requireDenial(t, redeem(t, r.broker, nth(t, w, i)), CodeAuthorizationRequired)
	}
}

// Static secrets are read every time unless the run's revision allows
// caching them.
func TestStaticSecretsAreCachedOnlyWhenTheRevisionAllows(t *testing.T) {
	w := newWorld().withMandate("autonomous")
	r := newRevocationRig(t, w)
	for i := range 2 {
		permitted(t, redeem(t, r.broker, nth(t, w, i))).Destroy()
	}
	if w.secrets.readCount() != 2 || r.cache.Len() != 0 {
		t.Fatalf("default: reads %d entries %d", w.secrets.readCount(), r.cache.Len())
	}

	w = newWorld().withMandate("autonomous")
	r = newRevocationRig(t, w, func(cfg *Config) {
		cfg.Revisions = fakeRevisions{doc: profile.PolicyDocument{Cache: &profile.CachePolicy{CacheStaticSecrets: true}}}
	})
	for i := range 2 {
		envelope := permitted(t, redeem(t, r.broker, nth(t, w, i)))
		if string(envelope.Material["api_token"]) != tSecretValue {
			t.Fatalf("material %q", envelope.Material["api_token"])
		}
		envelope.Destroy()
	}
	if w.secrets.readCount() != 1 || r.cache.Len() != 1 {
		t.Fatalf("allowed: reads %d entries %d", w.secrets.readCount(), r.cache.Len())
	}
}

// A replica's invalidation drops the partition at once; the latency from
// the change to the first denial is observed.
func TestInvalidationDropsThePartitionAndTheDenialIsTimed(t *testing.T) {
	w := oauthWorld()
	r := newRevocationRig(t, w)
	permitted(t, redeem(t, r.broker, nth(t, w, 1))).Destroy()
	r.hub.Notify(context.Background(), epoch.Change{
		Epoch:     epoch.SecurityEpoch{Kind: epoch.KindDelegation, ID: tDelegation, Value: 2},
		ChangedAt: tNow.Add(-time.Second),
	})
	if r.cache.Len() != 0 {
		t.Fatal("the invalidation did not drop the partition")
	}
	r.epochs.set(epoch.KindDelegation, epoch.Revoked)
	requireDenial(t, redeem(t, r.broker, nth(t, w, 2)), CodeDenied)
}

// Without an epoch reader nothing changes: no read, no cache (the P3 path).
func TestNoEpochReaderKeepsTheRedemptionPath(t *testing.T) {
	w := oauthWorld()
	b := w.broker(t)
	for i := range 2 {
		permitted(t, redeem(t, b, nth(t, w, i))).Destroy()
	}
	if w.exchanger.calls != 2 || b.Cache() != nil {
		t.Fatalf("exchanges %d", w.exchanger.calls)
	}
}
