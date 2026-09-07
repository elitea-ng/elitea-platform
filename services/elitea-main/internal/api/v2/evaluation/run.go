package evaluation

import (
	"context"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// The RBAC strings the run routes gate on. Transcribed from the reference's
// EVAL_PERMISSIONS block; granted by
// migrations/shared/0115_evaluation_dataset_run_permissions.sql.
//
// There is no `run.cancel` string in the reference vocabulary, so CANCEL is
// gated on PermissionRunCreate — the same right that started the run.
// Inventing a name would either widen the product's declared vocabulary or,
// left ungranted, ship a permanent 403 (#313's shape).
const (
	PermissionRunRead   = "models.applications.evaluation.run.read"
	PermissionRunCreate = "models.applications.evaluation.run.create"
)

// The run status vocabulary, verbatim from the reference's EVAL_RUN_STATUS.
//
// The words are the reference's and not this repository's usual
// queued/done/failed, deliberately: the client renders them, `isRunTerminal`
// and `isRunActive` in `run.helpers.js` switch on exactly these five strings,
// and a status the client does not know renders as a blank chip. The mapping
// to the generic state machine is created=queued, finished=done,
// errored=failed.
const (
	RunStatusCreated   = "created"
	RunStatusRunning   = "running"
	RunStatusFinished  = "finished"
	RunStatusErrored   = "errored"
	RunStatusCancelled = "cancelled"
)

// Trigger vocabulary. Only `on_demand` has a writer here — `offline_batch`
// belongs to a scheduled run, which this slice does not serve.
const (
	RunTriggerOnDemand     = "on_demand"
	RunTriggerOfflineBatch = "offline_batch"
)

// ExecutionModePredictBlocking names what this slice actually does: ONE
// blocking LLM turn per case, built from the agent version's instructions and
// the case input, through the same PredictCompleter /predict_llm and the
// AI-draft routes use.
//
// IT IS RECORDED ON EVERY RUN ROW AND RETURNED BY EVERY READ, because it is
// not the agent. There are no tools, no toolkits and no conversation memory in
// this path, so a score produced by it measures the instructions and the model
// and nothing else. A number that silently claimed to measure the agent would
// be the "200 with a fallback body" this repository has shipped before; naming
// the mode is what makes the claim honest and what lets a later runtime-plane
// run be distinguished from these instead of silently re-labelling them.
const (
	ExecutionModePredictBlocking = "predict_blocking"
	ExecutionModeRuntimePlane    = "runtime_plane"
)

// Result status vocabulary, verbatim from the reference's EVAL_RESULT_STATUS.
//
// `error` is the one this slice writes most carefully. A judge that answers
// prose instead of the score schema produces `error` with the raw text kept in
// the verdict — never `ok` with a score of 0, which is a real and very bad
// score and would drag a headline down with a number nobody measured.
//
// `pending_human` and `skipped` are accepted on read (a later slice writes
// them) and are not produced here.
const (
	ResultStatusOK           = "ok"
	ResultStatusError        = "error"
	ResultStatusPendingHuman = "pending_human"
	ResultStatusSkipped      = "skipped"
)

// TerminalRunStatuses is the reference's `isRunTerminal`.
var terminalRunStatuses = map[string]bool{
	RunStatusFinished:  true,
	RunStatusErrored:   true,
	RunStatusCancelled: true,
}

// IsRunTerminal reports whether a run has stopped for good.
func IsRunTerminal(status string) bool { return terminalRunStatuses[status] }

// SnapshotDimension is one dimension AS IT WAS when the run started.
//
// The scorecard normalises against these numbers and not against the library
// row, because a dimension is editable and the normalisation divides by the
// scale range and flips on polarity — so an edit to either would re-scale a
// finished run's scores with no record that it happened.
type SnapshotDimension struct {
	Name      string  `json:"name"`
	ScaleType string  `json:"scale_type"`
	ScaleMin  float64 `json:"scale_min"`
	ScaleMax  float64 `json:"scale_max"`
	Polarity  string  `json:"polarity"`
	// Description is the rubric, and it is the AI judge's prompt. It is frozen
	// with the rest: a run scored against one rubric must not be explained by a
	// different one later.
	Description string `json:"description"`
}

// SnapshotBinding is what the suite table will hold when it exists. In this
// slice the run start route builds one per requested dimension from that
// dimension's own defaults, which is why no `eval_bindings` table is needed
// yet — see tenant/0132's header.
type SnapshotBinding struct {
	DimensionID string `json:"dimension_id"`
	// Engine is always EngineAI in this slice. The field is stored rather than
	// implied so that a `code` or `human` binding written later is
	// distinguishable from these, instead of every historical row silently
	// claiming to be whatever the newest default is.
	Engine         string   `json:"engine"`
	Weight         float64  `json:"weight"`
	Target         *float64 `json:"target"`
	TargetOperator string   `json:"target_operator"`
	OrderIndex     int      `json:"order_index"`
}

// SnapshotCase identifies a case by id and position. The case TEXT is not
// copied: the evidence on each result row carries what was actually sent, so
// duplicating it here would create a second copy that can disagree.
type SnapshotCase struct {
	ID         string `json:"id"`
	OrderIndex int    `json:"order_index"`
}

// RunSnapshot is `eval_runs.snapshot`. Its JSON shape is the reference's.
type RunSnapshot struct {
	Cases      []SnapshotCase               `json:"cases"`
	Dimensions map[string]SnapshotDimension `json:"dimensions"`
	Bindings   []SnapshotBinding            `json:"bindings"`
}

// Run is one stored run row.
type Run struct {
	ID                   string `json:"id"`
	UUID                 string `json:"uuid,omitempty"`
	DatasetID            string `json:"dataset_id"`
	ApplicationID        *int   `json:"application_id"`
	ApplicationVersionID *int   `json:"application_version_id"`
	TriggerType          string `json:"trigger_type"`
	// CreatedBy is the person who started the run. It is on the wire because
	// the scorecard names them, and it is on the ROW because the run's model
	// calls happen long after the HTTP request is over — the orchestrator has
	// no request context to read a principal from.
	CreatedBy     *int        `json:"created_by"`
	Status        string      `json:"status"`
	ExecutionMode string      `json:"execution_mode"`
	Snapshot      RunSnapshot `json:"snapshot"`
	Progress      RunProgress `json:"progress"`
	// HeadlineScore is a POINTER. A run that errored before scoring anything
	// has NO headline, which is not the same as a headline of 0 — and 0 is a
	// legitimate score, so the two must stay distinguishable on the wire.
	HeadlineScore *float64 `json:"headline_score"`
	Error         string   `json:"error,omitempty"`
	CreatedAt     string   `json:"created_at"`
	StartedAt     string   `json:"started_at,omitempty"`
	FinishedAt    string   `json:"finished_at,omitempty"`
}

// RunProgress is the reference's `progress{done,total}`.
type RunProgress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// RunResult is one score.
type RunResult struct {
	ID              string         `json:"id"`
	RunID           string         `json:"run_id"`
	DatasetCaseID   string         `json:"dataset_case_id"`
	DimensionID     string         `json:"dimension_id"`
	Status          string         `json:"status"`
	NativeScore     *float64       `json:"native_score"`
	NormalizedScore *float64       `json:"normalized_score"`
	TargetMet       *bool          `json:"target_met"`
	Verdict         map[string]any `json:"verdict"`
	Evidence        map[string]any `json:"evidence"`
	CreatedAt       string         `json:"created_at"`
}

// RunStartInput is the POST /eval_runs body.
//
// NO `suite_id`. The reference's body carries one, and there is no suite table
// in this slice; the dimensions are named directly and the server builds the
// snapshot bindings from each dimension's own defaults. When suites land, the
// field arrives beside `dimension_ids` rather than replacing it — an ad-hoc run
// over a chosen set is a thing the reference's own bootstrap flow does too.
type RunStartInput struct {
	DatasetID            string   `json:"dataset_id"`
	ApplicationID        *int     `json:"application_id"`
	ApplicationVersionID *int     `json:"application_version_id"`
	DimensionIDs         []string `json:"dimension_ids"`
	TriggerType          string   `json:"trigger_type"`
}

// MaxDimensionsPerRun bounds the fan-out. A run performs one agent call per
// case plus one judge call per (case, dimension), so the model spend is
// cases × (1 + dimensions). With MaxCasesPerDataset at 10 this caps a single
// run at 60 model calls.
const MaxDimensionsPerRun = 5

// Normalize applies the defaults a body may omit.
func (input *RunStartInput) Normalize() {
	if input.TriggerType == "" {
		input.TriggerType = RunTriggerOnDemand
	}
	seen := map[string]bool{}
	unique := make([]string, 0, len(input.DimensionIDs))
	for _, id := range input.DimensionIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	input.DimensionIDs = unique
}

// Validate refuses the bodies that could not produce a scorecard.
func (input RunStartInput) Validate() error {
	if input.DatasetID == "" {
		return apierr.BadRequest("dataset_id is required")
	}
	if len(input.DimensionIDs) == 0 {
		// NOT "run against every dimension in the library". A run costs one
		// model call per (case, dimension), and a default of "all" turns an
		// empty field into the most expensive request the route can accept.
		return apierr.BadRequest("dimension_ids is required: name at least one dimension to score against")
	}
	if len(input.DimensionIDs) > MaxDimensionsPerRun {
		return apierr.BadRequest("a run may score against at most 5 dimensions")
	}
	if input.TriggerType != RunTriggerOnDemand && input.TriggerType != RunTriggerOfflineBatch {
		return apierr.BadRequest("trigger_type must be on_demand or offline_batch")
	}
	if input.ApplicationVersionID == nil {
		// The version carries the instructions the agent turn is built from.
		// Without it there is nothing to run, and defaulting to "the latest"
		// would make a run's meaning depend on when it was read.
		return apierr.BadRequest("application_version_id is required: a run scores one specific agent version")
	}
	return nil
}

// RunListFilter narrows the run listing.
type RunListFilter struct {
	ApplicationID *int
	DatasetID     *string
	Limit         int
}

// DefaultRunListLimit and MaxRunListLimit bound the history read. The
// reference's run history panel shows the most recent runs and computes a
// delta against the nearest older SCORED run, so it needs more than one page
// of context but never the whole history.
const (
	DefaultRunListLimit = 50
	MaxRunListLimit     = 200
)

// ResultPage bounds the scorecard read. The reference's constants are
// EVAL_RESULT_MAX_LIMIT = 2000 with a backend default of 500.
const (
	DefaultResultPageLimit = 500
	MaxResultPageLimit     = 2000
)

// ResultPage is the scorecard's paging window.
type ResultPage struct {
	Limit  int
	Offset int
}

// RunRepository is the run storage this package needs.
//
// StartRun takes the SNAPSHOT already built, not the ids: building it needs the
// dimension rows and the case rows, and doing that inside the repository would
// put the product rule "an ai dimension binds with its own default weight" in
// the SQL layer where the handler's tests cannot reach it.
type RunRepository interface {
	ListRuns(ctx context.Context, projectID string, filter RunListFilter) ([]Run, error)
	GetRun(ctx context.Context, projectID, runID string) (Run, error)
	CreateRun(ctx context.Context, projectID string, run Run) (Run, error)
	CancelRun(ctx context.Context, projectID, runID string) (Run, error)
	ListResults(ctx context.Context, projectID, runID string, page ResultPage) ([]RunResult, int, error)

	// The orchestrator's own surface. It is on the same interface because it
	// reads and writes the same rows, and a second interface over one table is
	// two places for a projection to drift apart.
	ClaimRun(ctx context.Context, projectID, runID string) (Run, error)
	Heartbeat(ctx context.Context, projectID, runID string, done int) error
	SaveResult(ctx context.Context, projectID string, result RunResult) error
	FinishRun(ctx context.Context, projectID, runID, status string, headline *float64, failure string) error
	// RequeueStaleRuns moves `running` rows whose heartbeat is older than the
	// TTL back to `created`, across EVERY project schema, and answers the list
	// of (projectID, runID) it re-queued. It is what makes a run survive a
	// restart: the goroutine died with the process, and nothing else can tell
	// a dead run from a slow one.
	RequeueStaleRuns(ctx context.Context, olderThanSeconds int) ([]RunRef, error)
	// PendingRuns lists `created` runs so a fresh process picks up what it
	// re-queued (and anything queued while no worker was alive).
	PendingRuns(ctx context.Context, limit int) ([]RunRef, error)
	// AgentVersion reads the instructions and model the agent turn is built
	// from. It lives here rather than on a second repository because it reads
	// the same tenant schema through the same pool, and the orchestrator is its
	// only caller.
	AgentVersion(ctx context.Context, projectID string, versionID int) (AgentVersion, error)
	// DatasetCases reads the cases a run executes, in snapshot order.
	DatasetCases(ctx context.Context, projectID, datasetID string) ([]DatasetCase, error)
}

// RunRef identifies one run without loading it.
type RunRef struct {
	ProjectID string
	RunID     string
}

// AgentVersion is the slice of `application_versions` the agent turn needs.
type AgentVersion struct {
	ID           int
	Instructions string
	ModelName    string
}
