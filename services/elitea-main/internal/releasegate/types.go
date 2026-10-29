// Package releasegate is the evaluation release gate of ADR-0040
// decision 4 and the scheduled re-evaluation of decision 5.
//
// A gate suite is an evaluation dataset of an agent marked as a gate, with
// a pass criterion per dimension (SuiteConfig). A gate (StartGate) runs
// every gate suite against a candidate version through the evaluation
// orchestrator, and runs the regression suite (and every suite with a
// `no_worse_than` criterion) against the current release as well. When
// every run is terminal, Result evaluates the criteria (Evaluate) and
// stores the outcome once: `passed`, or `failed` with typed failures. A
// governance-role holder may override a failed gate with a reason
// (Override); the override is recorded beside the outcome, which stays
// `failed`.
//
// A scheduled gate runs the gate suites against the current release
// itself, on the cadence of the suite config (`reevaluate_every_days`), and
// compares with the reference run of that release: the candidate runs of
// its earliest passed scheduled gate. RegressedSince tells the
// recertification routine that such a gate failed.
//
// The package does not create releases, change requests or approvals, and
// does not check permissions: the routes and the change-request flow do.
package releasegate

import (
	"errors"
	"time"
)

// Gate states.
const (
	StatePending = "pending"
	StatePassed  = "passed"
	StateFailed  = "failed"
)

// Gate kinds.
const (
	KindChange    = "change"
	KindScheduled = "scheduled"
)

// Run roles inside a gate.
const (
	RoleCandidate = "candidate"
	RoleBaseline  = "baseline"
)

// Criterion kinds.
const (
	// CriterionThreshold: the candidate's mean normalised score on the
	// dimension is at least MinScore.
	CriterionThreshold = "threshold"
	// CriterionNoWorseThan: the candidate's mean normalised score is at
	// most MaxDrop points below the current release's.
	CriterionNoWorseThan = "no_worse_than"
)

// Failure codes. Each failure of a gate carries one.
const (
	// FailureRunErrored: an evaluation run of the gate ended `errored`.
	FailureRunErrored = "gate_run_errored"
	// FailureRunCancelled: an evaluation run of the gate was cancelled.
	FailureRunCancelled = "gate_run_cancelled"
	// FailureRunMissing: an evaluation run of the gate no longer exists
	// (its dataset was deleted, which deletes its runs).
	FailureRunMissing = "gate_run_missing"
	// FailureSuiteNotRunnable: a gate suite could not start: its dataset
	// was deleted, has no active case, or names a dimension that cannot be
	// scored.
	FailureSuiteNotRunnable = "gate_suite_not_runnable"
	// FailureDimensionUnscored: a criterion's dimension has no `ok` score
	// on a run, so the criterion cannot be decided.
	FailureDimensionUnscored = "gate_dimension_unscored"
	// FailureDimensionIncomplete: a case could not be scored on a
	// criterion's dimension (an `error` or `skipped` result). The mean of
	// the other cases is not accepted in its place.
	FailureDimensionIncomplete = "gate_dimension_incomplete"
	// FailureThresholdNotMet: a `threshold` criterion failed.
	FailureThresholdNotMet = "gate_threshold_not_met"
	// FailureScoreRegressed: a `no_worse_than` criterion failed.
	FailureScoreRegressed = "gate_score_regressed"
	// FailureRegression: cases of the regression suite that passed on the
	// current release fail on the candidate. Cases lists them.
	FailureRegression = "gate_regression"
	// FailureScheduleActorRefused: a scheduled gate did not run because the
	// user who set the cadence may no longer start evaluation runs in the
	// project (or none is recorded).
	FailureScheduleActorRefused = "gate_schedule_actor_refused"
)

// Bounds.
const (
	// MaxCriteria is the most criteria of one suite. Each criterion's
	// dimension is scored by the run, and a run scores at most
	// evaluation.MaxDimensionsPerRun dimensions.
	MaxCriteria = 5
	// MaxSuites is the most gate suites of one agent. A gate starts up to
	// two runs per suite.
	MaxSuites = 10
	// MaxReasonLength is the most characters of an override reason (the
	// column CHECK).
	MaxReasonLength = 200
	// MaxReevaluateEveryDays bounds the scheduled re-evaluation cadence.
	MaxReevaluateEveryDays = 365
	// caseExcerptLength is the most characters of a case's input that a
	// regression listing names it by.
	caseExcerptLength = 120
)

// Errors.
var (
	ErrGateNotFound      = errors.New("release gate: gate not found")
	ErrVersionNotFound   = errors.New("release gate: the candidate version is not a version of this agent")
	ErrReleaseNotFound   = errors.New("release gate: the current release is not a release of this agent")
	ErrDatasetNotFound   = errors.New("release gate: the dataset does not exist in this project or belongs to another agent")
	ErrGateNotFailed     = errors.New("release gate: only a failed gate can be overridden")
	ErrAlreadyOverridden = errors.New("release gate: the gate is already overridden")
	ErrReasonRequired    = errors.New("release gate: an override needs a reason of 1 to 200 characters")
	ErrRegressionTaken   = errors.New("release gate: the agent already has a regression suite")
	ErrTooManySuites     = errors.New("release gate: the agent already has the most gate suites")
	ErrInvalidActor      = errors.New("release gate: an override needs the acting user")
	ErrInvalidConfig     = errors.New("release gate: invalid suite config")
	ErrNoStore           = errors.New("release gate: no database")
)

// Criterion is the pass criterion of one dimension of a suite.
type Criterion struct {
	DimensionID string   `json:"dimension_id"`
	Kind        string   `json:"kind"`
	MinScore    *float64 `json:"min_score,omitempty"`
	MaxDrop     *float64 `json:"max_drop,omitempty"`
}

// SuiteConfig is one gate suite of an agent.
type SuiteConfig struct {
	ApplicationID       int64       `json:"application_id"`
	DatasetID           int64       `json:"dataset_id"`
	Regression          bool        `json:"regression"`
	Criteria            []Criterion `json:"criteria"`
	ReevaluateEveryDays *int        `json:"reevaluate_every_days"`
	UpdatedBy           *int64      `json:"updated_by"`
	UpdatedAt           time.Time   `json:"updated_at"`
	// DatasetMissing is true when the dataset was deleted after it was
	// marked. It is read, not stored: a gate fails such a suite with
	// gate_suite_not_runnable.
	DatasetMissing bool `json:"dataset_missing"`
}

// NeedsBaseline reports whether the suite also runs against the current
// release: the regression suite, and a suite with a no_worse_than
// criterion.
func (s SuiteConfig) NeedsBaseline() bool {
	if s.Regression {
		return true
	}
	for _, criterion := range s.Criteria {
		if criterion.Kind == CriterionNoWorseThan {
			return true
		}
	}
	return false
}

// Loosens reports whether replacing current with next makes the gate
// easier to pass (next nil: the suite is unmarked). Loosening is:
// unmarking a suite, unsetting the regression flag, removing a criterion,
// changing a criterion's kind, lowering a threshold's min_score or raising
// a no_worse_than's max_drop. A new suite (current nil), a new criterion
// and a stricter value only tighten. The schedule is not a pass criterion.
func Loosens(current, next *SuiteConfig) bool {
	if current == nil {
		return false
	}
	if next == nil {
		return true
	}
	if current.Regression && !next.Regression {
		return true
	}
	byDimension := make(map[string]Criterion, len(next.Criteria))
	for _, criterion := range next.Criteria {
		byDimension[criterion.DimensionID] = criterion
	}
	for _, was := range current.Criteria {
		now, kept := byDimension[was.DimensionID]
		if !kept || now.Kind != was.Kind {
			return true
		}
		switch was.Kind {
		case CriterionThreshold:
			if was.MinScore != nil && (now.MinScore == nil || *now.MinScore < *was.MinScore) {
				return true
			}
		case CriterionNoWorseThan:
			if was.MaxDrop != nil && (now.MaxDrop == nil || *now.MaxDrop > *was.MaxDrop) {
				return true
			}
		}
	}
	return false
}

// DimensionIDs answers the criteria's dimensions in order: the dimensions
// a gate run of the suite scores.
func (s SuiteConfig) DimensionIDs() []string {
	ids := make([]string, 0, len(s.Criteria))
	for _, criterion := range s.Criteria {
		ids = append(ids, criterion.DimensionID)
	}
	return ids
}

// GateRun is one evaluation run of a gate.
type GateRun struct {
	DatasetID            int64  `json:"dataset_id"`
	Role                 string `json:"role"`
	ApplicationVersionID *int64 `json:"application_version_id"`
	EvalRunID            int64  `json:"eval_run_id"`
	// Reused marks a baseline run that an earlier gate started.
	Reused bool `json:"reused"`
}

// RegressedCase is one case of the regression suite that passed on the
// current release and fails on the candidate.
type RegressedCase struct {
	CaseID string `json:"case_id"`
	// Name is the start of the case's input (cases have no other name),
	// or empty when the case was deleted.
	Name string `json:"name"`
	// DimensionIDs are the dimensions on which the case regressed.
	DimensionIDs []string `json:"dimension_ids"`
}

// GateFailure is one reason a gate failed.
type GateFailure struct {
	Code        string          `json:"code"`
	DatasetID   int64           `json:"dataset_id,omitempty"`
	Role        string          `json:"role,omitempty"`
	EvalRunID   int64           `json:"eval_run_id,omitempty"`
	DimensionID string          `json:"dimension_id,omitempty"`
	Score       *float64        `json:"score,omitempty"`
	Baseline    *float64        `json:"baseline,omitempty"`
	Required    *float64        `json:"required,omitempty"`
	Cases       []RegressedCase `json:"cases,omitempty"`
	Message     string          `json:"message"`
}

// Criterion outcome statuses.
const (
	CriterionPassed        = "passed"
	CriterionFailed        = "failed"
	CriterionNotApplicable = "not_applicable"
	CriterionNotEvaluated  = "not_evaluated"
)

// CriterionOutcome is the result of one criterion.
type CriterionOutcome struct {
	Criterion
	Status string `json:"status"`
	// Score is the candidate's mean normalised score on the dimension.
	Score *float64 `json:"score"`
	// Baseline is the current release's mean, when a baseline ran.
	Baseline *float64 `json:"baseline"`
	// Required is the lowest passing score.
	Required *float64 `json:"required"`
	Note     string   `json:"note,omitempty"`
}

// SuiteOutcome is the result of one suite.
type SuiteOutcome struct {
	DatasetID  int64              `json:"dataset_id"`
	Regression bool               `json:"regression"`
	Criteria   []CriterionOutcome `json:"criteria"`
	// RegressedCases is the regression listing of the regression suite.
	RegressedCases []RegressedCase `json:"regressed_cases"`
}

// Override records a governance-role holder accepting a failed gate.
type Override struct {
	ActorID int64     `json:"actor_id"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

// GateResult is a gate with its runs and outcome.
type GateResult struct {
	ID                 int64          `json:"id"`
	TenantProjectID    int64          `json:"-"`
	ApplicationID      int64          `json:"application_id"`
	Kind               string         `json:"kind"`
	CandidateVersionID int64          `json:"candidate_version_id"`
	CurrentReleaseID   *int64         `json:"current_release_id"`
	BaselineVersionID  *int64         `json:"baseline_version_id"`
	State              string         `json:"state"`
	Suites             []SuiteConfig  `json:"suites"`
	Runs               []GateRun      `json:"runs"`
	Outcome            []SuiteOutcome `json:"outcome"`
	Failures           []GateFailure  `json:"failures"`
	CreatedBy          *int64         `json:"created_by"`
	CreatedAt          time.Time      `json:"created_at"`
	EvaluatedAt        *time.Time     `json:"evaluated_at"`
	Override           *Override      `json:"override"`
}

// Overridden reports whether a governance-role holder accepted the failed
// gate.
func (g GateResult) Overridden() bool { return g.Override != nil }

// Blocks reports whether the gate blocks approval: it is pending, or it
// failed and was not overridden.
func (g GateResult) Blocks() bool {
	return g.State == StatePending || (g.State == StateFailed && g.Override == nil)
}
