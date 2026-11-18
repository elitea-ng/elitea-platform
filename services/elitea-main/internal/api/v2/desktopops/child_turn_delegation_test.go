package desktopops

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationedge"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/failurelimit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The nested agent a desktop runs as a cloud child crosses the edge from the
// turn's agent to the child in Main (issue 2214): the edge's grant narrows or
// refuses it before it is admitted, and the desktop gets the notice.

type delegationSettings struct {
	platformconfig.DelegationSettings
}

func (s delegationSettings) Get(context.Context) (platformconfig.DelegationSettings, error) {
	return s.DelegationSettings, nil
}

type delegationCallees map[int64]dg.Callee

func (c delegationCallees) Snapshot(_ context.Context, _, applicationID, _ int64) (dg.Callee, error) {
	return c[applicationID], nil
}

type delegationGrants []dg.Grant

func (g delegationGrants) ConsumableFromCallers(context.Context, int64, []int64) ([]dg.Grant, error) {
	return g, nil
}

// narrowingStarts runs the request's NarrowVersion as the start use case does,
// and records the version it would have admitted.
type narrowingStarts struct {
	fakeChildStarts
	version  json.RawMessage
	admitted json.RawMessage
}

func (f *narrowingStarts) StartCurrentApplication(ctx context.Context, request agentexecutionapp.CurrentApplicationStartRequest) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	version := f.version
	if request.NarrowVersion != nil {
		var err error
		if version, err = request.NarrowVersion(ctx, version); err != nil {
			return agentexecutionapp.CurrentApplicationStartOutcome{}, err
		}
	}
	f.admitted = version
	return f.fakeChildStarts.StartCurrentApplication(ctx, request)
}

func edgeHandler(t *testing.T, evaluator *delegationedge.Evaluator) (*childTurnHandler, *narrowingStarts, *fakeChildStore) {
	t.Helper()
	starts := &narrowingStarts{version: json.RawMessage(
		`{"tools":[{"id":2,"type":"jira","toolkit_name":"Jira","settings":{"selected_tools":["read","delete"]}}]}`)}
	store := &fakeChildStore{}
	h := newChildTurnHandler(ChildTurnDependencies{
		Start: starts, Authorizer: &fakeChildAuthorizer{}, Turns: &fakeTurns{turn: localturn.LiveTurn{ApplicationID: 11, VersionID: 12}},
		Store: store, Audit: &recordedAudit{}, Limiter: failurelimit.New(100, 60),
		Delegation: evaluator,
	})
	return h, starts, store
}

func evaluatorFor(t *testing.T, grants delegationGrants) *delegationedge.Evaluator {
	t.Helper()
	evaluator, err := delegationedge.New(
		delegationSettings{platformconfig.DelegationSettings{Stored: true, Mode: platformconfig.DelegationEnforce}},
		delegationCallees{
			11: {Name: "Supervisor", Set: dg.GrantSet{Toolkits: []dg.ToolkitGrant{{Key: "toolkit:1", Tools: dg.Tools{Mode: dg.ToolsAllExcept}}}}},
			21: {Name: "Reader", Set: dg.GrantSet{Toolkits: []dg.ToolkitGrant{{Key: "toolkit:2", Tools: dg.Tools{Mode: dg.ToolsOnly, Names: []string{"delete", "read"}}}}}},
		},
		grants)
	if err != nil {
		t.Fatal(err)
	}
	return evaluator
}

func TestChildStartWithoutAGrantIsRefusedWithTheNoticeAndStartsNothing(t *testing.T) {
	h, starts, store := edgeHandler(t, evaluatorFor(t, nil))
	response := callChild(h, http.MethodPost, childStartURL, childBody("read PROJ-1", "k1"))
	if response.Code != http.StatusForbidden {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	body := decodeChild(t, response)
	if body["error"] != "delegation_refused" {
		t.Fatalf("body = %v", body)
	}
	notice, _ := body["delegation_notice"].(map[string]any)
	if notice["kind"] != "refused" || notice["reason"] != "no_grant" || notice["id"] != "k1" {
		t.Fatalf("notice = %v", notice)
	}
	edge, _ := notice["edge"].(map[string]any)
	if edge["callee_name"] != "Reader" || edge["caller_name"] != "Supervisor" || edge["kind"] != "agent_tool" {
		t.Fatalf("edge = %v", edge)
	}
	if len(starts.requests) != 0 || starts.admitted != nil {
		t.Fatal("a refused child was admitted")
	}
	if len(store.discarded) != 1 {
		t.Fatalf("the refused child's conversation was not discarded: %v", store.discarded)
	}
}

func TestChildStartWithAStaleGrantIsNarrowedAndAnswersTheNotice(t *testing.T) {
	grant := dg.Grant{ID: 5, Edge: dg.Edge{CallerEntityID: 11, CalleeEntityID: 21, Kind: dg.EdgeAgentTool}, State: dg.StateStale,
		GrantSet: dg.GrantSet{Toolkits: []dg.ToolkitGrant{{Key: "toolkit:2", Tools: dg.Tools{Mode: dg.ToolsOnly, Names: []string{"read"}}}}}}
	h, starts, _ := edgeHandler(t, evaluatorFor(t, delegationGrants{grant}))
	response := callChild(h, http.MethodPost, childStartURL, childBody("read PROJ-1", "k1"))
	if response.Code != http.StatusAccepted {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	var admitted struct {
		Tools []struct {
			Settings struct {
				Selected []string `json:"selected_tools"`
			} `json:"settings"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(starts.admitted, &admitted); err != nil {
		t.Fatal(err)
	}
	if len(admitted.Tools) != 1 || len(admitted.Tools[0].Settings.Selected) != 1 || admitted.Tools[0].Settings.Selected[0] != "read" {
		t.Fatalf("the child was admitted with %s", starts.admitted)
	}
	notice, _ := decodeChild(t, response)["delegation_notice"].(map[string]any)
	if notice["kind"] != "narrowed" || notice["reason"] != "stale" || notice["grant_id"] != float64(5) {
		t.Fatalf("notice = %v", notice)
	}
}

// With the rule off (no evaluator: the grant switch is off) a child start is
// exactly what it was: nothing is rewritten, no notice is answered.
func TestChildStartIsUnchangedWhileTheRuleIsOff(t *testing.T) {
	h, starts, _ := edgeHandler(t, nil)
	response := callChild(h, http.MethodPost, childStartURL, childBody("read PROJ-1", "k1"))
	if response.Code != http.StatusAccepted {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	if string(starts.admitted) != string(starts.version) {
		t.Fatalf("the child was rewritten: %s", starts.admitted)
	}
	if _, present := decodeChild(t, response)["delegation_notice"]; present {
		t.Fatal("a notice was answered with the rule off")
	}
}
