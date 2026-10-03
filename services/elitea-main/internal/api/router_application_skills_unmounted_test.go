package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	applicationskillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

// UI-DC-2: WHAT THE ATTACHED-SKILLS READ ANSWERS WHEN SKILLS ARE OFF.
//
// apps/elitea-web hides the agent editor's Skills section when this read
// answers an HTTP 404 (useAgentSkills `isUnavailable`). That only works if
// the FULL router — not just mountReviewedProductionRoutes — answers a typed
// JSON 404 for an authenticated caller when CurrentApplicationSkills is nil
// (ELITEA_APPLICATION_SKILLS_ENABLED off). It once did not: a prototype
// `/application_skills/{mode}/...` wildcard in the skills block answered 200
// with the project-wide skills list (#367/#395), and the panel showed every
// skill in the project as attached. The skills block IS mounted here
// (SkillsRepo is set), so a wildcard put back there fails this test.
const attachedSkillsPath = "/api/v2/elitea_core/application_skills/prompt_lib/7/31"

func TestAttachedSkillsReadIsATypedJSON404WhenTheRouteIsNotComposed(t *testing.T) {
	router := NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})

	recorder := serveResponse(t, router, http.MethodGet, attachedSkillsPath)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET %s status = %d, want 404: the web client only hides the "+
			"Skills section on a 404.\n  Body: %s", attachedSkillsPath, recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("404 body %q is not JSON: %v", recorder.Body.String(), err)
	}
	if _, listed := body["items"]; listed {
		t.Fatalf("the 404 carries a skills list: %s", recorder.Body.String())
	}
}

// The control: the same full router WITH the route composed does not answer
// 404 for the same request, so the assertion above measures the absent route
// and not a path the router can never reach.
func TestAttachedSkillsReadIsServedWhenTheRouteIsComposed(t *testing.T) {
	route, err := applicationskillsapi.NewCurrentApplicationSkillsRoute(
		&productionApplicationSkillsReader{},
		middleware.AuthConfig{
			PrincipalValidator:        productionProjectPrincipalValidator{},
			ForwardedIdentityVerifier: productionProjectPeerVerifier{},
		},
		productionApplicationSkillsPermissionResolver{},
	)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{
		SkillsRepo:               struct{ v2skills.Repository }{},
		AuthValidator:            testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:       testPrincipalValidator{},
		CurrentApplicationSkills: route,
	})

	if status := serveStatus(t, router, http.MethodGet, attachedSkillsPath); status == http.StatusNotFound {
		t.Fatalf("GET %s answered 404 with the route composed; the absent-route "+
			"assertion proves nothing", attachedSkillsPath)
	}
}
