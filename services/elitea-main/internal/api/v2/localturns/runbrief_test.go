package localturns

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const (
	briefRoot   = "0123456789abcdef0123456789abcdef"
	briefRun    = "fedcba9876543210fedcba9876543210"
	briefURL    = "/api/v2/elitea_core/run_brief/prompt_lib/1"
	checkURL    = "/api/v2/elitea_core/run_check/prompt_lib/1"
	foreignRoot = "ffffffffffffffffffffffffffffffff"
)

// fakeRunBriefs is the run brief service with one chain.
type fakeRunBriefs struct {
	chain       runbrief.ChainBrief
	writes      int
	requests    []string
	transcripts []string
	result      runbrief.Result
	checked     bool
	checkErr    error
}

func (f *fakeRunBriefs) Chain(_ context.Context, root string) (runbrief.ChainBrief, bool, error) {
	if root != f.chain.RootExecutionID {
		return runbrief.ChainBrief{}, false, nil
	}
	return f.chain, true, nil
}

func (f *fakeRunBriefs) EnsureBrief(_ context.Context, _ string, request string) (runbrief.ChainBrief, bool, error) {
	if f.chain.State == unattended.BriefPending {
		f.writes++
		f.requests = append(f.requests, request)
		f.chain.State = unattended.BriefReady
		f.chain.Brief = &runbrief.Brief{Goal: "g", DoneWhen: []runbrief.Condition{{ID: "c1", Text: "t"}},
			Constraints: []string{}, Assumptions: []string{}, Ambiguity: runbrief.AmbiguityLow}
	}
	return f.chain, true, nil
}

func (f *fakeRunBriefs) Check(
	ctx context.Context, _ string, executionID string, transcript func(context.Context) (string, error),
) (runbrief.Result, bool, error) {
	if f.checkErr != nil {
		return runbrief.Result{}, false, f.checkErr
	}
	text, _ := transcript(ctx)
	f.transcripts = append(f.transcripts, executionID+":"+text)
	return f.result, f.checked, nil
}

func desktopChain() *fakeRunBriefs {
	return &fakeRunBriefs{chain: runbrief.ChainBrief{
		RootExecutionID: briefRoot, ProjectID: 1, OwnerUserID: 7, Origin: unattended.OriginDesktop,
		State: unattended.BriefPending, Failures: map[string]int{},
	}}
}

func briefBody(root string) string {
	return `{"chain_root_execution_id":"` + root + `","request":"Make the login test pass."}`
}

func TestRunBriefWritesOnceAndAnswersTheStoredBrief(t *testing.T) {
	goals := desktopChain()
	h := &handler{useCase: &fakeUseCase{}, goals: goals}
	for range 2 {
		response := serve(t, h.runBrief, tokenUser(), briefURL, RunBriefPath, briefBody(briefRoot))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body)
		}
		var body struct {
			State string          `json:"state"`
			Brief *runbrief.Brief `json:"brief"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.State != "ready" ||
			body.Brief == nil || body.Brief.Goal != "g" {
			t.Fatalf("body = %s", response.Body)
		}
	}
	if goals.writes != 1 || goals.requests[0] != "Make the login test pass." {
		t.Fatalf("writes = %d, requests = %v", goals.writes, goals.requests)
	}
}

func TestRunBriefAnswersAStateWithoutABrief(t *testing.T) {
	goals := desktopChain()
	goals.chain.State = unattended.BriefOff
	h := &handler{useCase: &fakeUseCase{}, goals: goals}
	response := serve(t, h.runBrief, tokenUser(), briefURL, RunBriefPath, briefBody(briefRoot))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"state":"off"}` ||
		goals.writes != 0 {
		t.Fatalf("status = %d, body %s, writes %d", response.Code, response.Body, goals.writes)
	}
}

func TestRunBriefServesOnlyTheOwnersDesktopChain(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fakeRunBriefs)
		url    string
		root   string
	}{
		"unknown chain": {func(*fakeRunBriefs) {}, briefURL, foreignRoot},
		"another owner": {func(f *fakeRunBriefs) { f.chain.OwnerUserID = 8 }, briefURL, briefRoot},
		"cloud chain":   {func(f *fakeRunBriefs) { f.chain.Origin = unattended.OriginCloud }, briefURL, briefRoot},
		"other project": {func(*fakeRunBriefs) {}, "/api/v2/elitea_core/run_brief/prompt_lib/2", briefRoot},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			goals := desktopChain()
			tc.mutate(goals)
			h := &handler{useCase: &fakeUseCase{}, goals: goals}
			response := serve(t, h.runBrief, tokenUser(), tc.url, RunBriefPath, briefBody(tc.root))
			if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "chain_not_found") ||
				goals.writes != 0 {
				t.Fatalf("status = %d, body %s, writes %d", response.Code, response.Body, goals.writes)
			}
		})
	}
}

func TestRunBriefRefusesBadCallers(t *testing.T) {
	h := &handler{useCase: &fakeUseCase{}, goals: desktopChain()}
	session := &auth.User{ID: "7", UserID: "7", AuthType: "session"}
	if response := serve(t, h.runBrief, session, briefURL, RunBriefPath, briefBody(briefRoot)); response.Code != http.StatusForbidden {
		t.Fatalf("session: status = %d", response.Code)
	}
	if response := serve(t, h.runBrief, tokenUser(), briefURL, RunBriefPath, briefBody("nope")); response.Code != http.StatusBadRequest {
		t.Fatalf("bad root: status = %d", response.Code)
	}
	if response := serve(t, h.runBrief, tokenUser(), briefURL, RunBriefPath,
		`{"chain_root_execution_id":"`+briefRoot+`"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("no request: status = %d", response.Code)
	}
	none := &handler{useCase: &fakeUseCase{}}
	if response := serve(t, none.runBrief, tokenUser(), briefURL, RunBriefPath, briefBody(briefRoot)); response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), "run_brief_unavailable") {
		t.Fatalf("not composed: status = %d", response.Code)
	}
}

func checkBody(run string) string {
	return `{"chain_root_execution_id":"` + briefRoot + `","execution_id":"` + run + `","transcript":"User: go"}`
}

func TestRunCheckAnswersTheCheck(t *testing.T) {
	goals := desktopChain()
	goals.chain.State = unattended.BriefReady
	goals.checked = true
	goals.result = runbrief.Result{Outcome: runbrief.OutcomeNotMet, Met: 0, Total: 1, Reason: "r",
		ContinueText: "Continue from where you stopped.", Conditions: []runbrief.ConditionResult{
			{ID: "c1", Text: "t", Verdict: runbrief.VerdictNotMet, Reason: "r"},
		}}
	h := &handler{useCase: &fakeUseCase{}, goals: goals}
	response := serve(t, h.runCheck, tokenUser(), checkURL, RunCheckPath, checkBody(briefRun))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if body["outcome"] != "not_met" || body["continue_text"] != "Continue from where you stopped." ||
		body["total"] != float64(1) || len(goals.transcripts) != 1 || goals.transcripts[0] != briefRun+":User: go" {
		t.Fatalf("body = %v, transcripts = %v", body, goals.transcripts)
	}
}

func TestRunCheckWithoutACheckIsNotChecked(t *testing.T) {
	goals := desktopChain()
	h := &handler{useCase: &fakeUseCase{}, goals: goals}
	response := serve(t, h.runCheck, tokenUser(), checkURL, RunCheckPath, checkBody(briefRun))
	if response.Code != http.StatusOK ||
		strings.TrimSpace(response.Body.String()) != `{"outcome":"not_checked","conditions":[],"met":0,"total":0}` {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
}

func TestRunCheckRefusesATurnOutsideTheChain(t *testing.T) {
	goals := desktopChain()
	goals.checkErr = runbrief.ErrNotLink
	h := &handler{useCase: &fakeUseCase{}, goals: goals}
	response := serve(t, h.runCheck, tokenUser(), checkURL, RunCheckPath, checkBody(foreignRoot))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "chain_not_found") {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
	other := desktopChain()
	other.chain.OwnerUserID = 9
	h = &handler{useCase: &fakeUseCase{}, goals: other}
	if response := serve(t, h.runCheck, tokenUser(), checkURL, RunCheckPath, checkBody(briefRun)); response.Code != http.StatusNotFound {
		t.Fatalf("foreign chain: status = %d", response.Code)
	}
}

func TestStartBodyReadsGoalCheckAndKind(t *testing.T) {
	var body startBody
	if err := json.Unmarshal([]byte(`{"unattended":{"grant_id":"0b7d1f6a-3c2e-4f8a-9d10-2a3b4c5d6e7f",
		"chain_root_execution_id":"`+briefRoot+`","kind":"goal_not_met","goal_check":false}}`), &body); err != nil {
		t.Fatal(err)
	}
	request := body.Unattended.request()
	if request.Kind != unattended.LinkGoalNotMet || request.GoalCheck {
		t.Fatalf("request = %+v", request)
	}
	var absent startBody
	if err := json.Unmarshal([]byte(`{"unattended":{"grant_id":"0b7d1f6a-3c2e-4f8a-9d10-2a3b4c5d6e7f"}}`), &absent); err != nil {
		t.Fatal(err)
	}
	if request := absent.Unattended.request(); !request.GoalCheck || request.Kind != "" {
		t.Fatalf("absent goal_check = %+v", request)
	}
}
