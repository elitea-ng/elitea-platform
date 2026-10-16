package storage

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"crypto/tls"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/go-chi/chi/v5"
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
	route := "/runtime/content/executions/0123456789abcdef0123456789abcdef/generations/1/code-platform/effects"
	s := &ContentServer{}
	w := httptest.NewRecorder()
	s.PostCodePlatformEffects(w, httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled effects status %d", w.Code)
	}
	// The route is registered only with the pump, next to the step route.
	if router := s.Routes(); router == nil {
		t.Fatal("no router")
	} else {
		w = httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/executions/e/generations/1/code-platform/effects", strings.NewReader(`{}`)))
		if w.Code != http.StatusNotFound {
			t.Fatalf("unconfigured route status %d", w.Code)
		}
	}
	s.codePlatformPump = &CodePlatformPump{}
	w = httptest.NewRecorder()
	s.PostCodePlatformEffects(w, httptest.NewRequest(http.MethodPost, route, strings.NewReader(strings.Repeat("x", 513))))
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing claim reached body/pump: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.PostCodePlatformEffects(w, httptest.NewRequest(http.MethodGet, route, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET status %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.PostCodePlatformEffects(w, httptest.NewRequest(http.MethodPost, route+"?x=1", strings.NewReader(`{}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("query status %d", w.Code)
	}
}

func TestCodePlatformEffectsRouteRegisteredOnlyWithPump(t *testing.T) {
	s := &ContentServer{requests: make(chan struct{}, 1), codePlatformPump: &CodePlatformPump{}}
	w := httptest.NewRecorder()
	// Reaches the handler (403: no verified TLS peer), not chi's 404.
	s.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/executions/0123456789abcdef0123456789abcdef/generations/1/code-platform/effects", strings.NewReader(`{}`)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("configured route status %d", w.Code)
	}
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

// The HTTP surface: same claim parsing, status mapping, closed response.
func effectsHTTP(t *testing.T, pump *CodePlatformPump, claim ContentClaim, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := &ContentServer{requests: make(chan struct{}, 2), codePlatformPump: pump}
	router := chi.NewRouter()
	router.Post("/executions/{executionID}/generations/{generation}/code-platform/effects", s.PostCodePlatformEffects)
	r := httptest.NewRequest(http.MethodPost, "/executions/"+claim.ExecutionID+"/generations/1/code-platform/effects", strings.NewReader(body))
	leaf := &x509.Certificate{}
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}}
	r.Header.Set(claimIDHeader, claim.ClaimID)
	r.Header.Set(fenceHeader, base64.RawURLEncoding.EncodeToString(append(append([]byte(nil), claim.FenceToken...), make([]byte, 32-len(claim.FenceToken))...)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func TestCodePlatformEffectsHTTPDispositionsAndStatusMapping(t *testing.T) {
	body := func(f *platformIntentFixture) string {
		return `{"schema":"` + effectsRequestSchema + `","revision":1,"dispatch_activation":"` + f.intent.Binding.DispatchActivation + `","prepared_request_fingerprint":"` + f.broker.PreparedFingerprint + `"}`
	}
	// The fixture claim fence is 3 bytes; the route requires 32, so bind a full one.
	run := func(mutate func(*CodePlatformPump, *platformIntentFixture, *effectsReaderFixture)) (*httptest.ResponseRecorder, *effectsReaderFixture) {
		pump, f, reader, claim := effectsPumpFixture(t)
		claim.FenceToken = make([]byte, 32)
		if mutate != nil {
			mutate(pump, f, reader)
		}
		return effectsHTTP(t, pump, claim, body(f)), reader
	}
	for name, tc := range map[string]struct {
		mutate func(*CodePlatformPump, *platformIntentFixture, *effectsReaderFixture)
		status int
		body   string
	}{
		"none":             {func(_ *CodePlatformPump, _ *platformIntentFixture, r *effectsReaderFixture) { r.possible = false }, 200, `{"schema":"elitea.runtime.code-platform-effects-response.v1","revision":1,"disposition":"none"}` + "\n"},
		"possible":         {func(_ *CodePlatformPump, _ *platformIntentFixture, r *effectsReaderFixture) { r.possible = true }, 200, `{"schema":"elitea.runtime.code-platform-effects-response.v1","revision":1,"disposition":"possible"}` + "\n"},
		"authority-denied": {func(_ *CodePlatformPump, f *platformIntentFixture, _ *effectsReaderFixture) { f.denyAt = 1 }, 403, ""},
		"binding-refused": {func(_ *CodePlatformPump, f *platformIntentFixture, _ *effectsReaderFixture) {
			f.broker.PreparedFingerprint = strings.Repeat("e", 64)
		}, 409, ""},
		"unavailable": {func(p *CodePlatformPump, _ *platformIntentFixture, _ *effectsReaderFixture) { p.WithEffectJournal(nil) }, 503, ""},
		"reader-failure": {func(_ *CodePlatformPump, _ *platformIntentFixture, r *effectsReaderFixture) {
			r.err = errors.New("pq: raw driver text (token=abc)")
		}, 503, ""},
	} {
		t.Run(name, func(t *testing.T) {
			w, _ := run(tc.mutate)
			if w.Code != tc.status {
				t.Fatalf("status %d body %q", w.Code, w.Body.String())
			}
			if tc.status == 200 {
				if w.Body.String() != tc.body || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("body %q headers %v", w.Body.String(), w.Header())
				}
			} else if strings.Contains(w.Body.String(), "token=abc") || strings.Contains(w.Body.String(), "driver") {
				t.Fatalf("error text leaked: %q", w.Body.String())
			}
		})
	}
	t.Run("invalid-body", func(t *testing.T) {
		pump, _, _, claim := effectsPumpFixture(t)
		if w := effectsHTTP(t, pump, claim, `{"schema":"x"}`); w.Code != http.StatusBadRequest {
			t.Fatalf("status %d", w.Code)
		}
	})
}
