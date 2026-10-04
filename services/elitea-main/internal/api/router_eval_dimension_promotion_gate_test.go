package api

// The dimension update route in the router NewRouter really builds. The route
// gate is dimension.update, but promoting an `agent_adhoc` dimension to the
// project library ADDS a library entry, so the handler also asks for
// dimension.create. The handler tests inject that check by hand; only this
// test sees whether the composition root wires it, and wires it against the
// route's project.
//
//	PUT /elitea_core/eval_dimension/prompt_lib/{projectID}/{dimensionID}

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	v2evaluation "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
)

// promotionRepo holds ONE agent dimension and decides the tier the way the
// real repository does. Every other method panics through the nil interface.
type promotionRepo struct {
	v2evaluation.Repository
	stored  v2evaluation.Dimension
	updates int
}

func (r *promotionRepo) Update(_ context.Context, _, id string, d v2evaluation.Dimension) (v2evaluation.Dimension, error) {
	r.updates++
	tier, err := v2evaluation.ResolveTierUpdate(r.stored.Tier, d.Tier, d.PromotionPermitted)
	if err != nil {
		return v2evaluation.Dimension{}, err
	}
	d.ID, d.Tier = id, tier
	if tier == v2evaluation.TierProject {
		d.ApplicationID = nil
	}
	r.stored = d
	return d, nil
}

func newPromotionRouter(repo *promotionRepo, granted ...string) chi.Router {
	return NewRouter(RouterConfig{
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		EvalDimensionsRepo:        repo,
		ProjectAccessQuerier:      &memberOfProject{project: "7"},
		ProjectPermissionResolver: fakePermissionResolver{granted: granted, forProject: "7"},
	})
}

const promotionBody = `{"name":"Helpfulness","description":"Does the answer help?","tier":"project",
"allowed_engines":["ai"],"scale_type":"continuous","scale_min":0,"scale_max":100,"polarity":"higher_better",
"default_weight":1,"default_target":null,"default_target_operator":"","code":"","return_contract":""}`

func TestDimensionPromotionRequiresCreateInTheRouter(t *testing.T) {
	cases := []struct {
		name    string
		granted []string
		want    int
	}{
		{"update and create", []string{v2evaluation.PermissionDimensionUpdate, v2evaluation.PermissionDimensionCreate}, http.StatusOK},
		{"update without create", []string{v2evaluation.PermissionDimensionUpdate}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := 11
			repo := &promotionRepo{stored: v2evaluation.Dimension{ID: "1", Tier: v2evaluation.TierAgentAdhoc, ApplicationID: &agent}}
			request := httptest.NewRequest(http.MethodPut,
				"/api/v2/elitea_core/eval_dimension/prompt_lib/7/1", strings.NewReader(promotionBody))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			newPromotionRouter(repo, tc.granted...).ServeHTTP(recorder, testAuthHeader(request))

			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.want, recorder.Body.String())
			}
			wantTier := v2evaluation.TierProject
			if tc.want != http.StatusOK {
				wantTier = v2evaluation.TierAgentAdhoc
			}
			if repo.stored.Tier != wantTier {
				t.Fatalf("stored tier = %q, want %q", repo.stored.Tier, wantTier)
			}
		})
	}
}
