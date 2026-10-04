// Package evaluation serves the Agent Evaluation DIMENSION LIBRARY, and only
// that.
//
// SCOPE, STATED UP FRONT BECAUSE IT IS THE POINT.
//
// The baseline UI (frontends/EliteaUI, `src/[fsd]/widgets/evaluation/`) is 38
// operations over 19 `eval_*` path families. This package implements FOUR of
// them — the dimension list, create, update and delete. It has no suites, no
// bindings, no datasets, no cases, no runs, no results, no human scores, no
// platform catalogue and no `generate_eval_dimensions`. Those are not stubbed
// and not registered: a route that answers 200 with an empty body is
// indistinguishable, from the browser, from a feature that works and has no
// data, and this repository has shipped that shape before.
//
// The reason this one slice can land alone is that a dimension is authored,
// stored and read back with nothing behind it. A suite binds dimensions to an
// agent, a dataset feeds cases to a run, and a run needs an orchestrator, a
// judge model and a code sandbox — none of which exist in this service. The
// library does not need them.
//
// WHAT A DIMENSION IS. One reusable scoring criterion: a name, a rubric that
// doubles as the AI grading prompt, the engines allowed to score it, the scale
// it is scored on, and the direction that makes a score good.
package evaluation

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// The four RBAC strings the routes gate on, transcribed verbatim from the
// baseline's own EVAL_PERMISSIONS block
// (widgets/evaluation/lib/constants/evaluation.constants.js), whose comment
// says they "must match backend check_api decorators, §19.6".
//
// They are exported CONSTANTS rather than literals at the router's call site
// for two reasons, and both are load-bearing:
//
//   - internal/api/router_permission_grant_gate_test.go resolves constants
//     through the AST, so declaring them here still binds these routes to the
//     rule that every gated permission must be granted by a shared migration.
//     migrations/shared/0100_evaluation_dimension_permissions.sql is that
//     grant.
//   - internal/api/router_elitea_core_permission_map_test.go asserts that every
//     literal handed to router.go's `projectPermission` helper appears in
//     testdata/legacy/legacy-rbac-static-catalog.json. That assertion is
//     correct for a PORTED pylon route, where an unknown name is a typo that
//     ships as a permanent 403. Agent Evaluation is not in the pylon corpus
//     this repository carries (`legacy/plugins/elitea_core/api/v2/` has no
//     evaluation module), so these four names have no `check_api` to be
//     transcribed from and would fail an assertion about provenance they never
//     claimed. The router therefore builds the identical middleware directly
//     rather than routing a false claim through that helper. See 0100's header.
const (
	PermissionDimensionRead   = "models.applications.evaluation.dimension.read"
	PermissionDimensionCreate = "models.applications.evaluation.dimension.create"
	PermissionDimensionUpdate = "models.applications.evaluation.dimension.update"
	PermissionDimensionDelete = "models.applications.evaluation.dimension.delete"
)

// Engine is a way a dimension can be scored.
const (
	EngineAI    = "ai"
	EngineHuman = "human"
	EngineCode  = "code"
)

// Tier is the scope a dimension is authored at.
//
// `platform` is accepted on READ and refused on WRITE: the platform registry
// the baseline materialises those rows from (`materializePlatformDimension`)
// is not built in this slice, so nothing can legitimately author one yet, and
// a stored `platform` row renders read-only.
const (
	TierProject    = "project"
	TierAgentAdhoc = "agent_adhoc"
	TierPlatform   = "platform"
)

// Scale types and polarities. Both are inputs to the score normalisation the
// run engine will do; see the note on Dimension.Polarity.
const (
	ScaleBinary     = "binary"
	ScaleOrdinal    = "ordinal"
	ScaleContinuous = "continuous"

	PolarityHigherBetter = "higher_better"
	PolarityLowerBetter  = "lower_better"

	ReturnContractBool   = "bool"
	ReturnContractNumber = "number"
)

// MaxDimensionDescriptionBytes bounds the rubric. The description is the AI
// judge's prompt, so it is sent to a model for every case of every run that
// scores this dimension: an unbounded rubric is a per-run cost, not only a
// storage question.
const MaxDimensionDescriptionBytes = 32 << 10 // 32 KiB

// MaxDimensionCodeBytes bounds a code-engine dimension's validation script.
const MaxDimensionCodeBytes = 64 << 10 // 64 KiB

// MaxNameLength matches the editor's own `inputProps={{ maxLength: 128 }}` and
// the column width. Enforced here so a non-browser caller gets the same answer.
const MaxNameLength = 128

var (
	knownEngines    = map[string]bool{EngineAI: true, EngineHuman: true, EngineCode: true}
	knownTiers      = map[string]bool{TierProject: true, TierAgentAdhoc: true, TierPlatform: true}
	knownScales     = map[string]bool{ScaleBinary: true, ScaleOrdinal: true, ScaleContinuous: true}
	knownPolarities = map[string]bool{PolarityHigherBetter: true, PolarityLowerBetter: true}
	knownReturns    = map[string]bool{ReturnContractBool: true, ReturnContractNumber: true}
	// The operator set the baseline offers (TARGET_OPERATOR_OPTIONS). `==`
	// rather than `=`, because that is the string the stored comparison is
	// evaluated with (`evaluateTargetMet` in scorecard.helpers.js).
	knownOperators = map[string]bool{">=": true, ">": true, "<=": true, "<": true, "==": true}
)

// Dimension is one stored library row.
//
// Every JSON name here is the baseline's; the editor dialog posts exactly this
// body, so a rename would be a silent break rather than a compile error.
type Dimension struct {
	ID          string `json:"id"`
	UUID        string `json:"uuid,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Tier        string `json:"tier"`
	// ApplicationID is set only for the `agent_adhoc` tier. A pointer, so
	// "not scoped to an agent" is a JSON null rather than agent 0.
	ApplicationID  *int     `json:"application_id"`
	AllowedEngines []string `json:"allowed_engines"`
	ScaleType      string   `json:"scale_type"`
	ScaleMin       float64  `json:"scale_min"`
	ScaleMax       float64  `json:"scale_max"`
	// Polarity has no default and no zero value that is safe. It is applied
	// LAST in normalisation, so an inverse metric (toxicity, latency) stored
	// without it scores a good answer 0 and nothing anywhere reports that.
	// The baseline leaves it deliberately unset in its default form for the
	// same reason and refuses to save until the author states it.
	Polarity      string  `json:"polarity"`
	DefaultWeight float64 `json:"default_weight"`
	// DefaultTarget and DefaultTargetOperator are required together or not at
	// all: a target with no operator is not comparable, and an operator with
	// no target compares against nothing.
	DefaultTarget         *float64 `json:"default_target"`
	DefaultTargetOperator string   `json:"default_target_operator"`
	// Code and ReturnContract are code-engine authoring only.
	Code           string `json:"code"`
	ReturnContract string `json:"return_contract"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// IsCodeEngine reports the code-only shape: `allowed_engines == ['code']`.
//
// This is the single predicate the mutual-exclusion rule is written in terms
// of, in the handler, in the repository and in the client. The baseline's own
// `isCodeOnly` is the same test (DimensionEditorDialog.jsx:26).
func (d Dimension) IsCodeEngine() bool {
	return len(d.AllowedEngines) == 1 && d.AllowedEngines[0] == EngineCode
}

// Repository is the storage this package needs and nothing more.
type Repository interface {
	List(ctx context.Context, projectID string, filter ListFilter) ([]Dimension, error)
	Create(ctx context.Context, projectID string, dimension Dimension) (Dimension, error)
	Update(ctx context.Context, projectID, dimensionID string, dimension Dimension) (Dimension, error)
	Delete(ctx context.Context, projectID, dimensionID string) error
}

// ListFilter narrows the library listing.
//
// The baseline sends `include_platform` and an optional `agent_id`
// (evaluationApi.js:29-39). The listing is "this project's own dimensions,
// plus the ad-hoc ones belonging to the agent being edited" — an ad-hoc
// dimension authored on ANOTHER agent must not appear, or every agent's editor
// grows every other agent's rubrics. This is a scope filter, not an access
// boundary (see Handler.List): any project editor may edit any agent.
type ListFilter struct {
	IncludePlatform bool
	ApplicationID   *int
}

// normalizeEngines lowercases, trims, de-duplicates and sorts the engine set.
//
// Sorting is what makes `['code']` a decidable shape rather than an ordering
// accident, and it makes two dimensions with the same engines compare equal.
// The order is alphabetical (ai, code, human), which is not the display order —
// display order belongs to the client.
func normalizeEngines(engines []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(engines))
	for _, engine := range engines {
		trimmed := strings.ToLower(strings.TrimSpace(engine))
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	sort.Strings(out)
	return out
}

// Normalize trims the free text and canonicalises the engine set, then clears
// the fields that only mean something for the other engine shape.
//
// Clearing rather than rejecting is the baseline's behaviour: its save body
// sends `code: isCode ? form.code : null` and the same for `return_contract`,
// so a dimension switched from Code to AI does not keep a script nothing will
// ever run. A stored script on an AI dimension is worse than useless — it
// reads as an executable the sandbox would honour.
func (d *Dimension) Normalize() {
	d.Name = strings.TrimSpace(d.Name)
	d.Description = strings.TrimSpace(d.Description)
	d.Tier = strings.TrimSpace(d.Tier)
	d.ScaleType = strings.TrimSpace(d.ScaleType)
	d.Polarity = strings.TrimSpace(d.Polarity)
	d.DefaultTargetOperator = strings.TrimSpace(d.DefaultTargetOperator)
	d.ReturnContract = strings.TrimSpace(d.ReturnContract)
	d.AllowedEngines = normalizeEngines(d.AllowedEngines)

	if d.Tier == "" {
		d.Tier = TierProject
	}
	if d.DefaultWeight == 0 {
		d.DefaultWeight = 1
	}
	if d.IsCodeEngine() {
		if d.ReturnContract == "" {
			d.ReturnContract = ReturnContractBool
		}
	} else {
		d.Code = ""
		d.ReturnContract = ""
	}
	if d.Tier != TierAgentAdhoc {
		d.ApplicationID = nil
	}
}

// ResolveTierUpdate decides the tier an UPDATE writes, from the stored tier
// and the tier the caller sent.
//
// An empty `requested` means the body carried no tier, and the stored tier is
// kept. A client that does not send the scope must not move the dimension.
//
// PROMOTION is allowed: an `agent_adhoc` dimension may become a `project`
// dimension, and its `application_id` is then cleared. The route that calls
// this is gated on PermissionDimensionUpdate, which the project owner holds
// (legacy issue 6669: a private-project owner could not change the tier).
// Every project editor holds it too, and that is deliberate: the agent the
// dimension is scoped to is itself editable by every project editor, so the
// agent scope is not an access boundary to defend here (see Handler.List).
//
// DEMOTION is refused with a 409. A project dimension can already be in use by
// other agents' runs and editors; moving it to one agent would remove it from
// them without a trace.
func ResolveTierUpdate(stored, requested string) (string, error) {
	if requested == "" || requested == stored {
		return stored, nil
	}
	if stored == TierAgentAdhoc && requested == TierProject {
		return TierProject, nil
	}
	if stored == TierProject && requested == TierAgentAdhoc {
		return "", apierr.Conflict("a project dimension cannot be moved back to one agent: other agents can already use it")
	}
	return "", apierr.BadRequest(fmt.Sprintf("the tier of a %s dimension cannot change to %s", stored, requested))
}

// Validate reproduces, on the server, every rule the baseline's editor enforces
// before it will save (DimensionEditorDialog.jsx:106-131).
//
// It is not a duplicate of the client for politeness. The editor is one writer;
// the route is open to any principal holding
// `models.applications.evaluation.dimension.create`, and the columns validated
// here are the input to a score normalisation that cannot re-run this function
// later. The table repeats the same rules as CHECK constraints
// (tenant/0130_eval_dimensions.sql) so a third writer gets the same refusal —
// this layer exists to turn that refusal into a message a person can act on.
//
// Errors are apierr.BadRequest so the handler can hand them straight back.
func (d Dimension) Validate(isCreate bool) error {
	if d.Name == "" {
		return apierr.BadRequest("name is required")
	}
	if len(d.Name) > MaxNameLength {
		return apierr.BadRequest(fmt.Sprintf("name must be at most %d characters", MaxNameLength))
	}
	if len(d.Description) > MaxDimensionDescriptionBytes {
		return apierr.BadRequest(fmt.Sprintf("description must be at most %d bytes", MaxDimensionDescriptionBytes))
	}
	if len(d.Code) > MaxDimensionCodeBytes {
		return apierr.BadRequest(fmt.Sprintf("code must be at most %d bytes", MaxDimensionCodeBytes))
	}
	if !knownTiers[d.Tier] {
		return apierr.BadRequest("tier must be one of project, agent_adhoc, platform")
	}
	// `platform` rows are materialised from a registry this slice does not
	// build. Accepting one here would let a caller mint a row the UI renders
	// as platform-owned and read-only — an authored row wearing someone else's
	// authority.
	if isCreate && d.Tier == TierPlatform {
		return apierr.BadRequest("the platform tier is not authorable: platform dimensions are materialised from the platform catalogue, which this release does not serve")
	}
	if d.Tier == TierAgentAdhoc && d.ApplicationID == nil {
		return apierr.BadRequest("the agent_adhoc tier requires an agent")
	}

	if len(d.AllowedEngines) == 0 {
		return apierr.BadRequest("select at least one engine")
	}
	hasCode := false
	for _, engine := range d.AllowedEngines {
		if !knownEngines[engine] {
			return apierr.BadRequest(fmt.Sprintf("unknown engine %q: allowed_engines must be a subset of ai, human, code", engine))
		}
		if engine == EngineCode {
			hasCode = true
		}
	}
	// THE MUTUAL EXCLUSION. An AI judge grades against `description`; a code
	// validation executes `code`. A dimension claiming both has two different
	// answers for one score and nothing to choose between them.
	if hasCode && !d.IsCodeEngine() {
		return apierr.BadRequest("the code engine is mutually exclusive with ai and human: a code dimension's allowed_engines must be exactly [\"code\"]")
	}
	if d.IsCodeEngine() {
		if strings.TrimSpace(d.Code) == "" {
			return apierr.BadRequest("a code dimension requires validation code")
		}
		if !knownReturns[d.ReturnContract] {
			return apierr.BadRequest("return_contract must be bool or number")
		}
	}

	if !knownScales[d.ScaleType] {
		return apierr.BadRequest("scale_type must be one of binary, ordinal, continuous")
	}
	if !knownPolarities[d.Polarity] {
		// Spelled out rather than defaulted: see the note on the field.
		return apierr.BadRequest("polarity must be higher_better or lower_better — an inverse metric scores backwards without it")
	}
	if !(d.ScaleMin < d.ScaleMax) {
		return apierr.BadRequest("scale_min must be strictly less than scale_max")
	}

	hasTarget := d.DefaultTarget != nil
	hasOperator := d.DefaultTargetOperator != ""
	if hasTarget != hasOperator {
		return apierr.BadRequest("provide both default_target and default_target_operator, or neither")
	}
	if hasOperator && !knownOperators[d.DefaultTargetOperator] {
		return apierr.BadRequest("default_target_operator must be one of >=, >, <=, <, ==")
	}

	return nil
}
