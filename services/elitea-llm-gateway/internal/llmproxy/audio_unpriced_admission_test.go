package llmproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// audioOnlyPricedEstimator prices the ONE audio basis it is given from the
// catalog, and nothing else: tokens are a default-table guess, and every other
// audio basis is unpriced.
type audioOnlyPricedEstimator struct {
	fakeCostEstimator
	basis string
}

func (a *audioOnlyPricedEstimator) CostUnits(ctx context.Context, provider, model string, u cost.Units) cost.Cost {
	if u.Basis() == cost.BasisTokens {
		return a.fakeCostEstimator.CostUnits(ctx, provider, model, u)
	}
	if u.Basis() != a.basis {
		return cost.Cost{}
	}
	return cost.Cost{TotalNanoUSD: 1, Basis: a.basis, Source: cost.SourceCatalog}
}

func unpricedAdmissionHandler(router *trackingRouter, budgeted bool, calc CostEstimator) *Handler {
	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy, Budgeted: budgeted},
		updated:      make(chan struct{}, 8),
	}
	return NewHandler(router, nil, nil, WithBudgetGate(gate, calc))
}

func audioRouterWithAnswers() *trackingRouter {
	router := &trackingRouter{}
	router.speechResp = &schemas.BifrostSpeechResponse{Audio: []byte("audio")}
	router.transcriptionResp = &schemas.BifrostTranscriptionResponse{Text: "hello"}
	return router
}

// TestAudio_BudgetedProjectRefusesAnUnpricedModel is the fail-closed rule. A
// project with a budget asks for an audio model the catalog carries no rate
// for. Dispatching it would bill zero, and a zero never reaches the budget, so
// the request is refused before the provider is called.
func TestAudio_BudgetedProjectRefusesAnUnpricedModel(t *testing.T) {
	for _, path := range audioBudgetRoutes {
		t.Run(path, func(t *testing.T) {
			router := audioRouterWithAnswers()
			calc := &audioUnpricedEstimator{fakeCostEstimator{source: cost.SourceDefault}}
			h := unpricedAdmissionHandler(router, true, calc)
			before := audioRefusedUnpricedModel.Value()

			rec := httptest.NewRecorder()
			h.route().ServeHTTP(rec, audioBudgetRequest(t, path))

			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "audio_unpriced") {
				t.Fatalf("the refusal does not name the cause: %s", rec.Body.String())
			}
			if router.called.Load() {
				t.Fatalf("%s called the provider for a model it cannot bill", path)
			}
			if got := audioRefusedUnpricedModel.Value() - before; got != 1 {
				t.Fatalf("%s moved %s by %d, want 1", path, MetricAudioRefusedUnpricedModel, got)
			}
		})
	}
}

// TestAudio_UnbudgetedProjectKeepsBillZeroAndCount is the other half: a
// project with no budget has no ceiling to protect, so the unpriced model is
// still served, billed zero and counted, as before.
func TestAudio_UnbudgetedProjectKeepsBillZeroAndCount(t *testing.T) {
	for _, path := range audioBudgetRoutes {
		t.Run(path, func(t *testing.T) {
			router := audioRouterWithAnswers()
			calc := &audioUnpricedEstimator{fakeCostEstimator{source: cost.SourceDefault}}
			h := unpricedAdmissionHandler(router, false, calc)

			rec := httptest.NewRecorder()
			h.route().ServeHTTP(rec, audioBudgetRequest(t, path))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if !router.called.Load() {
				t.Fatalf("%s did not call the provider", path)
			}
		})
	}
}

// TestAudio_BudgetedProjectAdmitsAPricedModel proves the probe reads the
// route's own basis: characters or output seconds for speech, input seconds
// for transcription, or a catalog token rate for either.
func TestAudio_BudgetedProjectAdmitsAPricedModel(t *testing.T) {
	cases := []struct {
		name, path string
		calc       CostEstimator
	}{
		{"speech by characters", "/llm/v1/audio/speech", &audioOnlyPricedEstimator{fakeCostEstimator{source: cost.SourceDefault}, cost.BasisCharacters}},
		{"speech by seconds", "/llm/v1/audio/speech", &audioOnlyPricedEstimator{fakeCostEstimator{source: cost.SourceDefault}, cost.BasisSeconds}},
		{"transcription by seconds", "/llm/v1/audio/transcriptions", &audioOnlyPricedEstimator{fakeCostEstimator{source: cost.SourceDefault}, cost.BasisSeconds}},
		{"speech by catalog tokens", "/llm/v1/audio/speech", &audioUnpricedEstimator{fakeCostEstimator{source: cost.SourceCatalog}}},
		{"transcription by catalog tokens", "/llm/v1/audio/transcriptions", &audioUnpricedEstimator{fakeCostEstimator{source: cost.SourceCatalog}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := audioRouterWithAnswers()
			h := unpricedAdmissionHandler(router, true, tc.calc)

			rec := httptest.NewRecorder()
			h.route().ServeHTTP(rec, audioBudgetRequest(t, tc.path))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if !router.called.Load() {
				t.Fatalf("%s did not call the provider", tc.path)
			}
		})
	}

	// A characters rate does not price a transcription: that route never
	// reports characters, so admitting on it would still bill zero.
	t.Run("transcription priced only by characters is refused", func(t *testing.T) {
		router := audioRouterWithAnswers()
		h := unpricedAdmissionHandler(router, true,
			&audioOnlyPricedEstimator{fakeCostEstimator{source: cost.SourceDefault}, cost.BasisCharacters})
		rec := httptest.NewRecorder()
		h.route().ServeHTTP(rec, audioBudgetRequest(t, "/llm/v1/audio/transcriptions"))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
		}
	})
}
