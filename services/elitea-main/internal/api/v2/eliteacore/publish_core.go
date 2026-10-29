package eliteacore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/publicproject"
)

// The publish path, in two halves that two callers share (ADR-0040
// decision 3):
//
//   - publishPrecheck reads and refuses: the version, its name, its
//     category, its model and the validation gate. It writes nothing.
//   - publishTx writes, inside the caller's transaction: the published
//     clone, its tool and skill attachments, the catalogue twin, the
//     embedded sub-agents and the release.
//
// Handler.Publish runs both for a direct publish. A change request runs the
// precheck when it is submitted (PrecheckCandidate) and both halves, minus
// the validation gate, when its final approval publishes the candidate in
// the approval's own transaction (PublishCandidateTx). So the two publish
// paths cannot drift apart.

// PublishGovernor is the agent lifecycle side of a direct publish
// (ADR-0040 decision 3). *repos.AgentLifecycle satisfies it.
type PublishGovernor interface {
	// CheckDirectPublish refuses (change_request_required) when the agent's
	// tier needs an approved change request.
	CheckDirectPublish(ctx context.Context, projectID, applicationID int64) error
	// RecordDirectPublishTx checks the tier again under the registry row's
	// lock and records the self-approved change request of the publish, in
	// the publish transaction.
	RecordDirectPublishTx(ctx context.Context, tx pgx.Tx, publish agentlifecycle.DirectPublish) error
}

// WithPublishGovernor wires agent lifecycle governance into Publish.
// Unwired, every tier publishes directly and no change request is recorded.
func WithPublishGovernor(governor PublishGovernor) Option {
	return func(handler *Handler) {
		if governor == nil {
			return
		}
		if value := reflect.ValueOf(governor); value.Kind() == reflect.Ptr && value.IsNil() {
			return
		}
		handler.governor = governor
	}
}

// writeGovernanceError answers a governance refusal with its typed body,
// and anything else with a 500 that names no cause.
func writeGovernanceError(ctx context.Context, w http.ResponseWriter, err error) {
	if refusal, ok := agentlifecycle.AsRefusal(err); ok {
		writeJSON(w, refusal.Status(), refusal.Body())
		return
	}
	slog.ErrorContext(ctx, "publish: agent lifecycle governance", "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to publish agent"})
}

// publishValidation says how the precheck runs the validation gate.
type publishValidation struct {
	// skip leaves the gate out: a change request's final approval, whose
	// candidate passed it at submit and has not been saved since.
	skip     bool
	token    string
	callerID string
}

func refuse(status int, body map[string]any) *agentlifecycle.PublishRefusal {
	return &agentlifecycle.PublishRefusal{Status: status, Body: body}
}

func versionNameTaken(rule, issue string) *agentlifecycle.PublishRefusal {
	return refuse(http.StatusUnprocessableEntity, map[string]any{
		"error": "validation_failed",
		"validation_result": map[string]any{
			"issues": []map[string]any{
				{"rule": rule, "field": "version_name", "issue": issue, "source": "deterministic"},
			},
		},
	})
}

// publishPrecheck verifies the version and runs the publish checks. It
// answers the application of the version and its author.
func (h *Handler) publishPrecheck(ctx context.Context, s, projectID, versionID, versionName, category string, validation publishValidation) (int, int, *agentlifecycle.PublishRefusal) {
	// Verify version exists and is not 'base'
	//
	// `author_id` rides along for the attribution the published row carries.
	// It is the FALLBACK, not the answer: the person who publishes an agent
	// need not be the person who wrote the draft, so the acting principal wins
	// where there is one (publishActorID below).
	var appID int
	var vName, vStatus, agentType string
	var vAuthorID int
	err := h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT application_id, name, status, COALESCE(agent_type, ''), COALESCE(author_id, 0) FROM %s.application_versions WHERE id = $1`, s), versionID).Scan(&appID, &vName, &vStatus, &agentType, &vAuthorID)
	if err != nil {
		return 0, 0, refuse(http.StatusNotFound, map[string]any{"error": "version not found"})
	}
	if agentType == "pipeline" {
		return 0, 0, refuse(http.StatusBadRequest, map[string]any{"error": "pipeline_not_publishable", "msg": "pipeline agents cannot be published"})
	}
	if vStatus == "published" {
		return 0, 0, refuse(http.StatusConflict, map[string]any{"error": "version is already published"})
	}

	// Pre-check: does a version with this name already exist for this application?
	var nameExists bool
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM %s.application_versions WHERE application_id = $1 AND name = $2)`, s),
		appID, versionName).Scan(&nameExists) // failure leaves nameExists=false, safe to continue
	if nameExists {
		return 0, 0, versionNameTaken("version_name_exists_in_source", "version name already exists")
	}

	// Validate category if provided
	validCategories := map[string]bool{
		"Business Analyst": true, "Quality Assurance": true, "Development": true,
		"DevOps": true, "Project Management": true, "Knowledge & Documentation": true,
		"Elitea": true, "Epam": true, "Other": true,
	}
	if category != "" && !validCategories[category] {
		return 0, 0, refuse(http.StatusUnprocessableEntity, map[string]any{
			"error": "validation_failed",
			"validation_result": map[string]any{
				"issues": []map[string]any{
					{"rule": "invalid_category", "field": "category", "issue": "unknown category", "source": "deterministic"},
				},
			},
		})
	}

	// Hard-check: the model must come from the public project (runs before
	// validation).
	//
	// ONE project, resolved once. This used to accept `PUBLIC_PROJECT_ID`
	// (default 1) OR `SHARED_PROJECT_ID` (default 4), which made the guard
	// admit a second project no other surface knew about and that the
	// reference does not have: legacy `_check_shared_llm` compares against a
	// single `public_project_id`. Both names still resolve, as deprecated
	// aliases of ELITEA_AI_PROJECT_ID — see internal/publicproject.
	publicProjID := publicproject.IDString()
	var llmSettingsStr *string
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT llm_settings::text FROM %s.application_versions WHERE id = $1`, s), versionID).Scan(&llmSettingsStr) // failure leaves nil, safe
	if llmSettingsStr != nil {
		var llmSettings map[string]any
		_ = json.Unmarshal([]byte(*llmSettingsStr), &llmSettings) // DB jsonb column; malformed means empty map
		if modelProjID, ok := llmSettings["model_project_id"]; ok && modelProjID != nil {
			mpid := fmt.Sprintf("%v", modelProjID)
			if mpid != publicProjID {
				return 0, 0, refuse(http.StatusBadRequest, map[string]any{"error": "llm_not_shared"})
			}
			// The model IS the catalogue's — and the catalogue's own models are
			// no longer one set. A platform model carries a GRANT, and a
			// published agent is offered to every project that reads the
			// catalogue, so it may only name a model granted to all of them.
			// Same code, because it is the same finding; the `msg` names the
			// scope that was found. See publish_model_grant.go.
			if refusal := h.publishGrantRefusal(ctx, llmSettings); refusal != "" {
				return 0, 0, refuse(http.StatusBadRequest, map[string]any{
					"error": "llm_not_shared", "msg": refusal,
				})
			}
		}
	}

	if validation.skip {
		return appID, vAuthorID, nil
	}
	// Validation gate: if no token provided, run inline validation
	if validation.token == "" {
		valResult, _ := h.runPublishValidation(ctx, s, projectID, validation.callerID, versionID, versionName)
		if valResult != nil && valResult["status"] == "FAIL" {
			criticals, _ := valResult["critical_issues"].([]map[string]any)
			issues := make([]map[string]any, len(criticals))
			for i, c := range criticals {
				issues[i] = map[string]any{"rule": c["field"], "message": c["issue"]}
				if r, ok := c["rule"]; ok {
					issues[i]["rule"] = r
				}
			}
			valResult["issues"] = issues
			return 0, 0, refuse(http.StatusUnprocessableEntity, map[string]any{
				"error":             "validation_failed",
				"validation_result": valResult,
			})
		}
	} else {
		// The token is VERIFIED, not merely shaped.
		//
		// This used to accept any string of sixteen or more lowercase
		// hexadecimal characters, which made the quality gate optional for
		// anyone willing to type one: the token was never recorded, never
		// bound to a version and never looked up (issue 855). It is now a
		// signed grant that names the version it was issued for and carries
		// its own expiry — see publish_validation_token.go.
		if verdict := verifyPublishValidationToken(validation.token, versionID,
			h.publishTokenContentHash(ctx, s, versionID), time.Now()); verdict != publishTokenValid {
			return 0, 0, refuse(http.StatusBadRequest, map[string]any{
				"error": "validation_token_invalid",
				"msg":   publishTokenRefusal(verdict),
			})
		}
	}
	return appID, vAuthorID, nil
}

// publishWrite is what publishTx writes.
type publishWrite struct {
	schema, projectID string
	appID             int
	versionID         string
	versionName       string
	category          string
	// publishedBy is the meta.published_by of the published row; absent
	// when hasPublisher is false.
	publishedBy    int64
	hasPublisher   bool
	releaseActorID int64
}

// publishOutcome is what publishTx wrote.
type publishOutcome struct {
	cloneID   int
	twin      catalogTwinTargets
	releaseID int64
}

// publishTx clones the version into a published row and records the
// release, in the caller's transaction. A refusal leaves the transaction
// for the caller to roll back.
func (h *Handler) publishTx(ctx context.Context, tx pgx.Tx, p publishWrite) (publishOutcome, *agentlifecycle.PublishRefusal) {
	s := p.schema
	// Clone: insert a new version with status 'published' using same application_id
	//
	// The overlay is MARSHALLED rather than formatted. It used to be built with
	// fmt.Sprintf, which put a caller-supplied category straight inside a JSON
	// string literal; the category is whitelisted above, so nothing could reach
	// it, but a second key added later would not have been.
	//
	// `published_by` is the key the operator's published-agents dashboard reads
	// (admin_published_agents.go). The SKILL publish path has always written it
	// (internal/api/v2/skillpublish/publish.go); this path wrote only
	// `source_version_id` and `category`, so the dashboard's "published by"
	// column was null for every agent on every deployment, permanently, and an
	// operator could not tell an unattributed publish from an unmeasured one.
	// It rides into the catalogue twin as well, because the twin copies this
	// row's meta (catalog_mirror.go).
	var cloneID int
	overlay := map[string]any{"source_version_id": p.versionID}
	if p.hasPublisher {
		overlay["published_by"] = p.publishedBy
	}
	if p.category != "" {
		overlay["category"] = p.category
	}
	metaOverlayJSON, overlayErr := json.Marshal(overlay)
	if overlayErr != nil {
		return publishOutcome{}, refuse(http.StatusInternalServerError, map[string]any{"error": "failed to publish agent"})
	}
	cloneQ := fmt.Sprintf(`
		INSERT INTO %s.application_versions
			(application_id, name, status, author_id, llm_settings, instructions,
			 conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
		SELECT application_id, $2, 'published', author_id, llm_settings, instructions,
			   conversation_starters, welcome_message, agent_type,
			   COALESCE(meta, '{}'::jsonb) || $3::jsonb,
			   pipeline_settings,
			   now() AT TIME ZONE 'UTC'
		FROM %s.application_versions WHERE id = $1
		RETURNING id`, s, s)

	err := tx.QueryRow(ctx, cloneQ, p.versionID, p.versionName, string(metaOverlayJSON)).Scan(&cloneID)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "_application_version_name_uc") {
			return publishOutcome{}, versionNameTaken("version_name_exists_in_source", "version name already exists")
		}
		slog.ErrorContext(ctx, "publish: clone the version", "schema", s, "version_id", p.versionID, "error", err)
		return publishOutcome{}, refuse(http.StatusInternalServerError, map[string]any{"error": "failed to publish agent"})
	}

	// Clone entity_tool_mapping rows from source version to new published version.
	// The copy keeps the source `entity_type` and `entity_id`: the clone above
	// selects the source `application_id`, so the published version belongs to the
	// same agent, and the chat read joins `entity_id` to that application
	// (internal/db/queries/agent_chat.sql:107).
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools, created_at)
		SELECT $2, entity_id, entity_type, tool_id, selected_tools, now() AT TIME ZONE 'UTC'
		FROM %s.entity_tool_mapping WHERE entity_version_id = $1`, s, s), p.versionID, cloneID); err != nil {
		return publishOutcome{}, refuse(http.StatusInternalServerError, map[string]any{"error": "failed to publish agent tool attachments"})
	}

	// Clone entity_skill_mapping rows from source version to new published version.
	// Publish copied tool mappings only, so a published agent carried no skills
	// (#351). Fork sets the precedent it now matches
	// (internal/api/oapiserver/publishing.go:149).
	//
	// The table has no `entity_id` column (001_initial.sql:422-432): a skill
	// attachment is keyed by (entity_version_id, skill_id, entity_type) alone.
	// `entity_type` is carried from the source row rather than defaulted, because
	// it is part of that key and the chat read matches on it
	// (internal/db/queries/agent_chat.sql:132). `skill_version_id` rides along
	// because the same read LEFT JOINs it for the skill instructions — dropping it
	// publishes a named skill with an empty body.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id, created_at)
		SELECT $2, entity_type, skill_id, skill_version_id, now() AT TIME ZONE 'UTC'
		FROM %s.entity_skill_mapping WHERE entity_version_id = $1`, s, s), p.versionID, cloneID); err != nil {
		return publishOutcome{}, refuse(http.StatusInternalServerError, map[string]any{"error": "failed to publish agent skill attachments"})
	}

	// The catalogue twin, in the SAME transaction as the clone.
	//
	// Without it a publish from any project other than the public one wrote a
	// `published` row into a schema ELITEA Catalog never reads: the Published
	// tab listed the agent and the catalogue stayed empty, with no error on
	// either side. See catalog_mirror.go for why the twin carries no tool or
	// skill attachments.
	twin, mirrorErr := mirrorPublishedVersion(ctx, tx, s, p.projectID, p.appID, cloneID, p.versionName, p.category)
	if mirrorErr != nil {
		if errors.Is(mirrorErr, errCatalogVersionNameTaken) {
			return publishOutcome{}, versionNameTaken("version_name_exists_in_catalog", "version name already published to the catalog")
		}
		slog.ErrorContext(ctx, "publish: catalog mirror failed", "schema", s, "version_id", cloneID, "error", mirrorErr)
		return publishOutcome{}, refuse(http.StatusInternalServerError, map[string]any{"error": "failed to publish agent to the catalog"})
	}

	// Embed sub-agents: clone application_tools of type 'application'
	// recursively, in the publish transaction. The release below pins the
	// embedded copies, so they commit with it or not at all.
	if err := h.embedSubAgents(ctx, tx, s, p.projectID, p.versionID, cloneID); err != nil {
		slog.ErrorContext(ctx, "publish: embed the sub-agents", "schema", s, "version_id", cloneID, "error", err)
		return publishOutcome{}, refuse(http.StatusInternalServerError, map[string]any{"error": "failed to publish agent sub-agents"})
	}

	// The release of this publish (ADR-0040 decision 2), recorded last in
	// the same transaction, so its digest is the definition a run reads and
	// a publish never commits without its release: a failure here answers
	// 500 and leaves the agent as it was, and a retry publishes again.
	releaseID, err := h.recordPublishedRelease(ctx, tx, p.releaseActorID, p.projectID, p.appID, cloneID, p.versionID, p.versionName)
	if err != nil {
		slog.ErrorContext(ctx, "publish: record the agent release",
			"project_id", p.projectID, "application_id", p.appID, "version_id", cloneID, "error", err)
		return publishOutcome{}, refuse(http.StatusInternalServerError, map[string]any{"error": "failed to record the agent release"})
	}
	return publishOutcome{cloneID: cloneID, twin: twin, releaseID: releaseID}, nil
}

// publishResponse is the publish answer.
//
// `public_agent_id` and `public_version_id` name the rows in the AUTHOR's
// schema, which is what they have always named and what
// publish_tool_copy_postgres_integration_test.go and
// publish_skill_copy_postgres_integration_test.go read them for. The
// catalogue rows are a different pair, so they get their own two keys
// rather than a changed meaning of these.
//
// The catalogue keys are OMITTED when there is no twin — that is, when the
// author already stands in the public project and the clone above IS the
// catalogue row. A zero would read as "the catalogue has row 0".
// `source_version_id` names the DRAFT this publish was made from, and it
// agrees with the key of the same name in the published row's own stored
// meta (the overlay above). It used to repeat `public_version_id` — the id
// of the row this request had just created — so a client following the
// response back to the version the author keeps editing arrived at the
// published copy instead, and the two answers to "where did this come
// from?" disagreed inside one response.
func publishResponse(appID int, versionID, versionName string, outcome publishOutcome) map[string]any {
	response := map[string]any{
		"public_agent_id":   strconv.Itoa(appID),
		"public_version_id": strconv.Itoa(outcome.cloneID),
		"version_name":      versionName,
		"source_version_id": versionID,
	}
	if outcome.twin.VersionID != 0 {
		response["catalog_agent_id"] = strconv.Itoa(outcome.twin.ApplicationID)
		response["catalog_version_id"] = strconv.Itoa(outcome.twin.VersionID)
	}
	// Omitted only when the deployment records no releases.
	if outcome.releaseID > 0 {
		response["release_id"] = strconv.FormatInt(outcome.releaseID, 10)
	}
	return response
}

// announcePublished emits agent.version.published after the commit.
func (h *Handler) announcePublished(ctx context.Context, projectID string, appID, cloneID int, versionID, versionName string) {
	if h.events == nil {
		return
	}
	h.events.Emit(ctx, projectID, "agent.version.published", map[string]any{
		"application_id":     appID,
		"published_agent_id": cloneID,
		"source_version_id":  versionID,
		"version_name":       versionName,
	})
}

// candidateSchema resolves the publish's tenant schema and refuses a
// deployment that blocks publishing, as Publish does first.
func (h *Handler) candidateSchema(ctx context.Context, candidate agentlifecycle.PublishCandidate) (string, string, error) {
	projectID := strconv.FormatInt(candidate.ProjectID, 10)
	if guard := h.publishGuardrail(ctx); !guard.allows(candidate.ProjectID) {
		return "", "", refuse(http.StatusForbidden, map[string]any{"error": "publishing is blocked on this deployment"})
	}
	s, err := tenantschema.QuoteInt(candidate.ProjectID)
	if err != nil {
		return "", "", refuse(http.StatusBadRequest, map[string]any{"error": "invalid project id"})
	}
	return s, projectID, nil
}

// PrecheckCandidate runs every publish check on a change request's
// candidate, the validation gate included, when the request is submitted.
// It answers a *agentlifecycle.PublishRefusal for a check that fails.
func (h *Handler) PrecheckCandidate(ctx context.Context, candidate agentlifecycle.PublishCandidate) error {
	s, projectID, err := h.candidateSchema(ctx, candidate)
	if err != nil {
		return err
	}
	appID, _, refusal := h.publishPrecheck(ctx, s, projectID, strconv.FormatInt(candidate.VersionID, 10),
		candidate.VersionName, candidate.Category,
		publishValidation{callerID: strconv.FormatInt(candidate.PublisherID, 10)})
	if refusal != nil {
		return refusal
	}
	if int64(appID) != candidate.ApplicationID {
		return refuse(http.StatusNotFound, map[string]any{"error": "version not found"})
	}
	return nil
}

// PublishCandidateTx publishes a change request's candidate in the final
// approval's transaction: the publish checks again (minus the validation
// gate, which the unchanged candidate passed at submit), then the same
// writes as Publish. The release is required: an approved change request
// always names the release it created.
func (h *Handler) PublishCandidateTx(ctx context.Context, tx pgx.Tx, candidate agentlifecycle.PublishCandidate) (agentlifecycle.PublishedCandidate, error) {
	if h.releases == nil {
		return agentlifecycle.PublishedCandidate{}, errors.New("publish: the deployment records no releases")
	}
	s, projectID, err := h.candidateSchema(ctx, candidate)
	if err != nil {
		return agentlifecycle.PublishedCandidate{}, err
	}
	versionID := strconv.FormatInt(candidate.VersionID, 10)
	appID, _, refusal := h.publishPrecheck(ctx, s, projectID, versionID, candidate.VersionName, candidate.Category,
		publishValidation{skip: true})
	if refusal != nil {
		return agentlifecycle.PublishedCandidate{}, refusal
	}
	if int64(appID) != candidate.ApplicationID {
		return agentlifecycle.PublishedCandidate{}, refuse(http.StatusNotFound, map[string]any{"error": "version not found"})
	}
	outcome, refusal := h.publishTx(ctx, tx, publishWrite{
		schema: s, projectID: projectID, appID: appID, versionID: versionID,
		versionName: candidate.VersionName, category: candidate.Category,
		publishedBy: candidate.PublisherID, hasPublisher: candidate.PublisherID > 0,
		releaseActorID: candidate.PublisherID,
	})
	if refusal != nil {
		return agentlifecycle.PublishedCandidate{}, refusal
	}
	if outcome.releaseID <= 0 {
		return agentlifecycle.PublishedCandidate{}, errors.New("publish: no release recorded")
	}
	return agentlifecycle.PublishedCandidate{
		ApplicationID: int64(appID), PublishedVersionID: int64(outcome.cloneID),
		SourceVersionID: candidate.VersionID, ReleaseID: outcome.releaseID,
		VersionName: candidate.VersionName,
	}, nil
}

// AnnouncePublished emits agent.version.published for a candidate a change
// request published, after the approval committed.
func (h *Handler) AnnouncePublished(ctx context.Context, projectID int64, published agentlifecycle.PublishedCandidate) {
	h.announcePublished(ctx, strconv.FormatInt(projectID, 10), int(published.ApplicationID),
		int(published.PublishedVersionID), strconv.FormatInt(published.SourceVersionID, 10), published.VersionName)
}
