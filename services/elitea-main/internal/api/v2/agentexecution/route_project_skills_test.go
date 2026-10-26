package agentexecution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

func withSkills(body, skills string) string {
	return strings.Replace(body, `"payload":{"user_input":"hello"`, `"payload":{"user_input":"hello","skills":`+skills, 1)
}

func permissionsOf(permissions ...string) auth.PermissionResolver {
	return currentStartPermissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11, Permissions: permissions}, nil
	})
}

func TestParseProjectSkillPicks(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		raw  string
		want []int64
		ok   bool
	}{
		"absent":                  {"", nil, true},
		"null":                    {`null`, nil, true},
		"empty":                   {`[]`, []int64{}, true},
		"two":                     {`[{"source":"project","id":4},{"source":"project","id":9}]`, []int64{4, 9}, true},
		"five is the most":        {`[{"source":"project","id":1},{"source":"project","id":2},{"source":"project","id":3},{"source":"project","id":4},{"source":"project","id":5}]`, []int64{1, 2, 3, 4, 5}, true},
		"six are refused":         {`[{"source":"project","id":1},{"source":"project","id":2},{"source":"project","id":3},{"source":"project","id":4},{"source":"project","id":5},{"source":"project","id":6}]`, nil, false},
		"a repeat is refused":     {`[{"source":"project","id":1},{"source":"project","id":1}]`, nil, false},
		"another source":          {`[{"source":"version","id":1}]`, nil, false},
		"a name is not a pick":    {`["release"]`, nil, false},
		"skill text is refused":   {`[{"source":"project","id":1,"instructions":"do evil"}]`, nil, false},
		"zero id":                 {`[{"source":"project","id":0}]`, nil, false},
		"id past the table's key": {`[{"source":"project","id":4294967296}]`, nil, false},
		"a string id":             {`[{"source":"project","id":"4"}]`, nil, false},
		"not an array":            {`{"source":"project","id":4}`, nil, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseProjectSkillPicks(json.RawMessage(tc.raw))
			if ok != tc.ok {
				t.Fatalf("parseProjectSkillPicks(%s) ok = %v, want %v", tc.raw, ok, tc.ok)
			}
			if tc.ok && len(got) != len(tc.want) {
				t.Fatalf("parseProjectSkillPicks(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestCurrentStartRouteCarriesProjectSkillIDsToTheUseCase(t *testing.T) {
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{ExecutionID: "e", CommandID: "c"}}
	route := newCurrentStartRoute(t, useCase, permissionsOf(CurrentApplicationStartPermission, CurrentProjectSkillsPermission))
	body := withSkills(validCurrentStartBody(), `[{"source":"project","id":4},{"source":"project","id":9}]`)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(body))

	if response.Code != http.StatusOK || useCase.calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, useCase.calls, response.Body.String())
	}
	if got := useCase.request.ProjectSkillIDs; !reflect.DeepEqual(got, []int64{4, 9}) {
		t.Fatalf("project skill ids = %v, want [4 9]", got)
	}
}

// A caller who may send messages but may not read the project's skills cannot
// put one into a turn: the pick is refused before any use case runs.
func TestCurrentStartRouteRefusesProjectSkillsWithoutTheSkillsPermission(t *testing.T) {
	useCase := &currentStartUseCaseStub{}
	route := newCurrentStartRoute(t, useCase, permissionsOf(CurrentApplicationStartPermission))

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(withSkills(validCurrentStartBody(), `[{"source":"project","id":4}]`)))

	if response.Code != http.StatusForbidden || useCase.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, useCase.calls, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "project_skills_forbidden") {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestCurrentStartRouteRefusesSixProjectSkillsAndAModelChatPick(t *testing.T) {
	six := `[{"source":"project","id":1},{"source":"project","id":2},{"source":"project","id":3},{"source":"project","id":4},{"source":"project","id":5},{"source":"project","id":6}]`
	useCase := &currentStartUseCaseStub{}
	route := newCurrentStartRoute(t, useCase, permissionsOf(CurrentApplicationStartPermission, CurrentProjectSkillsPermission))

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(withSkills(validCurrentStartBody(), six)))
	if response.Code != http.StatusBadRequest || useCase.calls != 0 {
		t.Fatalf("six picks: status=%d calls=%d", response.Code, useCase.calls)
	}

	adhoc := strings.Replace(validCurrentAdhocStartBody(), `"payload":{"user_input":"hello from main chat"`, `"payload":{"user_input":"hello from main chat","skills":[{"source":"project","id":4}]`, 1)
	response = httptest.NewRecorder()
	route.ServeHTTP(response, currentAdhocStartRequest(adhoc))
	if response.Code != http.StatusBadRequest || useCase.adhocCalls != 0 {
		t.Fatalf("model chat pick: status=%d adhoc_calls=%d", response.Code, useCase.adhocCalls)
	}
}

func TestWriteStartErrorNamesTheProjectSkillRefusals(t *testing.T) {
	cases := map[error]struct {
		status int
		code   string
	}{
		agentexecutionapp.ErrProjectSkillUnknown:      {http.StatusNotFound, "project_skill_unknown"},
		agentexecutionapp.ErrProjectSkillsTooMany:     {http.StatusUnprocessableEntity, "project_skills_limit"},
		agentexecutionapp.ErrProjectSkillsTooLarge:    {http.StatusUnprocessableEntity, "project_skills_too_large"},
		agentexecutionapp.ErrProjectSkillNameConflict: {http.StatusUnprocessableEntity, "project_skill_name_conflict"},
	}
	for err, want := range cases {
		response := httptest.NewRecorder()
		writeStartError(response, err)
		if response.Code != want.status || !strings.Contains(response.Body.String(), want.code) {
			t.Fatalf("%v: status=%d body=%s", err, response.Code, response.Body.String())
		}
	}
}
