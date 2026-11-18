package analytics

import "strconv"

// EvalCaseExecutionsSQL is a subquery of the executions an evaluation run ran
// its cases as (tenant/0246 eval_case_runs), with the run and the case each
// belongs to: columns execution_id, run_id, dataset_case_id.
//
// An agent run's model calls are signed with its execution id and not with
// `eval:<run>:case:<case>`. Executions linked to a case's root run are the
// case's too: the continuations of a run chain (run_chain_links), which are
// separate executions with their own id (a delegated sub-agent runs inside its
// parent's execution and is already covered by it). withLinks says the project
// database has run_chain_links. run 0 selects every run of the project.
// schema must be a quoted tenant schema identifier.
func EvalCaseExecutionsSQL(schema string, run int64, withLinks bool) string {
	filter, linkFilter := "", ""
	if run > 0 {
		filter = ` WHERE c.run_id = ` + strconv.FormatInt(run, 10)
		linkFilter = filter
	}
	query := `SELECT c.execution_id, c.run_id, c.dataset_case_id FROM ` + schema + `.eval_case_runs AS c` + filter
	if withLinks {
		query += ` UNION SELECT l.execution_id, c.run_id, c.dataset_case_id FROM elitea_runtime.run_chain_links AS l` +
			` JOIN ` + schema + `.eval_case_runs AS c ON c.execution_id = l.root_execution_id` + linkFilter
	}
	return query
}
