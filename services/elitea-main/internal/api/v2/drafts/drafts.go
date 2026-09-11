// Package drafts serves the three AI-draft routes of #254's P1 batch:
//
//	POST /api/v2/elitea_core/generate_application_draft/prompt_lib/{projectID}
//	POST /api/v2/elitea_core/generate_project_context_draft/prompt_lib/{projectID}
//	POST /api/v2/elitea_core/generate_skill_draft/prompt_lib/{projectID}
//
// Each one turns a plain-text description into a STRUCTURED draft the review
// form in the SPA edits and submits. They are the last three routes named by
// router.go's NOTE(#126) tombstone: they stood behind a nil
// RouterConfig.Predictor gate nothing ever assigned, answered 404 in every
// deployment, and #126 deleted them with the prototype indexer transport.
//
// WHY ONE PACKAGE. The three share every mechanic — one blocking LLM turn, one
// JSON-object answer extracted from free model text, one validated draft in the
// response — and differ only in the system prompt and the shape they validate
// into. internal/api/v2/skills held one of the three (DraftHandler) with no
// caller since #126; keeping the trio together is what makes the shared
// extraction and the shared status contract a single reviewed thing rather than
// three drifting copies.
//
// THE LLM HOP. The completion is performed by the same v2predict.Completer that
// serves predict_llm — the mTLS hop to services/elitea-llm-gateway, which owns
// credential resolution and the egress allowlist. There is no second client
// here and no direct provider dial.
//
// BLOCKING ONLY, like predict_llm. Legacy called predict_sio_llm with
// await_task_timeout=60, i.e. its blocking mode, and read the answer out of the
// returned task result. There is no async half here and none is needed: legacy
// did not use one for these routes either.
//
// WHAT IS DELIBERATELY NOT PORTED, each with its reason:
//
//   - THE SERVICE-PROMPT STORE. Legacy reads its system prompts from the
//     configurations store by key ("generate_application_draft",
//     "skill_generator", "project_context_generator") and answers 500 when the
//     key is unset. The prompts below are in the source instead. A deployment
//     that has never seeded a service prompt still generates drafts, and the
//     prompt that produced a given response is readable in review — where a
//     store row is not. The keys stay valid configuration
//     (internal/application/configurations); nothing here writes or reads them.
//
//   - SKILL EDIT MODE. Main reads the requested project, skill, and version.
//     The model returns a draft without changing the stored skill.
//     Application edit mode remains unavailable. Project context edit mode
//     uses the prior content supplied by the caller.
//
//   - RESOURCE SUGGESTIONS ARE NOW REAL (issue #881; this note used to record
//     why they weren't — kept as a correction, not deleted). Legacy's
//     application draft returns suggested_toolkits/suggested_mcp/
//     suggested_agents/suggested_pipelines/suggested_skills, built by first
//     reading the project's toolkit instances, agents, pipelines and skills
//     and offering them to the model as candidates. The claim that
//     RouterConfig composed no reader for toolkit INSTANCES was true of
//     AppsRepo/SkillsRepo's siblings but not of the toolkits package itself:
//     v2toolkits.Repository.ListToolkits already existed, just not exposed
//     outside that package — v2toolkits.NewPostgresRepository (added
//     alongside this) is the minimal crack that exposes it. This port also
//     does the candidate MATCHING in Go rather than the model (see
//     suggestions.go's own doc comment for why). AppsRepo/SkillsRepo/the new
//     toolkits reader are threaded in as WithAppsRepo/WithSkillsRepo/
//     WithToolkitsRepo options, all optional: an omitted one degrades to an
//     empty list for that category, never a broken response.
package drafts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	v2predict "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// maxDraftRequestBytes bounds the body. user_description and
// current_project_background are free text, so the bound is generous without
// being absent.
const maxDraftRequestBytes = 1 << 20 // 1 MiB

// NotConfiguredCode is the machine-readable code these routes answer with while
// LLM_GATEWAY_URL is empty. It is v2predict.NotConfiguredCode's value on
// purpose: the missing configuration is the same one, so an operator matching
// on the code does not have to learn a second spelling.
const NotConfiguredCode = v2predict.NotConfiguredCode

// The caps legacy's pydantic models enforce, kept as the single place they are
// written down. Every one of them TRUNCATES rather than rejects (except the
// skill name, which has a shape and not only a length): a draft that is
// slightly over is still a usable starting point for the review form, and
// refusing it costs the user a whole generation. A field that is MISSING is a
// genuine generation failure and is refused with 422.
const (
	skillNameMaxLength         = 64
	skillDescriptionMaxLength  = 2304
	skillInstructionsMaxLength = 5000

	// PROJECT_CONTEXT_MAX_LEN in legacy/plugins/elitea_core/models/pd/project_context.py.
	projectBackgroundMaxLength = 2500

	applicationNameMaxLength            = 32
	applicationDescriptionMaxLength     = 2304
	applicationWelcomeMessageMaxLength  = 768
	applicationStarterMaxLength         = 768
	applicationMaxConversationStarters  = 4
	applicationInstructionsSanityLength = 32768
)

// AppsReader is the one applications.Repository method GenerateApplicationDraft
// needs (suggested_agents/suggested_pipelines — one repository, filtered by
// AgentsType, see suggestApplications). Narrowed deliberately: the real
// applications.Repository satisfies this structurally with zero glue code,
// and a test fake needs to implement only this one method instead of every
// unrelated CRUD/version operation on the full interface.
type AppsReader interface {
	List(ctx context.Context, req applications.ListRequest) (applications.ListResponse, error)
}

// SkillsReader is the one v2skills.Repository method GenerateApplicationDraft
// needs (suggested_skills) — same narrowing reason as AppsReader.
type SkillsReader interface {
	List(ctx context.Context, projectID string, params v2skills.ListParams) (v2skills.ListResponse, error)
}

// SkillVersionReader reads one version within its owning project and skill.
type SkillVersionReader interface {
	GetVersion(context.Context, string, string, string) (v2skills.Skill, error)
}

// WithSkillVersions enables skill edit drafts.
func WithSkillVersions(repo SkillVersionReader) Option {
	return func(h *Handler) { h.skillVersions = repo }
}

// ToolkitsReader is the one v2toolkits.Repository method GenerateApplicationDraft
// needs (suggested_toolkits/suggested_mcp, split by elitea_tools.type — see
// suggestToolkits) — same narrowing reason as AppsReader.
type ToolkitsReader interface {
	ListToolkits(ctx context.Context, projectID string, page, pageSize int) ([]map[string]any, int, error)
}

// Handler serves the three draft routes.
type Handler struct {
	completer     v2predict.Completer
	skillVersions SkillVersionReader
	permissions   auth.PermissionResolver
	// apps/skills/toolkits back GenerateApplicationDraft's five suggested_*
	// lists (issue #881). All three are optional (nil-safe — see
	// suggestions.go): a deployment that composes none of them still serves
	// every draft route, just with empty suggestion lists.
	apps     AppsReader
	skills   SkillsReader
	toolkits ToolkitsReader
}

// Option configures a Handler at construction — same pattern
// internal/api/v2/secrets and internal/api/v2/toolkits already use for an
// optional dependency.
type Option func(*Handler)

// WithAppsRepo supplies the reader GenerateApplicationDraft uses for
// suggested_agents/suggested_pipelines.
func WithAppsRepo(repo AppsReader) Option {
	return func(h *Handler) { h.apps = repo }
}

// WithSkillsRepo supplies the reader for suggested_skills.
func WithSkillsRepo(repo SkillsReader) Option {
	return func(h *Handler) { h.skills = repo }
}

// WithToolkitsRepo supplies the reader for suggested_toolkits/suggested_mcp.
func WithToolkitsRepo(repo ToolkitsReader) Option {
	return func(h *Handler) { h.toolkits = repo }
}

// NewHandler builds the handler. completer may be nil — that is the
// "LLM_GATEWAY_URL is unset" deployment, and every route then answers 503
// naming the variable. It is NOT a reason to leave the routes unregistered:
// #126 is the record of what an invisible 404 costs.
func NewHandler(completer v2predict.Completer, opts ...Option) *Handler {
	h := &Handler{completer: completer}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// llmSettings is the model override block every caller may send. It is the
// same subset predict_llm honours, for the same reasons: model_project_id and
// integration_uid are accepted and ignored, because honouring a
// caller-supplied project here would let a caller spend another project's
// provider credentials from a path whose only membership check is on
// {projectID}.
type llmSettings struct {
	ModelName       string   `json:"model_name"`
	MaxTokens       *int     `json:"max_tokens"`
	Temperature     *float64 `json:"temperature"`
	ReasoningEffort string   `json:"reasoning_effort"`
}

type skillDraftRequest struct {
	UserDescription string       `json:"user_description"`
	LLMSettings     *llmSettings `json:"llm_settings"`
	// SkillID and VersionID select one stored skill version together.
	SkillID   json.RawMessage `json:"skill_id"`
	VersionID json.RawMessage `json:"version_id"`
}

type applicationDraftRequest struct {
	UserDescription string          `json:"user_description"`
	LLMSettings     *llmSettings    `json:"llm_settings"`
	ApplicationID   json.RawMessage `json:"application_id"`
	VersionID       json.RawMessage `json:"version_id"`
}

type projectContextDraftRequest struct {
	UserDescription string       `json:"user_description"`
	LLMSettings     *llmSettings `json:"llm_settings"`
	// CurrentProjectBackground selects edit mode. Unlike the other two edit
	// modes it carries its own prior content, so it needs no repository and is
	// served.
	CurrentProjectBackground *string `json:"current_project_background"`
}

// SkillDraft is the shape apps/elitea-web's SkillDraft type
// (features/skills/model/types.ts) is cast to directly.
//
// `tags` is the one field legacy's GenerateSkillDraftResponse does not carry.
// It is kept because this platform's skills DO have tags (skills.SkillVersion),
// the SPA's SkillDraft declares the field, and CreateSkill applies the draft
// wholesale with setValue(draft) — so dropping it would write an empty tag list
// over the form on every generated draft.
type SkillDraft struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Instructions string   `json:"instructions"`
	Tags         []string `json:"tags"`
}

// SuggestedResource is one entry of ApplicationDraft's five suggested_*
// lists — matches apps/elitea-web's SuggestedResource
// (features/agents/lib/agentDraft.ts) field for field. `Type` is toolkit-
// only (the toolkit's elitea_tools.type, e.g. "github"); `AgentType` marks a
// suggested_pipelines entry as `"pipeline"` (suggested_agents entries leave
// it empty) — the same discriminant applications.Application.AgentType
// already carries.
type SuggestedResource struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	AgentType   string `json:"agent_type,omitempty"`
}

// ApplicationDraft is the content half of legacy's
// GenerateApplicationDraftResponse, now including the five suggested_* lists
// (issue #881; see the package doc for how they're produced). Each list is
// always present (never null) — [] when no repository was composed for that
// category, or when nothing scored above zero.
type ApplicationDraft struct {
	Name                 string              `json:"name"`
	Description          string              `json:"description"`
	Instructions         string              `json:"instructions"`
	WelcomeMessage       string              `json:"welcome_message"`
	ConversationStarters []string            `json:"conversation_starters"`
	SuggestedToolkits    []SuggestedResource `json:"suggested_toolkits"`
	SuggestedMCP         []SuggestedResource `json:"suggested_mcp"`
	SuggestedPipelines   []SuggestedResource `json:"suggested_pipelines"`
	SuggestedAgents      []SuggestedResource `json:"suggested_agents"`
	SuggestedSkills      []SuggestedResource `json:"suggested_skills"`
}

// ProjectContextDraft is legacy's GenerateProjectContextDraftResponse.
type ProjectContextDraft struct {
	ProjectBackground string `json:"project_background"`
}

// GenerateSkillDraft serves POST /generate_skill_draft/prompt_lib/{projectID}.
func (h *Handler) GenerateSkillDraft(w http.ResponseWriter, r *http.Request) {
	var body skillDraftRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if !requireUserDescription(w, body.UserDescription) {
		return
	}
	prompt := skillDraftSystemPrompt
	if present(body.SkillID) != present(body.VersionID) {
		writeError(w, http.StatusBadRequest, "skill_id and version_id must be supplied together")
		return
	}
	if present(body.SkillID) {
		skillID, skillErr := draftID(body.SkillID)
		versionID, versionErr := draftID(body.VersionID)
		if skillErr != nil || versionErr != nil {
			writeError(w, http.StatusBadRequest, "skill_id and version_id must be positive integers")
			return
		}
		if h.skillVersions == nil {
			writeError(w, http.StatusServiceUnavailable, "skill version reader is not configured")
			return
		}
		if !h.authorizeSkillEdit(w, r) {
			return
		}
		current, err := h.skillVersions.GetVersion(r.Context(), chi.URLParam(r, "projectID"), skillID, versionID)
		if err != nil {
			var failure *apierr.APIError
			if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
				writeError(w, http.StatusNotFound, "skill or version not found")
			} else {
				slog.ErrorContext(r.Context(), "draft: skill version read failed", "err", err)
				writeError(w, http.StatusInternalServerError, "skill version could not be loaded")
			}
			return
		}
		encoded, err := json.Marshal(SkillDraft{Name: current.Name, Description: current.Description,
			Instructions: current.Instructions, Tags: current.Tags})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "skill version could not be prepared")
			return
		}
		prompt += "\n\nRevise this stored skill to satisfy the request. Preserve content that still applies. Return the complete draft.\n" + string(encoded)
	}

	content, ok := h.complete(w, r, prompt, body.UserDescription, body.LLMSettings)
	if !ok {
		return
	}

	var raw struct {
		Name         string   `json:"name"`
		Description  string   `json:"description"`
		Instructions string   `json:"instructions"`
		Tags         []string `json:"tags"`
	}
	if !decodeModelJSON(w, r, content, &raw) {
		return
	}

	name, err := slugifySkillName(raw.Name)
	if err != nil {
		writeUnprocessable(w, r, "skill", err)
		return
	}
	description := strings.TrimSpace(truncate(raw.Description, skillDescriptionMaxLength))
	instructions := strings.TrimSpace(truncate(raw.Instructions, skillInstructionsMaxLength))
	if description == "" || instructions == "" {
		writeUnprocessable(w, r, "skill", errors.New("description and instructions are both required"))
		return
	}

	tags := raw.Tags
	if tags == nil {
		tags = []string{}
	}
	writeJSON(w, http.StatusOK, SkillDraft{
		Name:         name,
		Description:  description,
		Instructions: instructions,
		Tags:         tags,
	})
}

// GenerateApplicationDraft serves POST /generate_application_draft/prompt_lib/{projectID}.
func (h *Handler) GenerateApplicationDraft(w http.ResponseWriter, r *http.Request) {
	var body applicationDraftRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if !requireUserDescription(w, body.UserDescription) {
		return
	}
	if present(body.ApplicationID) || present(body.VersionID) {
		writeError(w, http.StatusBadRequest,
			"editing an existing agent is not served here: application_id and version_id select legacy's edit mode, "+
				"which rewrites a stored agent version and is not ported (#254). Send user_description alone to draft a new agent.")
		return
	}

	content, ok := h.complete(w, r, applicationDraftSystemPrompt, body.UserDescription, body.LLMSettings)
	if !ok {
		return
	}

	var raw struct {
		Name                 string   `json:"name"`
		Description          string   `json:"description"`
		Instructions         string   `json:"instructions"`
		WelcomeMessage       string   `json:"welcome_message"`
		ConversationStarters []string `json:"conversation_starters"`
	}
	if !decodeModelJSON(w, r, content, &raw) {
		return
	}

	name := strings.TrimSpace(truncate(raw.Name, applicationNameMaxLength))
	description := strings.TrimSpace(truncate(raw.Description, applicationDescriptionMaxLength))
	instructions := strings.TrimSpace(truncate(raw.Instructions, applicationInstructionsSanityLength))
	if name == "" || description == "" || instructions == "" {
		writeUnprocessable(w, r, "agent", errors.New("name, description and instructions are all required"))
		return
	}

	starters := make([]string, 0, applicationMaxConversationStarters)
	for _, starter := range raw.ConversationStarters {
		starter = strings.TrimSpace(starter)
		if starter == "" {
			continue
		}
		starters = append(starters, truncate(starter, applicationStarterMaxLength))
		if len(starters) == applicationMaxConversationStarters {
			break
		}
	}

	// The query the five suggestion categories are scored against — the same
	// text a human reviewer sees, not just body.UserDescription: a draft
	// whose instructions elaborate on tools/domains the short user
	// description didn't mention should still surface those matches.
	suggestionQuery := name + " " + description + " " + instructions
	projectID := chi.URLParam(r, "projectID")
	ctx := r.Context()
	suggestedToolkits, suggestedMCP := h.suggestToolkits(ctx, projectID, suggestionQuery)

	writeJSON(w, http.StatusOK, ApplicationDraft{
		Name:                 name,
		Description:          description,
		Instructions:         instructions,
		WelcomeMessage:       strings.TrimSpace(truncate(raw.WelcomeMessage, applicationWelcomeMessageMaxLength)),
		ConversationStarters: starters,
		SuggestedToolkits:    suggestedToolkits,
		SuggestedMCP:         suggestedMCP,
		SuggestedPipelines:   h.suggestApplications(ctx, projectID, suggestionQuery, true),
		SuggestedAgents:      h.suggestApplications(ctx, projectID, suggestionQuery, false),
		SuggestedSkills:      h.suggestSkills(ctx, projectID, suggestionQuery),
	})
}

// GenerateProjectContextDraft serves POST /generate_project_context_draft/prompt_lib/{projectID}.
func (h *Handler) GenerateProjectContextDraft(w http.ResponseWriter, r *http.Request) {
	var body projectContextDraftRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if !requireUserDescription(w, body.UserDescription) {
		return
	}

	prompt := projectContextDraftSystemPrompt
	if body.CurrentProjectBackground != nil {
		prompt = fmt.Sprintf("%s\n\nThe project already has this Project Background. Refine it to satisfy the request, "+
			"keeping everything that is still accurate:\n\n%s",
			projectContextDraftSystemPrompt, truncate(*body.CurrentProjectBackground, projectBackgroundMaxLength))
	}

	content, ok := h.complete(w, r, prompt, body.UserDescription, body.LLMSettings)
	if !ok {
		return
	}

	var raw struct {
		ProjectBackground string `json:"project_background"`
	}
	if !decodeModelJSON(w, r, content, &raw) {
		return
	}
	background := strings.TrimSpace(truncate(raw.ProjectBackground, projectBackgroundMaxLength))
	if background == "" {
		writeUnprocessable(w, r, "project context", errors.New("project_background is required"))
		return
	}
	writeJSON(w, http.StatusOK, ProjectContextDraft{ProjectBackground: background})
}

// complete runs the one blocking LLM turn shared by all three routes. It
// writes the response and returns ok=false when the turn could not be made, so
// the callers read as a straight line.
func (h *Handler) complete(
	w http.ResponseWriter,
	r *http.Request,
	systemPrompt, userDescription string,
	settings *llmSettings,
) (string, bool) {
	projectID := chi.URLParam(r, "projectID")

	if h.completer == nil {
		slog.ErrorContext(r.Context(), "draft: no LLM plane composed", "project_id", projectID)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the LLM gateway is not configured: LLM_GATEWAY_URL is empty, so no completion backend is composed",
			"code":  NotConfiguredCode,
		})
		return "", false
	}

	request := v2predict.CompletionRequest{
		ProjectID: projectID,
		UserID:    callerUserID(r.Context()),
		Messages: []v2predict.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userDescription},
		},
	}
	if settings != nil {
		request.Model = settings.ModelName
		request.Temperature = settings.Temperature
		request.MaxTokens = settings.MaxTokens
		request.ReasoningEffort = settings.ReasoningEffort
	}

	content, err := h.completer.Complete(r.Context(), request)
	if err != nil {
		// 502, not 500: the failure is one identifiable hop away, and an
		// operator who cannot tell "elitea-main is broken" from "the gateway
		// did not answer" cannot tell which component to look at. The upstream
		// message is logged and not echoed — it can carry provider error text.
		slog.ErrorContext(r.Context(), "draft: completion failed",
			"project_id", projectID, "model", request.Model, "err", err)
		writeError(w, http.StatusBadGateway, "the LLM gateway could not complete this request")
		return "", false
	}
	return content, true
}

// decodeBody reads the bounded JSON body, writing the response and returning
// false on any failure. The decoder is deliberately NOT strict: legacy's models
// carry `extra: allow`, so a client sending fields this port does not read must
// not get a 400 for them.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxDraftRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		var maxBytesError *http.MaxBytesError
		switch {
		case errors.As(err, &maxBytesError):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		case errors.Is(err, io.EOF):
			writeError(w, http.StatusBadRequest, "request body is empty")
		default:
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return false
	}
	return true
}

func requireUserDescription(w http.ResponseWriter, userDescription string) bool {
	if strings.TrimSpace(userDescription) == "" {
		writeError(w, http.StatusBadRequest, "user_description is required")
		return false
	}
	return true
}

// present reports whether a raw JSON field was sent with a value. A field that
// is absent decodes to nil; one sent explicitly as null decodes to the four
// bytes "null", which is not a value either — a client clearing the field must
// not trip the edit-mode refusal.
func present(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

// decodeModelJSON extracts the JSON object from the model's free text and
// decodes it. Models fence their answers, prefix them with prose, or both, so
// the outermost {...} is taken rather than the whole string — the same
// tolerance legacy's extract_json_from_text has.
//
// A model answer that is not decodable is 422, not 500: the hop worked and the
// service is not broken; the generation is. 422 is also what legacy answered,
// and what the SPA distinguishes so it can offer "try again".
func decodeModelJSON(w http.ResponseWriter, r *http.Request, content string, into any) bool {
	object, err := extractJSONObject(content)
	if err != nil {
		slog.WarnContext(r.Context(), "draft: model answer is not JSON", "err", err, "length", len(content))
		writeError(w, http.StatusUnprocessableEntity, truncatedOrUnusable(content,
			"the model did not return a JSON draft"))
		return false
	}
	if err := json.Unmarshal([]byte(object), into); err != nil {
		slog.WarnContext(r.Context(), "draft: model JSON did not decode", "err", err)
		writeError(w, http.StatusUnprocessableEntity, truncatedOrUnusable(object,
			"the model returned unusable JSON"))
		return false
	}
	return true
}

// truncatedOrUnusable picks the message. More opening braces than closing ones
// is the signature of an answer the model ran out of tokens mid-way through,
// and the caller repairs that by raising max_tokens — where "unparseable"
// reads as "retry the same request", which fails identically every time.
func truncatedOrUnusable(content, otherwise string) string {
	if strings.Count(content, "{") > strings.Count(content, "}") {
		return "the model's answer was cut off. Raise max_tokens in llm_settings (4096 or more) and try again."
	}
	return otherwise
}

// extractJSONObject finds the outermost JSON object in free model text.
func extractJSONObject(content string) (string, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start == -1 || end == -1 || end < start {
		return "", errors.New("no JSON object found in the model response")
	}
	return content[start : end+1], nil
}

// skillNamePattern is legacy's SKILL_NAME_RE: lowercase letters, digits and
// hyphens, never leading or trailing.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

var skillNameSeparators = regexp.MustCompile(`[^a-z0-9-]+`)

var skillNameRuns = regexp.MustCompile(`-{2,}`)

// slugifySkillName ports legacy's _slugify_skill_name + validate_skill_name.
// The name is COERCED first and only then checked, so a model that answers
// "PR Reviewer" produces "pr-reviewer" rather than a failed generation. The
// reserved-word rule is legacy's own and is a refusal, not a coercion: silently
// renaming a skill the user asked for would be the wrong kind of helpful.
func slugifySkillName(value string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(value))
	slug = skillNameSeparators.ReplaceAllString(slug, "-")
	slug = skillNameRuns.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	slug = strings.Trim(truncate(slug, skillNameMaxLength), "-")

	if slug == "" || !skillNamePattern.MatchString(slug) {
		return "", errors.New("name must be lowercase letters, digits and hyphens")
	}
	if strings.Contains(slug, "claude") || strings.Contains(slug, "anthropic") {
		return "", errors.New(`name cannot contain "claude" or "anthropic"`)
	}
	return slug, nil
}

// truncate cuts to at most limit RUNES, never bytes: cutting mid-rune would
// put a replacement character into a draft the user is about to save.
func truncate(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimRight(string(runes[:limit]), " \t\r\n")
}

func writeUnprocessable(w http.ResponseWriter, r *http.Request, entity string, err error) {
	slog.WarnContext(r.Context(), "draft: generated draft failed validation", "entity", entity, "err", err)
	writeError(w, http.StatusUnprocessableEntity,
		fmt.Sprintf("the generated %s draft is not usable: %s", entity, err.Error()))
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]any{"error": message})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("draft: failed to encode response", "err", err)
	}
}

// callerUserID reads the authenticated principal the router's Auth middleware
// put on the context. It is never taken from a header or from the body.
func callerUserID(ctx context.Context) string {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return ""
	}
	if user.UserID != "" {
		return user.UserID
	}
	return user.ID
}

// draftID accepts numeric and string IDs without float conversion.
func draftID(raw json.RawMessage) (string, error) {
	value := strings.TrimSpace(string(raw))
	if strings.HasPrefix(value, "\"") {
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
	}
	id, err := strconv.ParseInt(value, 10, 32)
	if err != nil || id <= 0 {
		return "", errors.New("invalid ID")
	}
	return strconv.FormatInt(id, 10), nil
}

// WithPermissions enables the detail permission check before stored skill reads.
func WithPermissions(resolver auth.PermissionResolver) Option {
	return func(h *Handler) { h.permissions = resolver }
}

func (h *Handler) authorizeSkillEdit(w http.ResponseWriter, r *http.Request) bool {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "skill edit requires an authenticated caller")
		return false
	}
	if h.permissions == nil {
		writeError(w, http.StatusServiceUnavailable, "skill edit authorization is not configured")
		return false
	}
	resolution, err := h.permissions.ResolvePermissions(r.Context(), user, auth.PermissionModeDefault, chi.URLParam(r, "projectID"))
	if err != nil {
		if errors.Is(err, auth.ErrPermissionDenied) {
			writeError(w, http.StatusForbidden, "skill details access is required")
		} else {
			writeError(w, http.StatusInternalServerError, "skill edit authorization failed")
		}
		return false
	}
	for _, permission := range resolution.Permissions {
		if permission == "models.applications.skills.details" && resolution.UserID > 0 {
			return true
		}
	}
	writeError(w, http.StatusForbidden, "skill details access is required")
	return false
}
