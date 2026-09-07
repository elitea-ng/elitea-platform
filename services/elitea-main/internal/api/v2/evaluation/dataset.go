package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// The RBAC strings the dataset routes gate on, transcribed verbatim from the
// reference UI's EVAL_PERMISSIONS block
// (widgets/evaluation/lib/constants/evaluation.constants.js), exactly as the
// four dimension constants above them are.
//
// They are exported CONSTANTS rather than literals at the router's call site
// for the reason the dimension four give: router_permission_grant_gate_test.go
// resolves constants through the AST and fails unless a shared migration grants
// each of them in `default` mode, and
// migrations/shared/0115_evaluation_dataset_run_permissions.sql is that grant.
// They deliberately do NOT go through router.go's `projectPermission` helper,
// because that helper's own gate asserts pylon provenance these names do not
// have — see 0115's header and the note on the dimension constants.
const (
	PermissionDatasetRead   = "models.applications.evaluation.dataset.read"
	PermissionDatasetCreate = "models.applications.evaluation.dataset.create"
	PermissionDatasetUpdate = "models.applications.evaluation.dataset.update"
	PermissionDatasetDelete = "models.applications.evaluation.dataset.delete"
)

// Case source vocabulary. Only `manual` has a writer in this slice: `import`
// belongs to DatasetImportDialog and `conversation` to
// PromoteConversationsDialog, both of which are out of scope. The values are
// accepted on read so that a row written by a later slice renders, and refused
// on write so nothing can claim a provenance it does not have.
const (
	CaseSourceManual       = "manual"
	CaseSourceImport       = "import"
	CaseSourceConversation = "conversation"
)

// MaxCasesPerDataset is the reference's own MAX_CASES_PER_DATASET
// (evaluation.constants.js, annotated "Backend MAX_CASES_PER_DATASET in
// evaluation_dataset_utils.py (#6349)").
//
// It is enforced here rather than left to the operator because a run performs
// TWO model calls per case — the agent turn and the judge turn — so the cap is
// the only thing between a paste of a thousand rows and a bill nobody
// authorised. The refusal names the limit; it is not a silent truncation,
// which would store a dataset that reads as complete and is not.
const MaxCasesPerDataset = 10

// MaxDatasetNameLength matches the column width and the dimension editor's own
// bound, so a non-browser caller gets the same answer a person does.
const MaxDatasetNameLength = 128

// MaxCaseInputBytes bounds one case's input and expected output. Both are free
// text and both are sent to a model once per run, so an unbounded field is a
// cost and a context-window failure rather than only a storage question.
const MaxCaseInputBytes = 32 << 10 // 32 KiB

// Dataset is one stored dataset row.
//
// `application_id` and NOT `agent_id`: the reference sends `agent_id` as a
// QUERY PARAMETER and this schema stores every agent reference as
// `application_id` (eval_dimensions already does). Two names for one column
// across two tables of one feature is how a join gets written against the
// wrong one. tenant/0132's header records the deviation.
type Dataset struct {
	ID          string `json:"id"`
	UUID        string `json:"uuid,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// ApplicationID is nil for a project-wide dataset.
	ApplicationID *int `json:"application_id"`
	IsShared      bool `json:"is_shared"`
	// CaseCount is the STORED count of cases, not len(Cases). The detail read
	// is paged, so `len(cases)` is the page and not the dataset; the reference
	// reads `case_count` for the list badge and `cases_truncated` to say the
	// page is short. A client that counted the page would report a 200-case
	// dataset as having 200 when the page size is 200 and it has 900.
	CaseCount int    `json:"case_count"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// DatasetDetail is the ONE dataset read: the summary plus a page of cases.
//
// It is a SEPARATE TYPE and not two more fields on Dataset, and the reason is a
// defect this slice's own integration test caught before it shipped. With
// `cases` on Dataset it needs `omitempty` — otherwise every row of the LIST
// answers `"cases": []`, telling a client that a dataset with ten cases has
// none. And with `omitempty`, the DETAIL read of an empty dataset omits the key
// altogether, which a client reads as "not loaded yet" and waits for for ever.
// The two requirements cannot both be met by one field. Splitting the types
// settles it: the list carries no `cases` key at all, and the detail always
// carries one, empty or not.
type DatasetDetail struct {
	Dataset
	Cases []DatasetCase `json:"cases"`
	// CasesTruncated says the page is short of the dataset. It is computed
	// from the STORED count and the window, not from `len(cases) == limit` —
	// the two differ on the exact boundary, and the reference's pager trusts
	// this flag to decide whether to ask for more.
	CasesTruncated bool `json:"cases_truncated"`
}

// DatasetCase is one evaluation case.
type DatasetCase struct {
	ID        string `json:"id"`
	DatasetID string `json:"dataset_id"`
	Input     string `json:"input"`
	// Variables is the template substitution map. It is never nil on the wire:
	// the reference spreads this object, and `undefined` there is a crash.
	Variables map[string]any `json:"variables"`
	// ExpectedOutput is a POINTER. "no expected answer" and "an expected answer
	// of empty string" are different instructions to a judge: the second tells
	// it to mark every non-empty answer wrong.
	ExpectedOutput *string `json:"expected_output"`
	SourceType     string  `json:"source_type"`
	OrderIndex     int     `json:"order_index"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

// DatasetWriteInput is the create/update body.
//
// It is a separate type from Dataset rather than a reuse of it, because the
// fields a caller may set are a strict subset: `case_count`, `cases` and the
// timestamps are server-derived, and a body that accepted them would let a
// client post a `case_count` the table never agrees with.
type DatasetWriteInput struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	ApplicationID *int   `json:"application_id"`
	IsShared      bool   `json:"is_shared"`
}

// Normalize trims the free text.
func (input *DatasetWriteInput) Normalize() {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
}

// Validate reproduces on the server every rule the editor enforces before it
// will save, and the same rules tenant/0132 states as CHECK constraints.
func (input DatasetWriteInput) Validate() error {
	if input.Name == "" {
		return apierr.BadRequest("name is required")
	}
	if len(input.Name) > MaxDatasetNameLength {
		return apierr.BadRequest(fmt.Sprintf("name must be at most %d characters", MaxDatasetNameLength))
	}
	if input.ApplicationID != nil && *input.ApplicationID <= 0 {
		return apierr.BadRequest("application_id must be a positive agent id, or absent for a project-wide dataset")
	}
	return nil
}

// CaseWriteInput is the add/update-a-case body.
type CaseWriteInput struct {
	Input          string         `json:"input"`
	Variables      map[string]any `json:"variables"`
	ExpectedOutput *string        `json:"expected_output"`
}

// Normalize trims the input and replaces a nil variable map with an empty one.
//
// Nil is replaced rather than rejected because `{"input": "..."}` with no
// `variables` key is a legitimate body — a case with no template variables —
// and the column is NOT NULL. Leaving it nil would push a JSON `null` into a
// jsonb column whose CHECK requires an object, which surfaces as a 500 for
// what is a perfectly ordinary request.
func (input *CaseWriteInput) Normalize() {
	input.Input = strings.TrimSpace(input.Input)
	if input.Variables == nil {
		input.Variables = map[string]any{}
	}
	if input.ExpectedOutput != nil {
		trimmed := strings.TrimSpace(*input.ExpectedOutput)
		input.ExpectedOutput = &trimmed
	}
}

// Validate refuses the shapes a run could not execute.
func (input CaseWriteInput) Validate() error {
	if input.Input == "" {
		return apierr.BadRequest("a case needs an input: this is what the agent is asked")
	}
	if len(input.Input) > MaxCaseInputBytes {
		return apierr.BadRequest(fmt.Sprintf("input must be at most %d bytes", MaxCaseInputBytes))
	}
	if input.ExpectedOutput != nil && len(*input.ExpectedOutput) > MaxCaseInputBytes {
		return apierr.BadRequest(fmt.Sprintf("expected_output must be at most %d bytes", MaxCaseInputBytes))
	}
	// The map must survive a round trip through jsonb. A value that cannot be
	// marshalled reaches PostgreSQL as a driver error, i.e. a 500 for a caller
	// error, and json.Marshal is the only thing that can tell the two apart
	// before the write.
	if _, err := json.Marshal(input.Variables); err != nil {
		return apierr.BadRequest("variables must be a JSON object of scalar values")
	}
	return nil
}

// DatasetListFilter narrows the dataset listing to one agent's datasets plus
// the project-wide ones.
//
// The predicate is deliberately NOT "everything in this schema". The reference
// sends `agent_id` when the Evaluation tab is open on an agent, and a dataset
// authored for another agent is that agent's, so listing it would put every
// agent's cases in every other agent's editor — the same reasoning the
// dimension list applies to `agent_adhoc` rows.
type DatasetListFilter struct {
	ApplicationID *int
}

// CasePage bounds the detail read.
//
// The reference's page size is EVAL_DATASET_CASE_PAGE_SIZE = 200 and its
// backend caps at 1000. This slice caps a dataset at MaxCasesPerDataset, so a
// page is never short in practice — the paging exists anyway, because the cap
// is a product decision that can be raised and a read with no bound is the
// thing that becomes a problem silently when it is.
type CasePage struct {
	Limit  int
	Offset int
}

// DefaultCasePageLimit and MaxCasePageLimit mirror the reference's constants.
const (
	DefaultCasePageLimit = 200
	MaxCasePageLimit     = 1000
)

// DatasetRepository is the dataset storage this package needs and nothing more.
type DatasetRepository interface {
	ListDatasets(ctx context.Context, projectID string, filter DatasetListFilter) ([]Dataset, error)
	GetDataset(ctx context.Context, projectID, datasetID string, page CasePage) (DatasetDetail, error)
	CreateDataset(ctx context.Context, projectID string, input DatasetWriteInput) (Dataset, error)
	UpdateDataset(ctx context.Context, projectID, datasetID string, input DatasetWriteInput) (Dataset, error)
	DeleteDataset(ctx context.Context, projectID, datasetID string) error

	AddCase(ctx context.Context, projectID, datasetID string, input CaseWriteInput) (DatasetCase, error)
	UpdateCase(ctx context.Context, projectID, datasetID, caseID string, input CaseWriteInput) (DatasetCase, error)
	DeleteCase(ctx context.Context, projectID, datasetID, caseID string) error
}
