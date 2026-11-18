package delegationedge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationnotice"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type fakeSettings struct {
	settings platformconfig.DelegationSettings
}

func (f fakeSettings) Get(context.Context) (platformconfig.DelegationSettings, error) {
	return f.settings, nil
}

type fakeCallees map[int64]dg.Callee

func (f fakeCallees) Snapshot(_ context.Context, _, applicationID, _ int64) (dg.Callee, error) {
	callee, ok := f[applicationID]
	if !ok {
		return dg.Callee{}, dg.ErrCalleeNotFound
	}
	return callee, nil
}

type fakeGrants []dg.Grant

func (f fakeGrants) ConsumableFromCallers(context.Context, int64, []int64) ([]dg.Grant, error) {
	return f, nil
}

func onlyTools(key string, tools ...string) dg.ToolkitGrant {
	return dg.ToolkitGrant{Key: key, Tools: dg.Tools{Mode: dg.ToolsOnly, Names: tools}}
}

func everyTool(key string) dg.ToolkitGrant {
	return dg.ToolkitGrant{Key: key, Tools: dg.Tools{Mode: dg.ToolsAllExcept}}
}

const callerID, calleeID = 11, 21

func itemOf(kind, name, toolkit string) delegationnotice.Item {
	return delegationnotice.Item{Kind: kind, Name: name, Toolkit: toolkit}
}

// The callee X holds Git (toolkit 1, every tool) and Jira (toolkit 2: read and
// delete). Its frozen version also names a built-in the rule must leave alone.
const calleeVersion = `{"name":"x","tools":[
  {"id":1,"type":"github","toolkit_name":"Git","settings":{}},
  {"id":2,"type":"jira","toolkit_name":"Jira","settings":{"selected_tools":["read","delete"]}},
  {"type":"artifact","toolkit_name":"Files","settings":{}},
  {"type":"application","settings":{"application_id":5}}
]}`

func callees(callerSet dg.GrantSet) fakeCallees {
	return fakeCallees{
		callerID: {ApplicationID: callerID, Name: "Supervisor", Set: callerSet},
		calleeID: {ApplicationID: calleeID, Name: "Reader", Set: dg.GrantSet{Toolkits: []dg.ToolkitGrant{
			everyTool("toolkit:1"), onlyTools("toolkit:2", "delete", "read"),
		}}},
	}
}

func run(t *testing.T, settings platformconfig.DelegationSettings, callerSet dg.GrantSet, grants fakeGrants) (json.RawMessage, Outcome, error) {
	t.Helper()
	evaluator, err := New(fakeSettings{settings}, callees(callerSet), grants)
	if err != nil {
		t.Fatal(err)
	}
	var outcome Outcome
	hook := evaluator.Hook(Edge{
		ProjectID: 3, CallerApplicationID: callerID, CallerVersionID: 12,
		CalleeApplicationID: calleeID, CalleeVersionID: 22, NoticeID: "key-1",
	}, &outcome)
	narrowed, err := hook(context.Background(), json.RawMessage(calleeVersion))
	return narrowed, outcome, err
}

type frozenTool struct {
	ID       int            `json:"id"`
	Type     string         `json:"type"`
	Settings map[string]any `json:"settings"`
}

func toolsOf(t *testing.T, version json.RawMessage) []frozenTool {
	t.Helper()
	var decoded struct {
		Tools []frozenTool `json:"tools"`
	}
	if err := json.Unmarshal(version, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.Tools
}

func grantFor(set dg.GrantSet, kind dg.EdgeKind) dg.Grant {
	return dg.Grant{ID: 9, Edge: dg.Edge{CallerEntityID: callerID, CalleeEntityID: calleeID, Kind: kind}, GrantSet: set, State: dg.StateStale}
}

func TestANilEvaluatorChangesNothing(t *testing.T) {
	var evaluator *Evaluator
	if hook := evaluator.Hook(Edge{}, &Outcome{}); hook != nil {
		t.Fatal("a nil evaluator produced a hook")
	}
}

// A relayed cloud agent of a desktop thread is the turn's own agent: no edge.
func TestAnAgentIsNotDelegatedToItself(t *testing.T) {
	evaluator, err := New(fakeSettings{}, fakeCallees{}, fakeGrants{})
	if err != nil {
		t.Fatal(err)
	}
	if hook := evaluator.Hook(Edge{CallerApplicationID: calleeID, CalleeApplicationID: calleeID}, &Outcome{}); hook != nil {
		t.Fatal("an agent's own relay was evaluated as an edge")
	}
}

// An edge with an approved grant runs the callee with own ∩ (E(Y) ∪ G): what the
// callee holds beyond that is cut out of its frozen tools and shown.
func TestAGrantNarrowsTheChildToWhatItCoversAndShowsIt(t *testing.T) {
	caller := dg.GrantSet{Toolkits: []dg.ToolkitGrant{onlyTools("toolkit:1", "clone")}}
	// The grant was approved when Jira held only `read`; `delete` came later.
	grant := grantFor(dg.GrantSet{Toolkits: []dg.ToolkitGrant{everyTool("toolkit:1"), onlyTools("toolkit:2", "read")}}, dg.EdgeAgentTool)
	narrowed, outcome, err := run(t, platformconfig.DelegationSettings{Stored: true, Mode: platformconfig.DelegationEnforce}, caller, fakeGrants{grant})
	if err != nil {
		t.Fatal(err)
	}
	tools := toolsOf(t, narrowed)
	if len(tools) != 4 {
		t.Fatalf("tools = %+v", tools)
	}
	jira := tools[1].Settings["selected_tools"].([]any)
	if len(jira) != 1 || jira[0] != "read" {
		t.Fatalf("Jira keeps %v, want [read]", jira)
	}
	if tools[2].Type != "artifact" || tools[3].Type != "application" {
		t.Fatalf("a built-in or a nested application was touched: %+v", tools)
	}
	if outcome.Notice == nil || outcome.Notice.Kind != "narrowed" || outcome.Notice.Reason != "stale" || outcome.Notice.GrantID != 9 {
		t.Fatalf("notice = %+v", outcome.Notice)
	}
	if outcome.Notice.Edge.CalleeName != "Reader" || outcome.Notice.Edge.CallerName != "Supervisor" || outcome.Notice.Edge.Kind != "agent_tool" ||
		outcome.Notice.ID != "key-1" {
		t.Fatalf("notice edge = %+v", outcome.Notice)
	}
	if len(outcome.Notice.Missing) != 1 || outcome.Notice.Missing[0] != (itemOf("tool", "delete", "Jira")) {
		t.Fatalf("missing = %+v", outcome.Notice.Missing)
	}
}

func TestAGrantThatCoversTheChildLeavesItAlone(t *testing.T) {
	grant := grantFor(dg.GrantSet{Toolkits: []dg.ToolkitGrant{everyTool("toolkit:1"), onlyTools("toolkit:2", "delete", "read")}}, dg.EdgePipelineChild)
	narrowed, outcome, err := run(t, platformconfig.DelegationSettings{}, dg.GrantSet{}, fakeGrants{grant})
	if err != nil || outcome.Notice != nil || string(narrowed) != calleeVersion {
		t.Fatalf("narrowed %s notice %+v err %v", narrowed, outcome.Notice, err)
	}
}

// No grant, and the callee holds Jira, which its caller lacks: enforce refuses,
// naming the first thing missing; the refusal is the notice.
func TestNoGrantRefusesAChildThatHoldsMoreThanItsCaller(t *testing.T) {
	caller := dg.GrantSet{Toolkits: []dg.ToolkitGrant{everyTool("toolkit:1")}}
	_, outcome, err := run(t, platformconfig.DelegationSettings{Stored: true, Mode: platformconfig.DelegationEnforce}, caller, nil)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v", err)
	}
	notice := refused.Notice
	if notice.Kind != "refused" || notice.Reason != "no_grant" || notice.Code != "DELEGATION_GRANT_MISSING" ||
		len(notice.Missing) != 1 || notice.Missing[0].Kind != "toolkit" || notice.Missing[0].Name != "Jira" {
		t.Fatalf("notice = %+v", notice)
	}
	if outcome.Notice != nil {
		t.Fatal("a refused start also reported a narrowing")
	}
}

func TestNoGrantInReportModeRunsTheChildAsFrozen(t *testing.T) {
	caller := dg.GrantSet{Toolkits: []dg.ToolkitGrant{everyTool("toolkit:1")}}
	narrowed, outcome, err := run(t, platformconfig.DelegationSettings{Stored: true, Mode: platformconfig.DelegationReport}, caller, nil)
	if err != nil || outcome.Notice != nil || string(narrowed) != calleeVersion {
		t.Fatalf("narrowed %s notice %+v err %v", narrowed, outcome.Notice, err)
	}
}

// No grant and the callee holds no more than its caller: it runs, cut to the
// caller's selections, with nothing to show.
func TestNoGrantRunsAChildWithinItsCallerCutToTheCallersSelections(t *testing.T) {
	caller := dg.GrantSet{Toolkits: []dg.ToolkitGrant{onlyTools("toolkit:1", "clone"), everyTool("toolkit:2")}}
	narrowed, outcome, err := run(t, platformconfig.DelegationSettings{}, caller, nil)
	if err != nil || outcome.Notice != nil {
		t.Fatalf("notice %+v err %v", outcome.Notice, err)
	}
	tools := toolsOf(t, narrowed)
	git := tools[0].Settings["selected_tools"].([]any)
	if len(git) != 1 || git[0] != "clone" {
		t.Fatalf("Git keeps %v, want [clone]", git)
	}
	if len(tools[1].Settings["selected_tools"].([]any)) != 2 {
		t.Fatalf("Jira lost tools it holds: %v", tools[1].Settings)
	}
}

// A toolkit narrowed out is removed from the frozen tools; a toolkit the rule
// has no row for (a built-in) stays.
func TestANarrowedOutToolkitIsRemoved(t *testing.T) {
	caller := dg.GrantSet{Toolkits: []dg.ToolkitGrant{everyTool("toolkit:1")}}
	grant := grantFor(dg.GrantSet{Toolkits: []dg.ToolkitGrant{everyTool("toolkit:1")}}, dg.EdgeSubAgent)
	narrowed, outcome, err := run(t, platformconfig.DelegationSettings{}, caller, fakeGrants{grant})
	if err != nil {
		t.Fatal(err)
	}
	tools := toolsOf(t, narrowed)
	if len(tools) != 3 || tools[0].ID != 1 || tools[1].Type != "artifact" || tools[2].Type != "application" {
		t.Fatalf("tools = %+v", tools)
	}
	if outcome.Notice == nil || outcome.Notice.Missing[0] != itemOf("toolkit", "Jira", "") {
		t.Fatalf("notice = %+v", outcome.Notice)
	}
}

// A frozen toolkit with no settings object is still cut: the notice says it was
// narrowed, so it must not keep every tool.
func TestANarrowedToolkitWithoutSettingsIsStillCut(t *testing.T) {
	grant := grantFor(dg.GrantSet{Toolkits: []dg.ToolkitGrant{onlyTools("toolkit:2", "read")}}, dg.EdgeAgentTool)
	evaluator, err := New(fakeSettings{}, callees(dg.GrantSet{}), fakeGrants{grant})
	if err != nil {
		t.Fatal(err)
	}
	var outcome Outcome
	version := json.RawMessage(`{"tools":[{"id":2,"type":"jira","toolkit_name":"Jira"}]}`)
	narrowed, err := evaluator.Hook(Edge{CallerApplicationID: callerID, CalleeApplicationID: calleeID}, &outcome)(context.Background(), version)
	if err != nil {
		t.Fatal(err)
	}
	selected, _ := toolsOf(t, narrowed)[0].Settings["selected_tools"].([]any)
	if len(selected) != 1 || selected[0] != "read" {
		t.Fatalf("the toolkit keeps %s", narrowed)
	}
}

// A version that no longer exists is the start's permanent refusal (422), not a
// transient failure the desktop retries.
func TestAVersionThatNoLongerExistsIsAPermanentRefusal(t *testing.T) {
	evaluator, err := New(fakeSettings{}, fakeCallees{}, fakeGrants{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = evaluator.Hook(Edge{CallerApplicationID: callerID, CalleeApplicationID: calleeID}, &Outcome{})(context.Background(), json.RawMessage(calleeVersion))
	if !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnUnreadableSourceFailsTheStartInsteadOfRunningUnbounded(t *testing.T) {
	evaluator, err := New(fakeSettings{}, fakeCallees{}, fakeGrants{})
	if err != nil {
		t.Fatal(err)
	}
	hook := evaluator.Hook(Edge{CallerApplicationID: callerID, CalleeApplicationID: calleeID}, &Outcome{})
	if _, err := hook(context.Background(), json.RawMessage(calleeVersion)); err == nil {
		t.Fatal("a callee that could not be read was started")
	}
}
