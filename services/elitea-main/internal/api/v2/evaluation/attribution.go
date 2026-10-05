package evaluation

// Evaluation spend attribution (legacy issue 6677).
//
// Every model call a run makes is signed with an attribution id, which the
// gateway stores as gateway.llm_request_logs.execution_id. The ids name the
// run, the case and the ROLE of the call:
//
//	eval:<run>:case:<case>   the agent turn that answers the case
//	eval:<run>:judge:<case>  every judge call that grades that answer
//
// So "what did run 12 cost" is one prefix (`eval:12:`), and "what did the
// agent cost against what the judge cost" is a second segment. The agent and
// the judge are separate roles and the analytics reads never add one into the
// other's figure (internal/infra/db/repos/analytics_evaluation.go).
//
// The run id is the tenant table's integer id, so the id is unique only
// inside one project. That is enough: every analytics read is already scoped
// by the project column the log row carries.
//
// Before this file the agent turn and the judge call sent NO execution id,
// and their spend was indistinguishable from a browser's /predict_llm turn.

const (
	// AttributionPrefix starts every evaluation attribution id.
	AttributionPrefix = "eval:"
	// AttributionRoleAgent and AttributionRoleJudge are the third segment.
	AttributionRoleAgent = "case"
	AttributionRoleJudge = "judge"
)

// RunAttributionPrefix is the prefix every model call of one run carries.
func RunAttributionPrefix(runID string) string {
	return AttributionPrefix + runID + ":"
}

// AgentAttributionID is the id of the agent turn for one case.
func AgentAttributionID(runID, caseID string) string {
	return RunAttributionPrefix(runID) + AttributionRoleAgent + ":" + caseID
}

// JudgeAttributionID is the id of every judge call for one case.
func JudgeAttributionID(runID, caseID string) string {
	return RunAttributionPrefix(runID) + AttributionRoleJudge + ":" + caseID
}
