package evaluation_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// validDimension is the smallest body that must be accepted. Every case below
// mutates exactly one thing about it, so a failure names the rule rather than
// the fixture.
func validDimension() evaluation.Dimension {
	return evaluation.Dimension{
		Name:           "Helpfulness",
		Description:    "Does the answer actually help?",
		Tier:           evaluation.TierProject,
		AllowedEngines: []string{evaluation.EngineAI},
		ScaleType:      evaluation.ScaleContinuous,
		ScaleMin:       0,
		ScaleMax:       100,
		Polarity:       evaluation.PolarityHigherBetter,
		DefaultWeight:  1,
	}
}

func normalizedAndValidated(d evaluation.Dimension, isCreate bool) error {
	d.Normalize()
	return d.Validate(isCreate)
}

func requireBadRequest(t *testing.T, err error, because string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal because %s, got nil", because)
	}
	var apiErr *apierr.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an apierr.APIError because %s, got %T: %v", because, err, err)
	}
	if apiErr.Status != 400 {
		t.Fatalf("expected 400 because %s, got %d (%s)", because, apiErr.Status, apiErr.Message)
	}
}

func TestValidDimensionIsAccepted(t *testing.T) {
	t.Parallel()
	if err := normalizedAndValidated(validDimension(), true); err != nil {
		t.Fatalf("the baseline dimension must be accepted, got: %v", err)
	}
}

// THE MUTUAL EXCLUSION. This is the rule the baseline enforces by REPLACING the
// engine set rather than adding to it (DimensionEditorDialog.jsx:92-104), which
// means the browser can never produce a mixed set — and that is exactly why the
// server has to. A non-browser caller holding
// `models.applications.evaluation.dimension.create` can post anything.
//
// An AI judge grades against `description`; a code validation executes `code`.
// A dimension claiming both has two answers for one score with nothing to
// choose between them, and the choice would then be made silently by whichever
// scorer happened to run.
func TestCodeEngineIsMutuallyExclusiveWithAIAndHuman(t *testing.T) {
	t.Parallel()

	mixed := [][]string{
		{evaluation.EngineCode, evaluation.EngineAI},
		{evaluation.EngineAI, evaluation.EngineCode},
		{evaluation.EngineCode, evaluation.EngineHuman},
		{evaluation.EngineAI, evaluation.EngineHuman, evaluation.EngineCode},
		// Normalize lowercases and de-duplicates, so a mixed set cannot be
		// smuggled past the check by casing or repetition.
		{"CODE", "Ai"},
		{evaluation.EngineCode, evaluation.EngineCode, evaluation.EngineHuman},
	}
	for _, engines := range mixed {
		dimension := validDimension()
		dimension.AllowedEngines = engines
		dimension.Code = "def score(x): return True"
		dimension.ReturnContract = evaluation.ReturnContractBool
		requireBadRequest(t, normalizedAndValidated(dimension, true),
			"allowed_engines mixes code with ai/human")
	}

	// The two shapes that ARE legal, so the rule refuses mixing rather than
	// refusing the code engine.
	codeOnly := validDimension()
	codeOnly.AllowedEngines = []string{evaluation.EngineCode}
	codeOnly.Code = "def score(x): return True"
	codeOnly.ReturnContract = evaluation.ReturnContractBool
	if err := normalizedAndValidated(codeOnly, true); err != nil {
		t.Fatalf(`allowed_engines == ["code"] must be accepted, got: %v`, err)
	}

	aiAndHuman := validDimension()
	aiAndHuman.AllowedEngines = []string{evaluation.EngineAI, evaluation.EngineHuman}
	if err := normalizedAndValidated(aiAndHuman, true); err != nil {
		t.Fatalf("ai + human must be accepted together, got: %v", err)
	}
}

func TestCodeEngineRequiresCode(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"", "   ", "\n\t "} {
		dimension := validDimension()
		dimension.AllowedEngines = []string{evaluation.EngineCode}
		dimension.Code = code
		requireBadRequest(t, normalizedAndValidated(dimension, true),
			"a code dimension carries no validation code")
	}
}

// Switching a dimension off the code engine must CLEAR the script rather than
// keep it. A stored script on an AI dimension reads as an executable a sandbox
// would honour, and the author who removed the Code engine believes it is gone.
func TestLeavingTheCodeEngineClearsTheScript(t *testing.T) {
	t.Parallel()

	dimension := validDimension()
	dimension.AllowedEngines = []string{evaluation.EngineAI}
	dimension.Code = "def score(x): return True"
	dimension.ReturnContract = evaluation.ReturnContractNumber
	dimension.Normalize()

	if dimension.Code != "" {
		t.Fatalf("code must be cleared for a non-code dimension, got %q", dimension.Code)
	}
	if dimension.ReturnContract != "" {
		t.Fatalf("return_contract must be cleared for a non-code dimension, got %q", dimension.ReturnContract)
	}
}

func TestPolarityIsRequired(t *testing.T) {
	t.Parallel()

	// Not defaulted, on purpose: polarity is applied LAST in normalisation, so
	// an inverse metric stored without it scores a good answer 0 and nothing
	// reports that.
	for _, polarity := range []string{"", "higher", "HIGHER_BETTER ", "best"} {
		dimension := validDimension()
		dimension.Polarity = polarity
		requireBadRequest(t, normalizedAndValidated(dimension, true),
			"polarity is missing or unknown: "+polarity)
	}
}

func TestScaleBoundsMustBeStrictlyOrdered(t *testing.T) {
	t.Parallel()

	for _, bounds := range [][2]float64{{0, 0}, {5, 5}, {100, 0}, {1, -1}} {
		dimension := validDimension()
		dimension.ScaleMin, dimension.ScaleMax = bounds[0], bounds[1]
		requireBadRequest(t, normalizedAndValidated(dimension, true),
			"scale_min is not strictly less than scale_max")
	}

	// A negative range is fine as long as it is ordered — a latency or a cost
	// dimension is not obliged to start at zero.
	negative := validDimension()
	negative.ScaleMin, negative.ScaleMax = -1, 1
	if err := normalizedAndValidated(negative, true); err != nil {
		t.Fatalf("an ordered negative range must be accepted, got: %v", err)
	}
}

func TestTargetAndOperatorAreRequiredTogether(t *testing.T) {
	t.Parallel()

	target := 80.0

	onlyTarget := validDimension()
	onlyTarget.DefaultTarget = &target
	requireBadRequest(t, normalizedAndValidated(onlyTarget, true),
		"a target was given with no operator")

	onlyOperator := validDimension()
	onlyOperator.DefaultTargetOperator = ">="
	requireBadRequest(t, normalizedAndValidated(onlyOperator, true),
		"an operator was given with no target")

	unknownOperator := validDimension()
	unknownOperator.DefaultTarget = &target
	unknownOperator.DefaultTargetOperator = "=>"
	requireBadRequest(t, normalizedAndValidated(unknownOperator, true),
		"the operator is not one the scorer evaluates")

	both := validDimension()
	both.DefaultTarget = &target
	both.DefaultTargetOperator = ">="
	if err := normalizedAndValidated(both, true); err != nil {
		t.Fatalf("a target with its operator must be accepted, got: %v", err)
	}
	neither := validDimension()
	if err := normalizedAndValidated(neither, true); err != nil {
		t.Fatalf("neither target nor operator must be accepted, got: %v", err)
	}
}

func TestTiersAndTheirAgentScope(t *testing.T) {
	t.Parallel()

	unknown := validDimension()
	unknown.Tier = "team"
	requireBadRequest(t, normalizedAndValidated(unknown, true), "tier is not one of the three")

	// `platform` rows are materialised from a registry this slice does not
	// build. Authoring one would mint a row the UI renders as platform-owned
	// and read-only — an authored row wearing someone else's authority.
	platform := validDimension()
	platform.Tier = evaluation.TierPlatform
	requireBadRequest(t, normalizedAndValidated(platform, true), "the platform tier is not authorable")

	adhocWithoutAgent := validDimension()
	adhocWithoutAgent.Tier = evaluation.TierAgentAdhoc
	requireBadRequest(t, normalizedAndValidated(adhocWithoutAgent, true),
		"the agent_adhoc tier names no agent")

	agentID := 7
	adhoc := validDimension()
	adhoc.Tier = evaluation.TierAgentAdhoc
	adhoc.ApplicationID = &agentID
	if err := normalizedAndValidated(adhoc, true); err != nil {
		t.Fatalf("an agent-scoped dimension must be accepted, got: %v", err)
	}

	// An application id on a PROJECT-tier dimension is dropped rather than
	// stored: it would make a library row look agent-scoped to every later
	// reader while the tier says otherwise.
	projectWithAgent := validDimension()
	projectWithAgent.ApplicationID = &agentID
	projectWithAgent.Normalize()
	if projectWithAgent.ApplicationID != nil {
		t.Fatalf("a project-tier dimension must carry no application id, got %d", *projectWithAgent.ApplicationID)
	}
}

func TestNameIsRequiredAndBounded(t *testing.T) {
	t.Parallel()

	blank := validDimension()
	blank.Name = "   "
	requireBadRequest(t, normalizedAndValidated(blank, true), "the name is blank after trimming")

	long := validDimension()
	long.Name = strings.Repeat("x", evaluation.MaxNameLength+1)
	requireBadRequest(t, normalizedAndValidated(long, true), "the name exceeds the column width")

	atLimit := validDimension()
	atLimit.Name = strings.Repeat("x", evaluation.MaxNameLength)
	if err := normalizedAndValidated(atLimit, true); err != nil {
		t.Fatalf("a name exactly at the limit must be accepted, got: %v", err)
	}
}

func TestUnknownEngineIsRefused(t *testing.T) {
	t.Parallel()

	unknown := validDimension()
	unknown.AllowedEngines = []string{"llm"}
	requireBadRequest(t, normalizedAndValidated(unknown, true), "the engine is not one of the three")

	empty := validDimension()
	empty.AllowedEngines = nil
	requireBadRequest(t, normalizedAndValidated(empty, true), "no engine was selected")
}
