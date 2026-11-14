package evaluation

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Human scores (issue 2008).
//
// A dimension that only a person can score is bound with the `human` engine.
// The run answers each case as usual, stores the result as `pending_human` and
// never calls the judge for it. A reviewer then records a score per (case,
// dimension). Scores are APPEND-ONLY (tenant/0244): a re-score is a new row
// carrying the reviewer and a note, and the newest row is the score in force.
// Recording one turns the result into an `ok` result with the score, and
// recomputes the run's headline in the same transaction. The release gate reads
// a pending_human result as undecided and waits (internal/releasegate).

// The RBAC strings of the human score routes, granted by
// migrations/shared/0245_evaluation_human_score_permissions.sql.
const (
	PermissionHumanScoreRead   = "models.applications.evaluation.human_score.read"
	PermissionHumanScoreCreate = "models.applications.evaluation.human_score.create"
)

// MaxHumanNoteLength is the longest reviewer note, in characters.
const MaxHumanNoteLength = 2000

// HumanScore is one stored human score.
type HumanScore struct {
	ID              string  `json:"id"`
	RunID           string  `json:"run_id"`
	DatasetCaseID   string  `json:"dataset_case_id"`
	DimensionID     string  `json:"dimension_id"`
	NativeScore     float64 `json:"native_score"`
	NormalizedScore float64 `json:"normalized_score"`
	Note            string  `json:"note"`
	// ReviewerID is the person who scored. Nil only for a score recorded with
	// no authenticated user (the integration harness).
	ReviewerID *int   `json:"reviewer_id"`
	CreatedAt  string `json:"created_at"`
}

// HumanScoreInput is the POST body.
type HumanScoreInput struct {
	DatasetCaseID string  `json:"dataset_case_id"`
	DimensionID   string  `json:"dimension_id"`
	Score         float64 `json:"score"`
	Note          string  `json:"note"`
}

// HumanScoreWrite is what the repository stores: the input with its
// normalisation and target decision already computed against the RUN's
// snapshot, and the reviewer.
type HumanScoreWrite struct {
	RunID           string
	DatasetCaseID   string
	DimensionID     string
	NativeScore     float64
	NormalizedScore float64
	TargetMet       *bool
	Note            string
	ReviewerID      *int
}

// HumanScoreRecorded is the outcome of recording a score.
type HumanScoreRecorded struct {
	Score  HumanScore `json:"human_score"`
	Result RunResult  `json:"result"`
	// HeadlineScore is the run's headline after this score.
	HeadlineScore *float64 `json:"headline_score"`
}

// HumanScoreRepository is the human score storage.
type HumanScoreRepository interface {
	ListHumanScores(ctx context.Context, projectID, runID string) ([]HumanScore, error)
	// RecordHumanScore appends the score, turns the (run, case, dimension)
	// result into an `ok` result carrying it and recomputes the run's headline,
	// in one transaction. A result that does not exist, or that is not
	// awaiting or carrying a human score, is refused.
	RecordHumanScore(ctx context.Context, projectID string, score HumanScoreWrite) (HumanScoreRecorded, error)
}

// HumanScoreHandler serves the human score routes.
type HumanScoreHandler struct {
	runs   RunRepository
	scores HumanScoreRepository
}

func NewHumanScoreHandler(runs RunRepository, scores HumanScoreRepository) *HumanScoreHandler {
	return &HumanScoreHandler{runs: runs, scores: scores}
}

type humanScoreListResponse struct {
	Rows  []HumanScore `json:"rows"`
	Total int          `json:"total"`
}

func (h *HumanScoreHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, runID := chi.URLParam(r, "projectID"), chi.URLParam(r, "runID")
	if _, err := h.runs.GetRun(r.Context(), projectID, runID); err != nil {
		apierr.Write(w, err)
		return
	}
	scores, err := h.scores.ListHumanScores(r.Context(), projectID, runID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if scores == nil {
		scores = []HumanScore{}
	}
	writeJSON(w, http.StatusOK, humanScoreListResponse{Rows: scores, Total: len(scores)})
}

func (h *HumanScoreHandler) Create(w http.ResponseWriter, r *http.Request) {
	projectID, runID := chi.URLParam(r, "projectID"), chi.URLParam(r, "runID")
	var input HumanScoreInput
	if !decodeStrict(w, r, &input) {
		return
	}
	run, err := h.runs.GetRun(r.Context(), projectID, runID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	write, err := PrepareHumanScore(run, input)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	write.RunID = runID
	write.ReviewerID = callerUserID(r)
	recorded, err := h.scores.RecordHumanScore(r.Context(), projectID, write)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, recorded)
}

// PrepareHumanScore validates a score against the run's frozen snapshot and
// computes its normalisation and target decision. The snapshot, not the live
// library row, decides: a dimension edited after the run must not re-scale it.
func PrepareHumanScore(run Run, input HumanScoreInput) (HumanScoreWrite, error) {
	if _, err := strconv.Atoi(input.DatasetCaseID); err != nil || input.DatasetCaseID == "" {
		return HumanScoreWrite{}, apierr.BadRequest("dataset_case_id must be a case id of this run")
	}
	var binding *SnapshotBinding
	for index := range run.Snapshot.Bindings {
		if run.Snapshot.Bindings[index].DimensionID == input.DimensionID {
			binding = &run.Snapshot.Bindings[index]
			break
		}
	}
	if binding == nil {
		return HumanScoreWrite{}, apierr.BadRequest("dimension " + input.DimensionID + " is not part of this run")
	}
	if binding.Engine != EngineHuman {
		return HumanScoreWrite{}, apierr.BadRequest("this run scores that dimension with the " + binding.Engine + " engine, not by a human")
	}
	inRun := false
	for _, snapshotCase := range run.Snapshot.Cases {
		if snapshotCase.ID == input.DatasetCaseID {
			inRun = true
			break
		}
	}
	if !inRun {
		return HumanScoreWrite{}, apierr.BadRequest("case " + input.DatasetCaseID + " is not part of this run")
	}
	dimension, known := run.Snapshot.Dimensions[input.DimensionID]
	if !known {
		return HumanScoreWrite{}, apierr.BadRequest("this run's snapshot has no dimension " + input.DimensionID)
	}
	if err := checkHumanScale(dimension, input.Score); err != nil {
		return HumanScoreWrite{}, err
	}
	if len([]rune(input.Note)) > MaxHumanNoteLength {
		return HumanScoreWrite{}, apierr.FieldTooLong("note", MaxHumanNoteLength)
	}
	normalized, ok := NormalizeScore(input.Score, dimension.ScaleType, dimension.ScaleMin, dimension.ScaleMax, dimension.Polarity)
	if !ok {
		return HumanScoreWrite{}, apierr.BadRequest("the score cannot be normalised against this dimension's scale")
	}
	write := HumanScoreWrite{
		DatasetCaseID:   input.DatasetCaseID,
		DimensionID:     input.DimensionID,
		NativeScore:     input.Score,
		NormalizedScore: normalized,
		Note:            strings.TrimSpace(input.Note),
	}
	if binding.Target != nil && binding.TargetOperator != "" {
		if met, decidable := EvaluateTargetMet(input.Score, binding.TargetOperator, *binding.Target); decidable {
			write.TargetMet = &met
		}
	}
	return write, nil
}

// checkHumanScale refuses a score outside the dimension's own scale. A person
// typing 7 on a 0..5 scale made a mistake; clamping it would store a score
// nobody gave.
func checkHumanScale(dimension SnapshotDimension, score float64) error {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return apierr.BadRequest("score must be a number")
	}
	if dimension.ScaleType == ScaleBinary {
		if score != 0 && score != 1 {
			return apierr.BadRequest("score must be 0 or 1 on a binary dimension")
		}
		return nil
	}
	if score < dimension.ScaleMin || score > dimension.ScaleMax {
		return apierr.BadRequest(fmt.Sprintf("score must be between %g and %g", dimension.ScaleMin, dimension.ScaleMax))
	}
	return nil
}

// HeadlineFromScores averages normalised scores into the run's headline, or
// nil when there is none. It is the one definition: the orchestrator uses it
// when a run finishes and the repository when a human score lands, so the two
// cannot disagree.
func HeadlineFromScores(scores []float64) *float64 {
	if len(scores) == 0 {
		return nil
	}
	sum := 0.0
	for _, score := range scores {
		sum += score
	}
	rounded := roundTo2(sum / float64(len(scores)))
	return &rounded
}
