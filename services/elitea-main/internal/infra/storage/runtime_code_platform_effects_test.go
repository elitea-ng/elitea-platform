package storage

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"google.golang.org/grpc/codes"
)

const effectsRequestSchema = "elitea.runtime.code-platform-effects-request.v1"

func TestCodePlatformEffectsStrictBodyPins(t *testing.T) {
	valid := `{"schema":"` + effectsRequestSchema + `","revision":1,"dispatch_activation":"` + strings.Repeat("1", 64) + `","prepared_request_fingerprint":"` + strings.Repeat("2", 64) + `"}`
	if _, err := ParseCodePlatformEffects([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"unknown":       strings.Replace(valid, `"revision":1`, `"revision":1,"actor_id":2`, 1),
		"duplicate":     strings.Replace(valid, `"revision":1`, `"revision":1,"revision":1`, 1),
		"missing":       strings.Replace(valid, `"revision":1,`, ``, 1),
		"zero":          strings.Replace(valid, strings.Repeat("1", 64), strings.Repeat("0", 64), 1),
		"zero-prepared": strings.Replace(valid, strings.Repeat("2", 64), strings.Repeat("0", 64), 1),
		"upper":         strings.Replace(valid, strings.Repeat("1", 64), strings.Repeat("A", 64), 1),
		"null":          strings.Replace(valid, `"revision":1`, `"revision":null`, 1),
		"revision":      strings.Replace(valid, `"revision":1`, `"revision":2`, 1),
		"step-schema":   strings.Replace(valid, "effects-request", "step-request", 1),
		"trailing":      valid + `{}`,
		"bound":         strings.Repeat(" ", 513),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCodePlatformEffects([]byte(raw)); err == nil {
				t.Fatal("invalid authority frame accepted")
			}
		})
	}
}

func TestCodePlatformEffectsDefaultDisabledAndClaimBeforeBody(t *testing.T) {
	scope := contentScope("0123456789abcdef0123456789abcdef", 1)
	s := &ContentServer{requests: make(chan struct{}, 1)}
	_, err := s.ReadCodePlatformEffects(context.Background(), &runtimev1.ReadCodePlatformEffectsRequestV1{Scope: scope, RequestJson: []byte(`{}`)})
	requireContentCode(t, err, codes.Unimplemented)
	s.codePlatformPump = &CodePlatformPump{}
	// No claim metadata: refused before the body or the pump is touched.
	_, err = s.ReadCodePlatformEffects(contentPeerContext(context.Background(), nil), &runtimev1.ReadCodePlatformEffectsRequestV1{Scope: scope, RequestJson: []byte(strings.Repeat("x", 513))})
	requireContentCode(t, err, codes.PermissionDenied)
	// No verified peer at all.
	_, err = s.ReadCodePlatformEffects(context.Background(), &runtimev1.ReadCodePlatformEffectsRequestV1{Scope: scope, RequestJson: []byte(`{}`)})
	requireContentCode(t, err, codes.PermissionDenied)
}

type effectsReaderFixture struct {
	f                *platformIntentFixture
	possible         bool
	err              error
	calls            int
	execution        string
	generation       uint64
	dispatch, fprint string
	sawTransaction   bool
}

func (r *effectsReaderFixture) ReadOriginalCodePlatformUnresolvedEffects(_ context.Context, tx CodeTransaction, execution string, generation uint64, dispatch, fingerprint string) (bool, error) {
	r.calls++
	r.sawTransaction = r.f.inTx && tx != nil
	r.execution, r.generation, r.dispatch, r.fprint = execution, generation, dispatch, fingerprint
	return r.possible, r.err
}

func effectsPumpFixture(t *testing.T) (*CodePlatformPump, *platformIntentFixture, *effectsReaderFixture, ContentClaim) {
	t.Helper()
	pump, f, _, _, _, claim := platformPumpFixture(t, false)
	reader := &effectsReaderFixture{f: f}
	pump.WithEffectJournal(reader)
	return pump, f, reader, claim
}

func TestCodePlatformEffectsMissingReaderIsAnErrorNeverNone(t *testing.T) {
	pump, f, _, _, _, claim := platformPumpFixture(t, false)
	possible, err := pump.Effects(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
	if !errors.Is(err, domain.ErrUnavailable) || possible || f.callbacks != 0 {
		t.Fatalf("missing reader answered possible=%v err=%v callbacks=%d", possible, err, f.callbacks)
	}
	if _, err = (*CodePlatformPump)(nil).Effects(t.Context(), claim, strings.Repeat("1", 64), strings.Repeat("2", 64)); err == nil {
		t.Fatal("nil pump answered")
	}
}

func TestCodePlatformEffectsAnswersInsideAuthorizationTransaction(t *testing.T) {
	for _, possible := range []bool{false, true} {
		pump, f, reader, claim := effectsPumpFixture(t)
		reader.possible = possible
		got, err := pump.Effects(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
		if err != nil || got != possible {
			t.Fatalf("possible=%v got=%v err=%v", possible, got, err)
		}
		if reader.calls != 1 || !reader.sawTransaction || f.callbacks != 1 || f.updates != 0 {
			t.Fatalf("reader calls=%d inTx=%v callbacks=%d writes=%d", reader.calls, reader.sawTransaction, f.callbacks, f.updates)
		}
		if reader.execution != f.intent.Original.ExecutionID || reader.generation != 1 || reader.dispatch != f.intent.Binding.DispatchActivation || reader.fprint != f.broker.PreparedFingerprint {
			t.Fatal("reader selector is not the registered job")
		}
	}
}

func TestCodePlatformEffectsAuthorizationFailsClosedBeforeReader(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(*platformIntentFixture, *ContentClaim)
		want    error
	}{
		"denied-by-intent-consumer": {func(f *platformIntentFixture, _ *ContentClaim) { f.denyAt = 1 }, domain.ErrUnauthorized},
		"foreign-execution":         {func(_ *platformIntentFixture, c *ContentClaim) { c.ExecutionID = "foreign" }, code.ErrRejected},
	} {
		t.Run(name, func(t *testing.T) {
			pump, f, reader, claim := effectsPumpFixture(t)
			reader.possible = true
			tc.prepare(f, &claim)
			possible, err := pump.Effects(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
			if !errors.Is(err, tc.want) || possible || reader.calls != 0 {
				t.Fatalf("possible=%v err=%v reads=%d", possible, err, reader.calls)
			}
		})
	}
	t.Run("unrelated-dispatch-activation", func(t *testing.T) {
		pump, f, reader, claim := effectsPumpFixture(t)
		possible, err := pump.Effects(t.Context(), claim, strings.Repeat("9", 64), f.broker.PreparedFingerprint)
		if !errors.Is(err, code.ErrRejected) || possible || reader.calls != 0 {
			t.Fatalf("possible=%v err=%v reads=%d", possible, err, reader.calls)
		}
	})
	t.Run("wrong-prepared-fingerprint", func(t *testing.T) {
		pump, f, reader, claim := effectsPumpFixture(t)
		possible, err := pump.Effects(t.Context(), claim, f.intent.Binding.DispatchActivation, strings.Repeat("9", 64))
		if !errors.Is(err, code.ErrRejected) || possible || reader.calls != 0 {
			t.Fatalf("possible=%v err=%v reads=%d", possible, err, reader.calls)
		}
	})
	t.Run("zero-or-missing-peer", func(t *testing.T) {
		pump, f, reader, claim := effectsPumpFixture(t)
		claim.PeerCertificate = nil
		if _, err := pump.Effects(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint); !errors.Is(err, domain.ErrUnauthorized) || reader.calls != 0 {
			t.Fatalf("err=%v reads=%d", err, reader.calls)
		}
		claim.PeerCertificate = &x509.Certificate{}
		if _, err := pump.Effects(t.Context(), claim, strings.Repeat("0", 64), f.broker.PreparedFingerprint); !errors.Is(err, domain.ErrUnauthorized) || reader.calls != 0 {
			t.Fatalf("zero dispatch err=%v reads=%d", err, reader.calls)
		}
	})
	t.Run("reader-error-is-not-none", func(t *testing.T) {
		pump, f, reader, claim := effectsPumpFixture(t)
		reader.err = domain.ErrUnavailable
		possible, err := pump.Effects(t.Context(), claim, f.intent.Binding.DispatchActivation, f.broker.PreparedFingerprint)
		if !errors.Is(err, domain.ErrUnavailable) || possible {
			t.Fatalf("possible=%v err=%v", possible, err)
		}
	})
}

// The RPC surface: same claim parsing, status mapping, closed response.
func effectsRPC(t *testing.T, pump *CodePlatformPump, claim ContentClaim, body string) (*runtimev1.ReadCodePlatformEffectsResponseV1, error) {
	t.Helper()
	s := &ContentServer{requests: make(chan struct{}, 2), codePlatformPump: pump}
	fence := append(append([]byte(nil), claim.FenceToken...), make([]byte, 32-len(claim.FenceToken))...)
	ctx := contentClaimContext(context.Background(), &x509.Certificate{}, claim.ClaimID, fence)
	return s.ReadCodePlatformEffects(ctx, &runtimev1.ReadCodePlatformEffectsRequestV1{Scope: contentScope(claim.ExecutionID, 1), RequestJson: []byte(body)})
}

func TestCodePlatformEffectsRPCDispositionsAndStatusMapping(t *testing.T) {
	body := func(f *platformIntentFixture) string {
		return `{"schema":"` + effectsRequestSchema + `","revision":1,"dispatch_activation":"` + f.intent.Binding.DispatchActivation + `","prepared_request_fingerprint":"` + f.broker.PreparedFingerprint + `"}`
	}
	// The fixture claim fence is 3 bytes; the RPC requires 32, so bind a full one.
	run := func(mutate func(*CodePlatformPump, *platformIntentFixture, *effectsReaderFixture)) (*runtimev1.ReadCodePlatformEffectsResponseV1, error) {
		pump, f, reader, claim := effectsPumpFixture(t)
		claim.FenceToken = make([]byte, 32)
		if mutate != nil {
			mutate(pump, f, reader)
		}
		return effectsRPC(t, pump, claim, body(f))
	}
	for name, tc := range map[string]struct {
		mutate func(*CodePlatformPump, *platformIntentFixture, *effectsReaderFixture)
		code   codes.Code
		body   string
	}{
		"none":             {func(_ *CodePlatformPump, _ *platformIntentFixture, r *effectsReaderFixture) { r.possible = false }, codes.OK, `{"schema":"elitea.runtime.code-platform-effects-response.v1","revision":1,"disposition":"none"}`},
		"possible":         {func(_ *CodePlatformPump, _ *platformIntentFixture, r *effectsReaderFixture) { r.possible = true }, codes.OK, `{"schema":"elitea.runtime.code-platform-effects-response.v1","revision":1,"disposition":"possible"}`},
		"authority-denied": {func(_ *CodePlatformPump, f *platformIntentFixture, _ *effectsReaderFixture) { f.denyAt = 1 }, codes.PermissionDenied, ""},
		"binding-refused": {func(_ *CodePlatformPump, f *platformIntentFixture, _ *effectsReaderFixture) {
			f.broker.PreparedFingerprint = strings.Repeat("e", 64)
		}, codes.Aborted, ""},
		"unavailable": {func(p *CodePlatformPump, _ *platformIntentFixture, _ *effectsReaderFixture) { p.WithEffectJournal(nil) }, codes.Unavailable, ""},
		"reader-failure": {func(_ *CodePlatformPump, _ *platformIntentFixture, r *effectsReaderFixture) {
			r.err = errors.New("pq: raw driver text (token=abc)")
		}, codes.Unavailable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			response, err := run(tc.mutate)
			if tc.code == codes.OK {
				if err != nil || string(response.GetResponseJson()) != tc.body {
					t.Fatalf("response %q err %v", response.GetResponseJson(), err)
				}
				return
			}
			requireContentCode(t, err, tc.code)
			if strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "driver") {
				t.Fatalf("error text leaked: %q", err.Error())
			}
		})
	}
	t.Run("invalid-body", func(t *testing.T) {
		pump, _, _, claim := effectsPumpFixture(t)
		_, err := effectsRPC(t, pump, claim, `{"schema":"x"}`)
		requireContentCode(t, err, codes.InvalidArgument)
	})
}
