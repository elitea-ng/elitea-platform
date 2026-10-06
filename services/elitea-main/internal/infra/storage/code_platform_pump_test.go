package storage

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// These fixtures check the typed transaction/IO boundary, not PostgreSQL, mTLS,
// signatures, image execution, or toolkit dispatch acceptance.
type platformIntentFixture struct {
	t                          *testing.T
	intent                     RegisteredCodeIntent
	broker                     RegisteredCodeBrokerBinding
	inTx                       bool
	callbacks, denyAt, updates int
	selectors                  []string
}

func (f *platformIntentFixture) WithRegisteredCodePlatformIntent(ctx context.Context, claim ContentClaim, dispatch, fingerprint string, apply func(context.Context, CodeTransaction, RegisteredCodeIntent) error) error {
	f.callbacks++
	f.selectors = append(f.selectors, fingerprint)
	if f.denyAt > 0 && f.callbacks >= f.denyAt {
		return domain.ErrUnauthorized
	}
	if claim.ExecutionID != f.intent.Binding.ExecutionID || dispatch != f.intent.Binding.DispatchActivation || fingerprint != f.broker.PreparedFingerprint {
		return code.ErrRejected
	}
	if f.inTx {
		f.t.Fatal("nested original-intent transaction")
	}
	f.inTx = true
	defer func() { f.inTx = false }()
	current := f.intent
	if current.CompiledExecute != nil {
		selected := *current.CompiledExecute
		current.CompiledExecute = &selected
	}
	return apply(ctx, platformTransactionFixture{f}, current)
}

type platformTransactionFixture struct{ f *platformIntentFixture }

func (x platformTransactionFixture) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	if !x.f.inTx || !strings.HasPrefix(query, "UPDATE elitea_runtime.original_code_broker_bindings") {
		x.f.t.Fatal("unexpected transaction mutation")
	}
	x.f.updates++
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (x platformTransactionFixture) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	if !x.f.inTx || !strings.Contains(query, "original_code_broker_bindings") {
		x.f.t.Fatal("unexpected metadata query")
	}
	return platformRowFixture{x.f.broker}
}

type platformRowFixture struct{ broker RegisteredCodeBrokerBinding }

func (r platformRowFixture) Scan(values ...any) error {
	if len(values) != 4 {
		return domain.ErrConflict
	}
	*values[0].(*string) = r.broker.PreparedSHA256
	*values[1].(*string) = r.broker.PreparedFingerprint
	raw, _ := json.Marshal(r.broker.Broker)
	*values[2].(*[]byte) = raw
	*values[3].(*string) = r.broker.DependencyBundleSHA256
	return nil
}

type platformOwnerFixture struct {
	f                                                *platformIntentFixture
	reads, pending, published                        int
	frame                                            []byte
	epochDrift, compiledDrift, publishLost, notReady bool
	readError, pendingError                          error
	onRead, onPending                                func()
}

func (o *platformOwnerFixture) runtime(c code.PlatformGrantClaims) code.RetainedRuntime {
	if o.f.inTx {
		o.f.t.Fatal("Supervisor IO while original-intent transaction is open")
	}
	var selected *code.PlatformCompiledExecute
	if c.CompiledExecute != nil {
		copy := *c.CompiledExecute
		selected = &copy
	}
	return code.RetainedRuntime{Kind: "docker", RuntimeID: "original-runtime", OwnerEpoch: 7, BindingSHA256: c.BindingSHA256, PreparedSHA256: c.PreparedSHA256, PreparedFingerprint: c.PreparedFingerprint, CompiledExecute: selected, PolicySHA256: c.PolicySHA256, MaxCalls: c.MaxCalls, MaxTotalBytes: c.MaxTotalBytes, Lifecycle: "dispatched"}
}
func (o *platformOwnerFixture) ReadRetainedRuntime(_ context.Context, c code.PlatformGrantClaims, _ code.SignedGrant) (code.RetainedRuntime, error) {
	o.reads++
	if o.f.inTx {
		o.f.t.Fatal("owner read inside transaction")
	}
	if o.onRead != nil {
		o.onRead()
	}
	if o.notReady {
		return code.RetainedRuntime{}, ErrCodePlatformNotReady
	}
	if o.readError != nil {
		return code.RetainedRuntime{}, o.readError
	}
	r := o.runtime(c)
	if o.compiledDrift && r.CompiledExecute != nil {
		r.CompiledExecute.SelectedDescriptorSHA256 = strings.Repeat("f", 64)
	}
	return r, nil
}
func (o *platformOwnerFixture) ReadPendingPlatformCall(_ context.Context, c code.PlatformGrantClaims, _ code.SignedGrant) (code.RetainedRuntime, []byte, error) {
	o.pending++
	if o.f.inTx {
		o.f.t.Fatal("pending read inside transaction")
	}
	if o.onPending != nil {
		o.onPending()
	}
	if o.pendingError != nil {
		return code.RetainedRuntime{}, nil, o.pendingError
	}
	r := o.runtime(c)
	if o.epochDrift {
		r.OwnerEpoch++
	}
	return r, append([]byte(nil), o.frame...), nil
}
func (o *platformOwnerFixture) PublishCommittedPlatformReply(_ context.Context, c code.PlatformGrantClaims, _ code.SignedGrant, exact []byte) (code.RetainedRuntime, error) {
	o.published++
	r := o.runtime(c)
	if c.Sequence == nil || *c.Sequence != 1 || c.CommittedReplySHA256 == nil || *c.CommittedReplySHA256 != code.Digest(exact) {
		o.f.t.Fatal("reply identity lost")
	}
	if o.publishLost {
		return r, domain.ErrUnavailable
	}
	return r, nil
}

type platformSignerFixture struct {
	f      *platformIntentFixture
	claims []code.PlatformGrantClaims
}

func (s *platformSignerFixture) SignPlatform(c code.PlatformGrantClaims) (code.SignedGrant, error) {
	if s.f.inTx {
		s.f.t.Fatal("owner signing during metadata transaction")
	}
	if c.Validate() != nil {
		return code.SignedGrant{}, code.ErrRejected
	}
	s.claims = append(s.claims, c)
	return code.SignedGrant{}, nil
}

type platformCallFixture struct {
	f                  *platformIntentFixture
	called, registered int
	unknown            bool
	last               domain.Admission
}

func (c *platformCallFixture) RegisterCodePlatformJob(_ context.Context, _ CodeTransaction, a domain.Admission) error {
	if !c.f.inTx || a.Validate() != nil {
		c.f.t.Fatal("retained job registration outside admission transaction")
	}
	c.registered++
	c.last = a
	return nil
}
func (c *platformCallFixture) ForCodePlatformCall(_ ContentClaim, a domain.Admission) (CodePlatformCallService, error) {
	if c.f.inTx || c.registered == 0 || a.Job != c.last.Job {
		c.f.t.Fatal("native call before original runtime registration")
	}
	return c, nil
}
func (c *platformCallFixture) Call(_ context.Context, a domain.Admission, frame []byte) ([]byte, error) {
	if c.f.inTx || a.Job != c.last.Job || len(frame) == 0 {
		c.f.t.Fatal("native IO boundary violated")
	}
	c.called++
	if c.unknown {
		return nil, domain.ErrUnknown
	}
	return []byte("fixture-signed-committed-reply"), nil
}
func platformPumpFixture(t *testing.T, compiled bool) (*CodePlatformPump, *platformIntentFixture, *platformOwnerFixture, *platformCallFixture, *platformSignerFixture, ContentClaim) {
	t.Helper()
	intent, broker, selected := brokerCompiledFixture()
	intent.CompiledExecute = selected
	if !compiled {
		intent.Compiled = false
		intent.CompiledExecute = nil
		intent.Binding.RequestDigest = broker.PreparedFingerprint
	}
	intent.BindingSHA256 = strings.Repeat("d", 64)
	o := &intent.Original
	o.ActorID = 11
	o.ExecutionID = intent.Binding.ExecutionID
	o.OriginalGeneration = 1
	o.Reference = code.OriginalVisitRef{VisitID: strings.Repeat("a", 64), Revision: 1, DigestSHA256: strings.Repeat("c", 64)}
	o.Declaration.PlatformClient = true
	claim := ContentClaim{ExecutionID: o.ExecutionID, Generation: 1, ClaimID: "fedcba9876543210fedcba9876543210", FenceToken: []byte{1, 2, 3}, PeerCertificate: &x509.Certificate{}}
	fence := sha256.Sum256(claim.FenceToken)
	intent.Access = CodeIntentAccess{ClaimID: claim.ClaimID, ClaimAttempt: 2, LeaseEpoch: 3, FenceSHA256: hex.EncodeToString(fence[:]), WorkloadIdentity: "spiffe://elitea/worker/one", IssuedAtUnixMillis: 1000, ExpiresAtUnixMillis: 2000}
	f := &platformIntentFixture{t: t, intent: intent, broker: broker}
	owner := &platformOwnerFixture{f: f}
	calls := &platformCallFixture{f: f}
	signer := &platformSignerFixture{f: f}
	pump, err := NewCodePlatformPump(f, owner, signer, calls, calls, "spiffe://elitea/main/one")
	if err != nil {
		t.Fatal(err)
	}
	return pump, f, owner, calls, signer, claim
}
func platformPumpFrame() []byte {
	header := []byte(`{"revision":1,"sequence":1,"operation":"user_get","resource":{"kind":"current_user"},"arguments":{}}`)
	frame := make([]byte, 8+len(header))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(header)))
	copy(frame[8:], header)
	return frame
}
func TestCodePlatformPumpCompiledIdleUsesRegisteredWholeRequestAndValueSelectors(t *testing.T) {
	pump, f, owner, calls, signer, claim := platformPumpFixture(t, true)
	result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
	if err != nil || result.State != "idle" {
		t.Fatalf("%#v %v", result, err)
	}
	if owner.reads != 1 || owner.pending != 1 || calls.called != 0 || calls.registered != 2 {
		t.Fatal("idle step submitted an operation or omitted runtime registration")
	}
	for _, c := range signer.claims {
		if c.RequestDigest == c.PreparedFingerprint || c.RequestDigest != f.intent.Binding.RequestDigest || !code.SamePlatformCompiledExecute(c.CompiledExecute, codePlatformClaims(f.intent, f.broker, "spiffe://elitea/main/one", "read_retained_runtime", nil, nil, nil).CompiledExecute) {
			t.Fatal("compiled authority collapsed or selector changed")
		}
	}
	for _, selector := range f.selectors {
		if selector != f.broker.PreparedFingerprint {
			t.Fatal("route selector was changed to whole request digest")
		}
	}
}
func TestCodePlatformPumpPlainCommittedPreservesOriginalRuntime(t *testing.T) {
	pump, f, owner, calls, _, claim := platformPumpFixture(t, false)
	owner.frame = platformPumpFrame()
	result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
	if err != nil || result.State != "committed" || !code.NonzeroDigest(result.EffectID) || calls.called != 1 || owner.published != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	if calls.last.Job.RuntimeID != "original-runtime" {
		t.Fatal("selected another runtime")
	}
}
func TestCodePlatformPumpUnknownObservesSameJournalWithoutPublish(t *testing.T) {
	pump, f, owner, calls, _, claim := platformPumpFixture(t, true)
	owner.frame = platformPumpFrame()
	calls.unknown = true
	var effect string
	for range 2 {
		result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
		if err != nil || result.State != "unknown_effect" || !code.NonzeroDigest(result.EffectID) {
			t.Fatal(result, err)
		}
		if effect != "" && effect != result.EffectID {
			t.Fatal("unknown effect identity drift")
		}
		effect = result.EffectID
	}
	if owner.published != 0 || calls.called != 2 || owner.reads != 2 {
		t.Fatal("unknown call was published or skipped original journal observation")
	}
}
func TestCodePlatformPumpLostCurrentClaimStopsBeforePendingOwnerIO(t *testing.T) {
	pump, f, owner, calls, _, claim := platformPumpFixture(t, true)
	f.denyAt = 2
	owner.frame = platformPumpFrame()
	if _, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal(err)
	}
	if owner.pending != 0 || owner.published != 0 || calls.called != 0 {
		t.Fatal("lost authority continued IO")
	}
}
func TestCodePlatformPumpOwnerEpochDriftRefusesCall(t *testing.T) {
	pump, f, owner, calls, _, claim := platformPumpFixture(t, true)
	owner.frame = platformPumpFrame()
	owner.epochDrift = true
	if _, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint); err == nil {
		t.Fatal("owner epoch drift accepted")
	}
	if calls.called != 0 || owner.published != 0 {
		t.Fatal("changed owner dispatched a call")
	}
}
func TestCodePlatformPumpCompiledDescriptorDriftRefusesOwnerReply(t *testing.T) {
	pump, f, owner, calls, _, claim := platformPumpFixture(t, true)
	owner.compiledDrift = true
	if _, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint); err == nil {
		t.Fatal("foreign original selector accepted")
	}
	if owner.pending != 0 || calls.called != 0 {
		t.Fatal("substituted compiled runtime continued")
	}
}
func TestCodePlatformPumpReplyDeliveryLossKeepsOwningEffect(t *testing.T) {
	pump, f, owner, calls, _, claim := platformPumpFixture(t, true)
	owner.frame = platformPumpFrame()
	owner.publishLost = true
	result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
	if err != nil || result.State != "reply_delivery_unknown" || !code.NonzeroDigest(result.EffectID) || calls.called != 1 || owner.published != 1 {
		t.Fatal(result, err)
	}
}

func TestCodePlatformPumpNotReadyRechecksParentAndHasNoEffects(t *testing.T) {
	for _, compiled := range []bool{false, true} {
		pump, f, owner, calls, signer, claim := platformPumpFixture(t, compiled)
		owner.notReady = true
		result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
		if err != nil || result.State != "idle" || result.EffectID != "" || f.callbacks != 2 || owner.reads != 1 || owner.pending != 0 || owner.published != 0 || calls.called != 0 || calls.registered != 0 || len(signer.claims) != 1 {
			t.Fatal("not-ready crossed admission or effect boundary", result, err)
		}
		if signer.claims[0].RequestDigest != f.intent.Binding.RequestDigest || signer.claims[0].PreparedFingerprint != f.broker.PreparedFingerprint {
			t.Fatal("original selector changed")
		}
	}
	pump, f, owner, calls, _, claim := platformPumpFixture(t, false)
	owner.notReady = true
	f.denyAt = 2
	if _, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint); !errors.Is(err, domain.ErrUnauthorized) || calls.called != 0 || calls.registered != 0 {
		t.Fatal("revoked parent became idle", err)
	}
}
func TestCodePlatformPumpOwnerRefusalsNeverBecomeIdle(t *testing.T) {
	for _, failure := range []error{code.ErrRejected, domain.ErrUnauthorized, domain.ErrUnavailable} {
		pump, f, owner, calls, _, claim := platformPumpFixture(t, false)
		owner.readError = failure
		if result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint); err == nil || result.State == "idle" || owner.pending != 0 || calls.called != 0 || calls.registered != 0 {
			t.Fatal("refusal became a no-effect readiness result", result, err)
		}
	}
}

func TestCodePlatformPumpCompletedRechecksOriginalAndHasNoEffects(t *testing.T) {
	for _, compiled := range []bool{false, true} {
		for _, pending := range []bool{false, true} {
			name := "retained/plain"
			if pending {
				name = "pending/plain"
			}
			if compiled {
				name += "/compiled"
			}
			t.Run(name, func(t *testing.T) {
				pump, f, owner, calls, signer, claim := platformPumpFixture(t, compiled)
				owner.frame = platformPumpFrame()
				callbacks, registrations, readsPending, grants := 2, 0, 0, 1
				if pending {
					owner.pendingError = ErrCodePlatformCompleted
					callbacks, registrations, readsPending, grants = 3, 1, 1, 2
				} else {
					owner.readError = ErrCodePlatformCompleted
				}
				result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
				if err != nil || result.State != "idle" || result.EffectID != "" || f.callbacks != callbacks || f.updates != registrations || calls.registered != registrations || calls.called != 0 || owner.reads != 1 || owner.pending != readsPending || owner.published != 0 || len(signer.claims) != grants {
					t.Fatal("completion crossed original admission or effect boundary", result, err, f.callbacks, f.updates, calls.registered)
				}
				for _, c := range signer.claims {
					if c.RequestDigest != f.intent.Binding.RequestDigest || c.PreparedFingerprint != f.broker.PreparedFingerprint || c.Operation == "publish_committed_platform_reply" {
						t.Fatal("completion changed original grant", c)
					}
				}
			})
		}
	}
}
func TestCodePlatformPumpCompletedRefusesPostIOClaimLossAndSnapshotDrift(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, failure := range []string{"claim loss", "binding drift", "parent drift", "compiled drift", "broker drift"} {
			name := "retained/" + failure
			if pending {
				name = "pending/" + failure
			}
			t.Run(name, func(t *testing.T) {
				pump, f, owner, calls, _, claim := platformPumpFixture(t, true)
				callbacks, registrations := 2, 0
				var mutate func()
				expected := code.ErrRejected
				switch failure {
				case "claim loss":
					expected = domain.ErrUnauthorized
					f.denyAt = 2
					if pending {
						f.denyAt = 3
					}
				case "binding drift":
					mutate = func() { f.intent.BindingSHA256 = strings.Repeat("e", 64) }
				case "parent drift":
					mutate = func() { f.intent.Original.ActorID++ }
				case "compiled drift":
					mutate = func() { f.intent.CompiledExecute.DescriptorSHA256 = strings.Repeat("f", 64) }
				case "broker drift":
					mutate = func() { f.broker.Broker.MaxCalls++ }
				}
				if pending {
					owner.pendingError = ErrCodePlatformCompleted
					owner.onPending = mutate
					callbacks, registrations = 3, 1
				} else {
					owner.readError = ErrCodePlatformCompleted
					owner.onRead = mutate
				}
				result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
				if !errors.Is(err, expected) || result.State != "" || result.EffectID != "" || f.callbacks != callbacks || calls.called != 0 || owner.published != 0 || calls.registered != registrations || f.updates != registrations {
					t.Fatal("completion bypassed current original authority", result, err, f.callbacks, calls.registered)
				}
			})
		}
	}
}
func TestCodePlatformPumpPendingRefusalsNeverBecomeIdle(t *testing.T) {
	for _, failure := range []error{ErrCodePlatformNotReady, code.ErrRejected, domain.ErrUnauthorized, domain.ErrUnavailable} {
		pump, f, owner, calls, _, claim := platformPumpFixture(t, false)
		owner.pendingError = failure
		result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
		if !errors.Is(err, code.ErrRejected) || result.State != "" || calls.called != 0 || owner.published != 0 || calls.registered != 1 || f.updates != 1 || f.callbacks != 2 {
			t.Fatal("pending refusal became idle or acquired authority", result, err)
		}
	}
}

func TestCodePlatformPumpCompletingRechecksOriginalAndHasNoEffects(t *testing.T) {
	for _, compiled := range []bool{false, true} {
		for _, pending := range []bool{false, true} {
			name := "retained/plain"
			if pending {
				name = "pending/plain"
			}
			if compiled {
				name += "/compiled"
			}
			t.Run(name, func(t *testing.T) {
				pump, f, owner, calls, signer, claim := platformPumpFixture(t, compiled)
				owner.frame = platformPumpFrame()
				callbacks, registrations, readsPending, grants := 2, 0, 0, 1
				if pending {
					owner.pendingError = ErrCodePlatformCompleting
					callbacks, registrations, readsPending, grants = 3, 1, 1, 2
				} else {
					owner.readError = ErrCodePlatformCompleting
				}
				result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
				if err != nil || result.State != "idle" || result.EffectID != "" || f.callbacks != callbacks || f.updates != registrations || calls.registered != registrations || calls.called != 0 || owner.reads != 1 || owner.pending != readsPending || owner.published != 0 || len(signer.claims) != grants {
					t.Fatal("completion crossed original admission or effect boundary", result, err, f.callbacks, f.updates, calls.registered)
				}
				for _, c := range signer.claims {
					if c.RequestDigest != f.intent.Binding.RequestDigest || c.PreparedFingerprint != f.broker.PreparedFingerprint || c.Operation == "publish_committed_platform_reply" {
						t.Fatal("completion changed original grant", c)
					}
				}
			})
		}
	}
}
func TestCodePlatformPumpCompletingRefusesPostIOClaimLossAndSnapshotDrift(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, failure := range []string{"claim loss", "binding drift", "parent drift", "compiled drift", "broker drift"} {
			name := "retained/" + failure
			if pending {
				name = "pending/" + failure
			}
			t.Run(name, func(t *testing.T) {
				pump, f, owner, calls, _, claim := platformPumpFixture(t, true)
				callbacks, registrations := 2, 0
				var mutate func()
				expected := code.ErrRejected
				switch failure {
				case "claim loss":
					expected = domain.ErrUnauthorized
					f.denyAt = 2
					if pending {
						f.denyAt = 3
					}
				case "binding drift":
					mutate = func() { f.intent.BindingSHA256 = strings.Repeat("e", 64) }
				case "parent drift":
					mutate = func() { f.intent.Original.ActorID++ }
				case "compiled drift":
					mutate = func() { f.intent.CompiledExecute.DescriptorSHA256 = strings.Repeat("f", 64) }
				case "broker drift":
					mutate = func() { f.broker.Broker.MaxCalls++ }
				}
				if pending {
					owner.pendingError = ErrCodePlatformCompleting
					owner.onPending = mutate
					callbacks, registrations = 3, 1
				} else {
					owner.readError = ErrCodePlatformCompleting
					owner.onRead = mutate
				}
				result, err := pump.Step(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
				if !errors.Is(err, expected) || result.State != "" || result.EffectID != "" || f.callbacks != callbacks || calls.called != 0 || owner.published != 0 || calls.registered != registrations || f.updates != registrations {
					t.Fatal("completion bypassed current original authority", result, err, f.callbacks, calls.registered)
				}
			})
		}
	}
}
