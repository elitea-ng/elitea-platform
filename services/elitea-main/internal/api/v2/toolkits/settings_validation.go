package toolkits

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// ToolkitSettingsValidator resolves one toolkit's settings against the saved
// configuration rows the CALLER can see, without redeeming any secret.
//
// It exists because nothing on the toolkit write path used to resolve a
// credential at all: Create checked that a `github` toolkit named SOME
// `github_configuration` key and Update checked nothing, so a toolkit could be
// saved pointing at a credential that lives in another project, or at one that
// does not exist anywhere. The reference was then read for the first time at
// chat time, where the failure aborts the whole agent turn
// (internal/application/agentexecution/tools.go's unsupportedStartBecause) —
// far from the screen that caused it and with no field to point at.
//
// The one implementation is *configurationapp.CurrentToolkitSettingsResolver in
// CurrentToolkitSettingsReferenceMode, the SAME resolver and the same mode that
// index admission and agent-version freezing already run
// (internal/runtimecomposition/{index,agent}_runtime.go). The interface is
// declared here, consumer-side, for the reason ToolkitArgumentSchemaSource
// above gives: internal/runtimecomposition imports this package, so this
// package cannot import it back.
//
// A nil validator accepts every CREDENTIAL REFERENCE unresolved, which is the
// previous behaviour. That is what a deployment with
// ELITEA_CONFIGURATIONS_ENABLED unset gets, because the collaborator graph this
// resolver needs is composed only inside that flag (cmd/elitea-main/main.go).
//
// It is NOT a full bypass of the write path: refuseFrozenConfigurationMarker
// runs whether or not this is assigned, because that refusal needs no
// collaborator — see its own comment.
type ToolkitSettingsValidator interface {
	Resolve(context.Context, configurationapp.CurrentToolkitSettingsRequest) (map[string]any, error)
}

// WithSettingsValidator supplies the save-time settings resolver. Without it,
// Create and Update persist whatever credential reference the body carries (but
// still refuse a body carrying the reserved frozen-configuration marker) — see
// ToolkitSettingsValidator.
func WithSettingsValidator(validator ToolkitSettingsValidator) Option {
	return func(h *Handler) { h.settingsValidator = validator }
}

// toolkitSave names the save a settings refusal is about, so its log line says
// what was refused and where: the action, the project, the toolkit (its id on
// an update, the body's name when it sends one) and, beside it, the type.
type toolkitSave struct {
	action    string // create_toolkit | update_toolkit
	projectID string
	toolkitID string // empty on create: nothing exists yet
	name      string // the body's name; empty when the body sends none
}

func newToolkitSave(r *http.Request, action string, body map[string]any) toolkitSave {
	name, _ := body["name"].(string)
	return toolkitSave{
		action:    action,
		projectID: chi.URLParam(r, "projectID"),
		toolkitID: chi.URLParam(r, "toolkitID"),
		name:      name,
	}
}

// maxLoggedToolkitNameBytes bounds the caller's name in a refusal's log line.
const maxLoggedToolkitNameBytes = 128

// logAttrs is the refusal's slog attributes: the save's identity, the type,
// then the caller's own key/value pairs.
func (s toolkitSave) logAttrs(toolkitType string, extra ...any) []any {
	attrs := []any{"action", s.action, "project_id", s.projectID, "toolkit_type", toolkitType}
	if s.toolkitID != "" {
		attrs = append(attrs, "toolkit_id", s.toolkitID)
	}
	if name := s.name; name != "" {
		if len(name) > maxLoggedToolkitNameBytes {
			cut := maxLoggedToolkitNameBytes
			for cut > 0 && !utf8.RuneStart(name[cut]) {
				cut--
			}
			name = name[:cut]
		}
		attrs = append(attrs, "toolkit_name", name)
	}
	return append(attrs, extra...)
}

// toolkitSettingsViolationMessages turns a resolver violation code into the
// sentence the toolkit form shows beside the offending field.
//
// The violations themselves carry NO text by design — they "deliberately
// exclude configuration titles, toolkit IDs, model names, expected or actual
// configuration types, and secret values"
// (internal/application/configurations/toolkit_settings.go's
// CurrentToolkitSettingsViolation) — so the sentence has to be composed here,
// and it must stay as free of the same details as the code is.
//
// The two credential sentences are worded to match the strings the web client
// already shows for the equivalent legacy error bodies
// (apps/elitea-web/src/features/toolkits/lib/helpers/toolkitForm.helpers.ts's
// ERROR_HANDLERS), so the screen reads the same however the error arrived.
//
// CurrentToolkitSecretNotSealedCode is DELIBERATELY ABSENT, and its absence is
// load-bearing: see refuseUnresolvableToolkitSettings.
var toolkitSettingsViolationMessages = map[configurationapp.CurrentToolkitSettingsViolationCode]string{
	configurationapp.CurrentToolkitConfigurationReferenceInvalidCode: "This field does not hold a saved-credential reference.",
	configurationapp.CurrentToolkitConfigurationNotFoundCode:         "Your configuration does not match any available configurations.",
	configurationapp.CurrentToolkitConfigurationForbiddenCode:        "Your configuration does not match any available configurations.",
	configurationapp.CurrentToolkitConfigurationRecursionCode:        "This credential reference resolves in a loop.",
	configurationapp.CurrentToolkitConfigurationTypeMismatchCode:     "The selected credential is not of the type this field accepts.",
	configurationapp.CurrentToolkitReferenceInvalidCode:              "This field does not hold a saved-toolkit reference.",
	configurationapp.CurrentToolkitReferenceNotFoundCode:             "The referenced toolkit is not available in this project.",
	configurationapp.CurrentToolkitReferenceRecursionCode:            "This toolkit reference resolves in a loop.",
	configurationapp.CurrentToolkitModelReferenceInvalidCode:         "This field does not hold a model reference.",
	configurationapp.CurrentToolkitModelNotFoundCode:                 "This model is no longer available in project configurations.",
}

// toolkitSettingsFieldErrors renders the refusable violations as the
// `settings_errors` entries the web client's parseValidationErrors consumes.
//
// `loc` MUST carry two elements. The client keys each entry by loc[1] and
// SILENTLY DROPS anything else (toolkitForm.helpers.ts's locFieldKey), so a
// one-element loc — which is what this handler's own ValidateToolkit route
// still emits — renders a blank form with no error on any field and a save
// that just fails. loc[0] is the containing object, always "settings" here.
func toolkitSettingsFieldErrors(violations []configurationapp.CurrentToolkitSettingsViolation) []map[string]any {
	entries := make([]map[string]any, 0, len(violations))
	for _, violation := range violations {
		message, refusable := toolkitSettingsViolationMessages[violation.Code]
		if !refusable || violation.Field == "" {
			continue
		}
		entries = append(entries, map[string]any{
			"loc":  []string{"settings", violation.Field},
			"msg":  message,
			"type": "value_error",
			// The stable code, beside the sentence. The client renders `msg`;
			// this is for logs, tests and any later consumer that wants to
			// branch without string-matching English.
			"code": string(violation.Code),
		})
	}
	return entries
}

// refuseUnresolvableToolkitSettings resolves the settings a write is about to
// persist and answers the request itself when they cannot be resolved. It
// reports whether it wrote a response; true means the caller must stop.
//
// WHAT IT DOES NOT DO:
//
//   - It never persists the resolver's OUTPUT. Reference mode returns the
//     configuration EXPANDED and stamped with CurrentFrozenConfigurationMarker
//     (toolkit_settings.go), and writing that into elitea_tools.settings would
//     replace a live reference with a frozen snapshot and forge a vault owner
//     through configuration_project_id — precisely what that marker's own doc
//     comment warns against. Only the error is used; the body is persisted
//     unchanged. Pinned by
//     TestAnAcceptedSavePersistsTheRequestSettingsByteForByte here and by the
//     accepted-save assertion in
//     internal/api/toolkit_credential_admission_postgres_integration_test.go:
//     turning the discard into a copy-back left every other test green.
//   - It never refuses an ABSENT credential. Reference mode rewrites a
//     missing/null/false configuration field to `{}` and moves on, because an
//     anonymous OpenAPI toolkit is a legitimate save. A green save with no
//     credential is therefore NOT evidence that this gate failed to run.
//   - It never refuses on CurrentToolkitSecretNotSealedCode. That code fires on
//     a schema-declared `secret` field whose stored value is not yet a vault
//     reference — a property of rows that predate save-time vault wrapping, not
//     of the edit being made. Refusing it here would make an already-stored
//     toolkit of one of the ten affected SDK types (aws, azure, azure_search,
//     elastic, gcp, keycloak, kubernetes, yagmail, zephyr, zephyr_squad)
//     impossible to edit at all, including impossible to fix. The runtime
//     already fails closed on it.
//   - It never refuses an UNKNOWN toolkit type. `database`, `custom`,
//     `datasource` and every `*_loader` are served by the create form and are
//     absent from the pinned SDK snapshot, so the resolver answers
//     ErrCurrentToolkitSchemaNotFound for them. Refusing on that would make
//     most of the create form unusable.
//
// WHICH PRINCIPALS IT VALIDATES. The resolver refuses a non-positive UserID
// outright, so the gate can only run for a principal auth.User.OwningUserID can
// name — and OwningUserID returns positiveID(u.UserID) FIRST, whenever UserID is
// set at all (internal/auth/types.go). Every production authentication path sets
// it to a positive numeric id, including both token paths:
//
//   - a Form session sets ID and UserID to the session's user, which the
//     Form service has already reloaded as an active positive id
//     (internal/api/browserauth/form_session.go);
//   - direct PAT validation sets ID, UserID and TokenID from the token row, and
//     refuses the row outright unless both ids are positive
//     (internal/infra/authsvc/local_validator.go);
//   - a session cookie sets UserID from a `uid` claim it has already checked is
//     a positive integer (verifySessionCookie).
//
// So a PAT-driven save IS validated, against the TOKEN OWNER's credential
// visibility. That is the correct principal: the configuration rows a PAT may
// reference are its owner's, and the same UserID is what index admission and
// agent freezing resolve against.
//
// NO PRINCIPAL CLASS TAKES THE SKIP TODAY. It remains for the cases where this
// gate genuinely has nothing to resolve against: no auth.User in the request
// context at all, an id outside int32, and a projectID that is not a positive
// tenant-shaped integer. The (0,false) arm of OwningUserID needs an auth.User
// that is a token AND has no UserID. The one writer that could produce it was
// the legacy Redis RPC client, which copied user_id straight from the response
// without checking it and has since been deleted (the same history
// internal/api/middleware/project.go's project-by-name note records). The skip
// keeps any later writer of such a user from validating against the wrong
// user.
//
// Substituting the "1" that Create defaults userID to would validate against
// USER 1's credential visibility instead of the caller's, which is worse than
// not validating: it would admit a credential the caller cannot see and refuse
// one they can.
func (h *Handler) refuseUnresolvableToolkitSettings(
	w http.ResponseWriter,
	r *http.Request,
	save toolkitSave,
	toolkitType string,
	settings map[string]any,
) bool {
	if settings == nil {
		return false
	}
	// UNCONDITIONAL, and deliberately ahead of every skip below — including the
	// nil-validator skip. The marker is not a credential judgement that needs
	// the configurations graph to make; it is a property of the request body
	// alone, and persisting it is the exact forgery
	// CurrentFrozenConfigurationMarker's own doc comment exists to prevent. A
	// deployment with ELITEA_CONFIGURATIONS_ENABLED unset composes no validator
	// and still writes to the same column the claim-time materializer reads.
	if h.refuseFrozenConfigurationMarker(w, r, save, toolkitType, settings) {
		return true
	}
	// A memory toolkit's two own settings, which no configuration graph
	// judges: unconditional like the marker, so a deployment without the
	// validator still refuses a namespace or scope the worker would skip.
	if h.refuseMalformedMemorySettings(w, r, save, toolkitType, settings) {
		return true
	}
	// The bucket the file-store toolkits keep their files in, which their
	// schema declares required and nothing else enforces on save: like the
	// memory rules, unconditional.
	if h.refuseMissingToolkitBucket(w, r, save, toolkitType, settings) {
		return true
	}
	if h.settingsValidator == nil || toolkitType == "" {
		return false
	}
	projectNumber, err := tenantOwnerID(save.projectID)
	if err != nil || projectNumber > math.MaxInt32 {
		return false
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return false
	}
	ownerID, ok := user.OwningUserID()
	if !ok || ownerID > math.MaxInt32 {
		return false
	}

	if _, err := h.settingsValidator.Resolve(r.Context(), configurationapp.CurrentToolkitSettingsRequest{
		ToolkitType: toolkitType,
		Settings:    settings,
		ProjectID:   int32(projectNumber),
		UserID:      int32(ownerID),
		Mode:        configurationapp.CurrentToolkitSettingsReferenceMode,
	}); err != nil {
		return h.answerToolkitSettingsFailure(w, r, save, toolkitType, err)
	}
	return false
}

// answerToolkitSettingsFailure triages one Resolve error into refuse / proceed /
// unavailable. It reports whether it wrote a response.
func (h *Handler) answerToolkitSettingsFailure(
	w http.ResponseWriter,
	r *http.Request,
	save toolkitSave,
	toolkitType string,
	err error,
) bool {
	var validation *configurationapp.CurrentToolkitSettingsValidationError
	if errors.As(err, &validation) {
		entries := toolkitSettingsFieldErrors(validation.Violations)
		if len(entries) == 0 {
			// Every violation was one this gate does not refuse on (today:
			// secret_not_sealed). Saving is the previous behaviour and the
			// only one that leaves the row editable.
			return false
		}
		// DELIBERATELY UNDOCUMENTED IN api/openapi/v2.yaml. createToolkit and
		// updateToolkit already declare a 400 whose ErrorResponse schema sets no
		// additionalProperties:false, so `settings_errors` is a legal extension
		// of the documented body; the identical {valid, settings_errors}
		// envelope already ships from /toolkit_validator/, a route with no
		// OpenAPI operation at all. Editing the spec instead would trip the
		// oapi-codegen diff gate, the generated-web-client drift gate, the
		// contract workflow, the passthrough-marker rule and a pinned operation
		// count, for a body no generated client reads. Recorded here so the
		// omission is a stated choice rather than silence.
		slog.WarnContext(r.Context(), "toolkit save refused: its settings reference a configuration that is not available",
			save.logAttrs(toolkitType, "violations", len(entries))...)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":           "toolkit settings reference a configuration that is not available in this project",
			"valid":           false,
			"settings_errors": entries,
		})
		return true
	}
	if errors.Is(err, configurationapp.ErrCurrentToolkitSchemaNotFound) ||
		errors.Is(err, configurationapp.ErrCurrentToolkitSchemaInvalid) {
		// SERVER-SIDE conditions, and the only two that still proceed.
		//
		// SchemaNotFound is a toolkit type the pinned SDK snapshot does not
		// describe (`custom`, `database`, every `*_loader`) — see the list
		// above. SchemaInvalid is a pinned schema that IS present and cannot be
		// read: a non-object `properties`, a malformed `$ref`, a
		// `configuration_types` that is not a list of strings
		// (toolkit_settings.go's schema-plan builder). Neither says anything
		// about the credential the caller named, neither is anything the caller
		// can influence from the request body, and neither is anything the
		// caller could fix — refusing on them would make every toolkit of the
		// affected type impossible to save or edit while the snapshot stays
		// broken. Both fail closed later anyway: the runtime resolves the same
		// schema at claim time.
		return false
	}
	if errors.Is(err, configurationapp.ErrInvalidCurrentToolkitSettings) {
		// CALLER-SIDE, at every raise site this handler can reach, so this
		// REFUSES. It used to sit in the branch above, which handed the request
		// body an off switch for the whole gate: appending
		// `"__elitea_frozen_configuration_v1": true` — or 40 levels of dummy
		// nesting — to a settings blob whose credential lives in another
		// project turned a 400 into a 201 that persisted the foreign reference.
		//
		// The reachable raise sites in
		// internal/application/configurations/toolkit_settings.go are:
		//
		//   - the Resolve entry guard, for a `type` past
		//     MaxCurrentToolkitSettingsIdentifier or carrying NUL/CR/LF. The
		//     other entry-guard conditions (nil ctx, non-positive ProjectID or
		//     UserID, nil Settings, unknown Mode) cannot fire, because
		//     refuseUnresolvableToolkitSettings has already screened them;
		//   - cloneJSON's key check: CurrentFrozenConfigurationMarker used as a
		//     key at ANY depth, an over-long key, or a key with NUL/CR/LF;
		//   - checkContextAndDepth, past MaxCurrentToolkitSettingsDepth;
		//   - the node / string-byte budgets.
		//
		// All four are properties of the body. The budgets are the one mixed
		// case — an expanded configuration and the pinned schema's own field
		// names are charged to the same budget — but the caller's blob is
		// charged first and is the only side that can be made arbitrarily
		// large, and a save whose expansion does not fit is a save whose claim
		// would not fit either. Refusing is both fail-closed and consistent
		// with the two peers that already triage this sentinel: index admission
		// maps it to ErrInvalidAuthoritativeIndexInput
		// (internal/application/indexing/resolver.go) and claim-time
		// materialization to ErrContentRejected
		// (internal/infra/storage/configurations_materializer.go). Nothing else
		// in the tree treats it as "proceed".
		//
		// No settings_errors: there is no violation and so no field to key one
		// to, and an entry the web client cannot key to a field is dropped
		// silently there (toolkitForm.helpers.ts's locFieldKey). An empty body
		// is what makes the page fall back to its generic banner, which is the
		// only place this can be shown.
		slog.WarnContext(r.Context(), "toolkit save refused: its settings are unresolvable input",
			save.logAttrs(toolkitType, "err", err)...)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "toolkit settings are not in a form this project can store",
			"valid": false,
		})
		return true
	}
	// A briefly unreachable configuration store or vault, or a cancelled
	// request. Proceeding here would silently reintroduce exactly the hole this
	// gate closes; refusing with a distinct status keeps it visible in logs and
	// on the screen instead of hiding inside a 400 about the user's input.
	slog.ErrorContext(r.Context(), "toolkit save refused: settings validation is unavailable",
		save.logAttrs(toolkitType, "err", err)...)
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": "toolkit settings validation is unavailable",
	})
	return true
}

// refuseFrozenConfigurationMarker answers the request when the settings a write
// is about to persist carry CurrentFrozenConfigurationMarker. It reports whether
// it wrote a response.
//
// WHY THIS IS ITS OWN REFUSAL AND NOT A SILENT STRIP. The marker means "this
// object was expanded from a saved configuration by ReferenceMode, and the
// configuration_project_id beside it names the vault that owns its secrets"
// (toolkit_settings.go). Claim-time materialization requires it before it will
// treat a stored object as a frozen configuration and read that owner
// (currentFrozenConfigurationOwner in
// internal/infra/storage/configurations_materializer.go). A request body that
// supplies it is therefore attempting to forge a vault owner, which is the one
// thing the resolver rejects the key to prevent — and before this refusal
// existed, such a body was persisted verbatim.
//
// Stripping the key instead would silently accept the rest of a body written to
// exploit it and leave no trace on the screen or in the log. Refusing says what
// happened, and no legitimate client writes this key: the web credential picker
// stores exactly `{elitea_title, private}` (credentialPicker.tsx's
// toStoredValue), and the marker's own namespace prefix exists so it cannot
// collide with a schema field.
//
// The message NAMES the key. It is an exported constant, not a secret, and a
// caller that hit this by round-tripping a materialized settings blob back into
// a save needs to know which key to drop.
//
// NO LEGITIMATE ROUND TRIP CARRIES IT, so this refuses nothing a normal edit
// does. ReferenceMode's marked output is never persisted into
// elitea_tools.settings (see refuseUnresolvableToolkitSettings' first bullet),
// and claim-time materialization strips the key before the SDK sees it
// (configurations_materializer.go). (The legacy index-meta reader also treated
// a marked pgvector_configuration as unusable; that setting is now retired and
// dropped on every save, application/configurations/retired.go.) A stored row that DOES
// carry one is therefore a row forged through the bypass this refusal closes;
// it can still be renamed by a settings-free PATCH and deleted, but it cannot
// be saved back with the forgery intact, which is the intended outcome.
func (h *Handler) refuseFrozenConfigurationMarker(
	w http.ResponseWriter,
	r *http.Request,
	save toolkitSave,
	toolkitType string,
	settings map[string]any,
) bool {
	if !carriesFrozenConfigurationMarker(settings) {
		return false
	}
	slog.WarnContext(r.Context(), "toolkit save refused: its settings carry the reserved frozen-configuration marker",
		save.logAttrs(toolkitType, "reserved_key", configurationapp.CurrentFrozenConfigurationMarker)...)
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": "toolkit settings may not contain the reserved key " +
			configurationapp.CurrentFrozenConfigurationMarker,
		"valid": false,
	})
	return true
}

// carriesFrozenConfigurationMarker reports whether any object anywhere in the
// settings tree uses CurrentFrozenConfigurationMarker as a key.
//
// ITERATIVE, NOT RECURSIVE, and that is deliberate: `settings` is decoded
// straight out of the request body, so its nesting depth is chosen by the
// caller. A recursive walk here would let a body pick this process's stack
// depth. The walk visits each node once and allocates only the pending stack.
func carriesFrozenConfigurationMarker(settings map[string]any) bool {
	pending := []any{settings}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch value := current.(type) {
		case map[string]any:
			if _, marked := value[configurationapp.CurrentFrozenConfigurationMarker]; marked {
				return true
			}
			for _, item := range value {
				pending = append(pending, item)
			}
		case []any:
			pending = append(pending, value...)
		}
	}
	return false
}

// storedToolkitType reads the toolkit type a settings-only PATCH does not
// restate. An unreadable row answers "" so the caller skips validation, which
// is the same degrade the type-guardrail block above it already applies to a
// body that omits `type`.
func (h *Handler) storedToolkitType(ctx context.Context, projectID, toolkitID string) string {
	stored, err := h.repo.GetToolkit(ctx, projectID, toolkitID)
	if err != nil {
		return ""
	}
	storedType, _ := stored["type"].(string)
	return storedType
}

// memoryNamespaceMaxBytes is the longest namespace a memory toolkit may name;
// the Rust worker enforces the same rule (agent-runtime families/memory) and
// skips a toolkit that breaks it.
const memoryNamespaceMaxBytes = 256

// memoryScopes are the values of a memory toolkit's `memory_scope`. A toolkit
// is personal OR project (owner decision 2026-10-14): the former `both` is not
// saved any more. A toolkit already stored with it is read as personal by the
// worker (no data migration), and a save must pick one of the two.
var memoryScopes = map[string]bool{"personal": true, "project": true}

// memorySettingsProblems lists what is wrong with a memory toolkit's
// `namespace` and `memory_scope` (an absent or null value is fine: the
// namespace defaults to the toolkit's id and the scope to personal).
func memorySettingsProblems(settings map[string]any) []map[string]any {
	var problems []map[string]any
	problem := func(field, message string) {
		problems = append(problems, map[string]any{
			"loc":  []string{"settings", field},
			"msg":  message,
			"type": "value_error",
			"code": "invalid_memory_setting",
		})
	}
	if value, present := settings["namespace"]; present && value != nil {
		namespace, isText := value.(string)
		switch {
		case !isText || strings.TrimSpace(namespace) == "":
			problem("namespace", "The namespace must be a non-blank text.")
		case len(namespace) > memoryNamespaceMaxBytes:
			problem("namespace", "The namespace must be at most 256 bytes.")
		case strings.ContainsFunc(namespace, unicode.IsControl):
			problem("namespace", "The namespace must not contain control characters.")
		}
	}
	if value, present := settings["memory_scope"]; present && value != nil {
		scope, isText := value.(string)
		if !isText || !memoryScopes[scope] {
			problem("memory_scope", "The memory scope must be personal or project. To use both, attach two memory toolkits.")
		}
	}
	return problems
}

// refuseMalformedMemorySettings answers 400 for a memory toolkit whose
// `namespace` or `memory_scope` is malformed. It reports whether it wrote a
// response.
func (h *Handler) refuseMalformedMemorySettings(
	w http.ResponseWriter,
	r *http.Request,
	save toolkitSave,
	toolkitType string,
	settings map[string]any,
) bool {
	if toolkitType != "memory" {
		return false
	}
	problems := memorySettingsProblems(settings)
	if len(problems) == 0 {
		return false
	}
	slog.WarnContext(r.Context(), "toolkit save refused: the memory toolkit's settings are not valid",
		save.logAttrs(toolkitType, "problems", len(problems))...)
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":           "the memory toolkit's settings are not valid",
		"valid":           false,
		"settings_errors": problems,
	})
	return true
}

// toolkitBucketSetting names the settings key that holds the bucket of each
// toolkit type that stores its files in one. Every one is in its schema's
// `required` list (the SDK's for `artifact` and `pptx`, rust_only_toolkit_tools.json
// for `docx` and `xlsx`); `imagegen` is not here because its tools answer
// a missing bucket themselves.
var toolkitBucketSetting = map[string]string{
	"artifact": "bucket",
	"pptx":     "bucket_name",
	"docx":     "bucket_name",
	"xlsx":     "bucket_name",
}

// refuseMissingToolkitBucket answers 400 for a file-store toolkit saved with
// no bucket (absent, null, not a text, or blank). It reports whether it wrote
// a response.
//
// Before, such a toolkit saved fine and then failed EVERY turn of every agent
// it was attached to (live 4.45.0: `native_agent.invalid_configuration`, "the
// native agent toolsets could not be materialized"). The worker now leaves it
// out with a reason instead; this keeps it from being saved that way at all.
// Only presence is checked: the name's shape is the schema's `pattern`, which
// the form enforces, and the worker skips a toolkit whose name breaks it.
func (h *Handler) refuseMissingToolkitBucket(
	w http.ResponseWriter,
	r *http.Request,
	save toolkitSave,
	toolkitType string,
	settings map[string]any,
) bool {
	field, stores := toolkitBucketSetting[toolkitType]
	if !stores {
		return false
	}
	if bucket, isText := settings[field].(string); isText && strings.TrimSpace(bucket) != "" {
		return false
	}
	// The save is REFUSED and nothing is written. Live 4.45.1: this line used
	// to read "a toolkit was saved without a bucket", which reads as a
	// bucketless toolkit having been stored.
	slog.WarnContext(r.Context(), "toolkit save refused: a file-store toolkit needs a bucket",
		save.logAttrs(toolkitType, "missing_setting", field)...)
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": "the toolkit needs a bucket",
		"valid": false,
		"settings_errors": []map[string]any{{
			"loc":  []string{"settings", field},
			"msg":  "Choose the bucket this toolkit keeps its files in.",
			"type": "value_error",
			"code": "missing_bucket",
		}},
	})
	return true
}
