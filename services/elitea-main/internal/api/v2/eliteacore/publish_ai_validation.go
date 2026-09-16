package eliteacore

// The AI step of publish validation (issue #940 A18, ELITEA-0146).
//
// # What was missing
//
// `runPublishValidation` hard-coded `ai_validation_available: false` and every
// one of its checks was deterministic — a name collision, a cycle, a depth
// bound, a model-sharing rule. The case asks for the other half: an advisory
// LLM pass over the version's own text, run on a PROJECT-LEVEL LOW-TIER model,
// so that an agent whose OWN model is broken or unavailable still gets
// validated. That is the whole point of the case's title: the agent's model is
// what publishing is about, and it must not be what the pre-publish check
// depends on.
//
// # Three rules, each of them the difference between this and a plausible
// version of it
//
//  1. THE AI STEP NEVER RAISES A CRITICAL ISSUE, and never a warning. Its
//     findings land in `recommendations`, so it cannot change `status` and
//     cannot turn a publishable version into a refused one. A model that is
//     having a bad day must not become a publishing gate — and the same
//     reasoning says it must not be able to flip PASS to WARN either, since
//     the SPA renders WARN as a confirmation step the author has to clear.
//  2. EVERY FAILURE IS "UNAVAILABLE", NOT AN ERROR. No low-tier model
//     configured, the gateway unreachable, a refusal, a timeout, an answer
//     that is not the JSON shape asked for — all of them answer
//     `ai_validation_available: false` and leave the deterministic result
//     exactly as it was. ELITEA-0146 is precisely the case where the model
//     side is broken, and a 500 there would be the failure the case exists to
//     forbid.
//  3. THE MODEL IS RESOLVED FROM THE PROJECT, not from the version. The
//     version's `llm_settings.model_name` is the model under validation; using
//     it would make the check depend on the thing it is checking.
//
// # Why the client interface is local
//
// `v2predict.Completer` is the production hop, and router.go adapts it onto
// {@link PublishModelClient} in one closure. The interface is declared HERE,
// in the consumer, so this package depends on the four fields it uses rather
// than on the predict package's request struct — and so a test's fake is four
// lines instead of a gateway.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// publishCallerID is the authenticated principal, or "" when the request
// carries none. The AI turn is billed and authorised against them; an empty id
// lets the gateway refuse rather than this service inventing one.
func publishCallerID(r *http.Request) string {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return ""
	}
	return user.ID
}

// PublishModelClient is the one LLM call this step makes.
type PublishModelClient interface {
	Complete(ctx context.Context, request PublishModelRequest) (string, error)
}

// PublishModelRequest is one blocking completion.
type PublishModelRequest struct {
	// ProjectID and UserID are signed into the gateway's identity headers, so
	// the turn is billed and authorised against the caller who asked for the
	// validation — not against the agent being validated.
	ProjectID string
	UserID    string
	// Model is the PROJECT's low-tier model, never the version's own.
	Model  string
	System string
	User   string
}

// WithPublishAIValidation activates the AI step. Unset, `runPublishValidation`
// answers `ai_validation_available: false` exactly as it did before #940 A18 —
// which is the honest answer for a deployment with no gateway.
func WithPublishAIValidation(client PublishModelClient) Option {
	return func(h *Handler) {
		if client == nil {
			return
		}
		h.publishModelClient = client
	}
}

// publishAIFinding is one advisory remark.
type publishAIFinding struct {
	Field      string `json:"field"`
	Issue      string `json:"issue"`
	Suggestion string `json:"suggestion,omitempty"`
}

// publishAIAnswer is the JSON object the model is asked for. A model that
// answers prose instead produces a decode failure, which reads as
// "unavailable" — never as "no findings", which would be a claim this step did
// not make.
type publishAIAnswer struct {
	Findings []publishAIFinding `json:"findings"`
}

const publishAISystemPrompt = `You review AI agent definitions before they are published to a shared catalogue.
You are given the agent's name, description and instructions.
Reply with ONLY a JSON object of the form {"findings":[{"field":"...","issue":"...","suggestion":"..."}]}.
Each finding must name one concrete, actionable problem with the text: a missing description, instructions that
contradict the description, a name that does not say what the agent does, leaked credentials or internal URLs,
or wording that would confuse a reader outside the authoring team.
Report at most five findings. If the definition reads well, reply with {"findings":[]}.
Do not refuse, do not explain, and do not write anything outside the JSON object.`

// maxPublishAIFindings bounds what one model answer can add to a response the
// SPA renders as a list.
const maxPublishAIFindings = 5

// lowTierModelQuery resolves the project's low-tier model.
//
// It reads the same rows the model catalogue reads — `section = 'llm'`,
// `status_ok`, the `low_tier` flag in `data` — which is how
// `internal/application/configurations`'s own tier resolution decides what is
// low-tier (models.go's populateCurrentLLMTierDefaults). Shared rows first, so
// a project that carries only the globally shared catalogue still resolves
// one; then by id, so the answer is stable between calls.
const lowTierModelQuery = `
SELECT COALESCE(data ->> 'name', '')::text
FROM %s.configuration
WHERE section = 'llm'
  AND status_ok = true
  AND COALESCE((data ->> 'low_tier')::boolean, false) = true
  AND COALESCE(data ->> 'name', '') <> ''
ORDER BY shared DESC, id ASC
LIMIT 1`

// resolveLowTierModel answers the project's low-tier model name, or "" when
// the project has none. A query failure is "" as well: this step is advisory,
// and a broken read of the catalogue is a reason to skip it, not to fail the
// validation the caller actually asked for.
func (h *Handler) resolveLowTierModel(ctx context.Context, schema string) string {
	if h.pool == nil {
		return ""
	}
	var name string
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(lowTierModelQuery, schema)).Scan(&name); err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// publishAIValidationInput is the version text the model reviews.
type publishAIValidationInput struct {
	VersionID    string
	Name         string
	Description  string
	Instructions string
}

// readPublishAIValidationInput collects the version's own text.
//
// The description lives on the APPLICATION and the instructions on the
// VERSION, which is why this is one query over the join rather than a read of
// either table.
func (h *Handler) readPublishAIValidationInput(
	ctx context.Context, schema, versionID string,
) (publishAIValidationInput, bool) {
	if h.pool == nil {
		return publishAIValidationInput{}, false
	}
	input := publishAIValidationInput{VersionID: versionID}
	query := fmt.Sprintf(`
		SELECT COALESCE(app.name, ''), COALESCE(app.description, ''), COALESCE(version.instructions, '')
		FROM %s.application_versions AS version
		JOIN %s.applications AS app ON app.id = version.application_id
		WHERE version.id = $1`, schema, schema)
	if err := h.pool.QueryRow(ctx, query, versionID).
		Scan(&input.Name, &input.Description, &input.Instructions); err != nil {
		return publishAIValidationInput{}, false
	}
	return input, true
}

// maxPublishAIPromptRunes bounds what is sent. Instructions are free text and
// can be tens of thousands of characters; a low-tier model's context is the
// smallest on the platform, so the tail is dropped rather than the call
// failing.
const maxPublishAIPromptRunes = 6000

func publishAIUserPrompt(input publishAIValidationInput) string {
	prompt := fmt.Sprintf("Name: %s\n\nDescription: %s\n\nInstructions:\n%s",
		input.Name, input.Description, input.Instructions)
	runes := []rune(prompt)
	if len(runes) > maxPublishAIPromptRunes {
		return string(runes[:maxPublishAIPromptRunes])
	}
	return prompt
}

// runPublishAIValidation performs the advisory pass.
//
// It answers (findings, available). `available` false means the step did not
// run — no client, no low-tier model, no readable version text, or a model
// answer that could not be used — and the caller reports
// `ai_validation_available: false` with the deterministic result untouched.
func (h *Handler) runPublishAIValidation(
	ctx context.Context, schema, projectID, userID, versionID string,
) ([]map[string]any, bool) {
	if h.publishModelClient == nil {
		return nil, false
	}
	model := h.resolveLowTierModel(ctx, schema)
	if model == "" {
		return nil, false
	}
	input, ok := h.readPublishAIValidationInput(ctx, schema, versionID)
	if !ok {
		return nil, false
	}

	content, err := h.publishModelClient.Complete(ctx, PublishModelRequest{
		ProjectID: projectID,
		UserID:    userID,
		Model:     model,
		System:    publishAISystemPrompt,
		User:      publishAIUserPrompt(input),
	})
	if err != nil {
		// The case's own scenario. Logged, never surfaced: the author asked
		// whether their agent can be published, and "the review model is
		// down" is not an answer to that question.
		slog.InfoContext(ctx, "publish validation: AI step unavailable",
			"error", err, "version_id", versionID, "model", model)
		return nil, false
	}

	findings, ok := decodePublishAIFindings(content)
	if !ok {
		slog.InfoContext(ctx, "publish validation: AI step answered an unusable shape",
			"version_id", versionID, "model", model)
		return nil, false
	}
	return findings, true
}

// decodePublishAIFindings reads the model's answer.
//
// It tolerates a fenced code block around the object, which every model emits
// sooner or later, and NOTHING else: an answer that is not the asked-for shape
// reports false so the caller says "unavailable" rather than "no findings".
// Those two are different claims and the author acts on them differently.
func decodePublishAIFindings(content string) ([]map[string]any, bool) {
	trimmed := strings.TrimSpace(content)
	if fenced := extractFencedJSON(trimmed); fenced != "" {
		trimmed = fenced
	}
	if trimmed == "" {
		return nil, false
	}
	var answer publishAIAnswer
	if err := json.Unmarshal([]byte(trimmed), &answer); err != nil {
		return nil, false
	}
	findings := make([]map[string]any, 0, len(answer.Findings))
	for _, finding := range answer.Findings {
		issue := strings.TrimSpace(finding.Issue)
		if issue == "" {
			continue
		}
		entry := map[string]any{
			"field": strings.TrimSpace(finding.Field),
			"issue": issue,
			// `source: "ai"` beside the deterministic rules' own
			// `source: "deterministic"`, so a reader can tell which half of
			// the validation produced a remark — and so a client can render
			// an advisory one differently from a rule.
			"source": "ai",
		}
		if suggestion := strings.TrimSpace(finding.Suggestion); suggestion != "" {
			entry["suggestion"] = suggestion
		}
		findings = append(findings, entry)
		if len(findings) >= maxPublishAIFindings {
			break
		}
	}
	return findings, true
}

// extractFencedJSON returns the body of a ```…``` block, or "" when there is
// none.
func extractFencedJSON(content string) string {
	start := strings.Index(content, "```")
	if start < 0 {
		return ""
	}
	rest := content[start+3:]
	if newline := strings.IndexByte(rest, '\n'); newline >= 0 {
		// Drop an optional language tag on the opening fence line.
		if !strings.Contains(strings.TrimSpace(rest[:newline]), "{") {
			rest = rest[newline+1:]
		}
	}
	end := strings.Index(rest, "```")
	if end < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}
