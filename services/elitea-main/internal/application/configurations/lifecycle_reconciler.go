package configurations

import (
	"context"
	"errors"
)

// This reconciler no longer pushes anything into a provider proxy. The Bifrost
// gateway (services/elitea-llm-gateway) resolves per-project credentials and
// model definitions by reading the very p_{projectID}.configuration rows this
// lifecycle already writes — see that service's internal/account/credentials.go
// — so there is nothing to materialize into a second system. What survives is
// the DB-side half the product still depends on:
//
//   - status_ok, which is not decoration. The gateway's credential query filters
//     `section = 'ai_credentials' AND ... AND status_ok = true`, and the model
//     catalog and embedding-binding reads in this service filter on it too. A
//     row that never reaches status_ok = true is a row no runtime will use, so
//     the flag remains the single admission decision for a provider row.
//   - the reference/secret resolution that gates that flag. Expanding a
//     configuration's declared references and redeeming its hidden-secret
//     references is the one check that can still fail locally, so it stays as
//     the precondition for status_ok = true (see
//     CurrentProviderConfigurationResolver).
//   - the two internal reference-repair effects after a rename or an LLM delete.
//
// Current baseline evidence for the surviving behavior:
//
//   - projects/centry/pylon_main/plugins/runtime_interface_litellm/methods/
//     configuration_entities.py:36-203 at revision
//     997ecd9c866d0ac048fffb01bbecd2197c3d7435 manages the six credential types
//     and the llm, embedding, image_generation, tts, and asr model sections. It
//     skips models whose ai_credentials value is empty.
//   - projects/centry/pylon_main/plugins/runtime_interface_litellm/methods/
//     configuration_transformations.py:38-115 at the same revision performs the
//     expansion and unsecreting that the resolution port preserves.
//   - projects/centry/pylon_main/plugins/runtime_interface_litellm/methods/
//     tools.py:97-111 at the same revision applies allow_project_own_llms while
//     always permitting the public project.
//   - projects/centry/pylon_main/plugins/elitea_core/events/configuration.py:
//     73-109 and methods/configuration.py:43-87 at revision
//     41321950099cd1c3a71a96cc5f9b8766ef5fc7cc repair renamed toolkit
//     references and application defaults after an LLM configuration delete.
//
// Generic configuration schemas remain owned by elitea-sdk. A configuration
// that is not one of the exact provider credential or model contracts below is
// deliberately passive here.

var ErrInvalidCurrentConfigurationLifecycleEffectsReconciler = errors.New(
	"invalid current configuration lifecycle effects reconciler",
)

const (
	currentLifecycleIntentInvalidCode        = "LIFECYCLE_INTENT_INVALID"
	currentLifecycleProviderResolutionFailed = "PROVIDER_RESOLUTION_FAILED"
	currentLifecycleStatusFailedCode         = "STATUS_WRITE_FAILED"
	currentLifecycleRenameFailedCode         = "CONFIGURATION_RENAME_FAILED"
	currentLifecycleDeletedLLMFailedCode     = "DELETED_LLM_REPAIR_FAILED"
	currentLifecycleStatusReconcilingCode    = "PROVIDER_RECONCILING"
	currentLifecycleStatusReconciledCode     = "PROVIDER_RECONCILED"
	currentLifecycleEffectProviderResolve    = "provider:resolve"
	currentLifecycleEffectStatusPending      = "status:pending"
	currentLifecycleEffectStatusHealthy      = "status:healthy"
	currentLifecycleEffectRename             = "dependents:rename"
	currentLifecycleEffectDeletedLLMRepair   = "dependents:deleted-llm"
)

type currentConfigurationLifecycleEntityKind uint8

const (
	currentConfigurationLifecyclePassive currentConfigurationLifecycleEntityKind = iota
	currentConfigurationLifecycleProviderCredential
	currentConfigurationLifecycleProviderModel
)

// CurrentProviderProjectPolicy preserves the baseline allow_project_own_llms
// behavior for project-bound configuration events. Administration entities
// without a project are outside this configuration lifecycle contract.
//
// The policy outlives the proxy it was written for. It decides whether a
// non-public project's own provider row is admitted, and admission is now
// expressed as status_ok: a rejected row keeps status_ok = false, which is
// exactly the predicate the Bifrost gateway, the model catalog, and the
// embedding-binding lookup all filter on. Disallowing project-own LLMs
// therefore still means those rows are never used by any runtime.
type CurrentProviderProjectPolicy struct {
	AllowProjectOwnLLMs bool
	PublicProjectID     int32
}

// admits reports whether the project may own a provider row. It is the single
// copy of the rule: the lifecycle reconciler and CurrentProviderAdmission both
// call it, so a write route and the lifecycle cannot disagree about which
// project is allowed its own credential.
func (policy CurrentProviderProjectPolicy) admits(projectID int32) bool {
	return policy.AllowProjectOwnLLMs || projectID == policy.PublicProjectID
}

// CurrentProviderConfigurationResolution is immutable input to the resolution
// check that gates status_ok. Configuration.Data contains hidden-secret
// references and declared configuration references, not resolved values; the
// adapter owns bounded expansion/unsecreting, must not mutate or log the
// snapshot, and must not persist or return the resolved result.
type CurrentProviderConfigurationResolution struct {
	EffectID          string
	Revision          int64
	ProjectID         int32
	ConfigurationUUID string
	Section           string
	Configuration     CurrentConfigurationLifecycleSnapshot
}

// CurrentProviderConfigurationResolver proves that a provider configuration's
// references and hidden secrets actually resolve before the row is marked
// usable. It replaces the ensure-into-LiteLLM effect: the push used to fail
// when a reference dangled or a secret was missing, and that failure was what
// held status_ok at false. Nothing is pushed anywhere now, but the same local
// failure must still hold the row back, otherwise an unusable row would be
// advertised to the gateway and fail only at request time.
//
// A retry with the same EffectID must be safe; the check is read-only.
type CurrentProviderConfigurationResolver interface {
	ResolveCurrentProviderConfiguration(context.Context, CurrentProviderConfigurationResolution) error
}

type CurrentConfigurationLifecycleStatusUpdate struct {
	EffectID          string
	EventID           string
	Revision          int64
	ProjectID         int32
	ConfigurationID   int32
	ConfigurationUUID string
	StatusOK          bool
	SafeCode          string
}

// CurrentConfigurationLifecycleStatusWriter updates status internally and
// idempotently. Its implementation must not append another lifecycle event;
// otherwise status feedback creates an infinite reconciliation loop.
type CurrentConfigurationLifecycleStatusWriter interface {
	SetCurrentConfigurationLifecycleStatus(context.Context, CurrentConfigurationLifecycleStatusUpdate) error
}

type CurrentConfigurationRenameEffect struct {
	EffectID          string
	EventID           string
	Revision          int64
	ProjectID         int32
	ConfigurationUUID string
	Type              string
	BeforeTitle       string
	AfterTitle        string
}

// CurrentConfigurationRenameEffects repairs toolkit configuration references.
// Repeating an effect with the same EffectID must be safe.
type CurrentConfigurationRenameEffects interface {
	RenameCurrentConfigurationReferences(context.Context, CurrentConfigurationRenameEffect) error
}

type CurrentDeletedLLMEffect struct {
	EffectID  string
	EventID   string
	Revision  int64
	ProjectID int32
	ModelName string
}

// CurrentDeletedLLMEffects repairs application defaults after an LLM
// configuration is deleted. Repeating an effect with the same EffectID must be
// safe, including the public-project fan-out performed by its implementation.
type CurrentDeletedLLMEffects interface {
	RepairCurrentDeletedLLMReferences(context.Context, CurrentDeletedLLMEffect) error
}

// CurrentConfigurationLifecycleEffectsReconciler applies only externally
// observable lifecycle effects. Claiming, retries, leases, and ordering across
// revisions remain owned by CurrentConfigurationLifecycleProcessor and its
// PostgreSQL store.
type CurrentConfigurationLifecycleEffectsReconciler struct {
	resolver   CurrentProviderConfigurationResolver
	status     CurrentConfigurationLifecycleStatusWriter
	renames    CurrentConfigurationRenameEffects
	deletedLLM CurrentDeletedLLMEffects
	policy     CurrentProviderProjectPolicy
}

func NewCurrentConfigurationLifecycleEffectsReconciler(
	resolver CurrentProviderConfigurationResolver,
	status CurrentConfigurationLifecycleStatusWriter,
	renames CurrentConfigurationRenameEffects,
	deletedLLM CurrentDeletedLLMEffects,
	policy CurrentProviderProjectPolicy,
) (*CurrentConfigurationLifecycleEffectsReconciler, error) {
	if resolver == nil || status == nil || renames == nil || deletedLLM == nil || policy.PublicProjectID <= 0 {
		return nil, ErrInvalidCurrentConfigurationLifecycleEffectsReconciler
	}
	return &CurrentConfigurationLifecycleEffectsReconciler{
		resolver: resolver, status: status, renames: renames, deletedLLM: deletedLLM, policy: policy,
	}, nil
}

func (r *CurrentConfigurationLifecycleEffectsReconciler) ReconcileCurrentConfigurationLifecycle(
	ctx context.Context,
	event CurrentConfigurationLifecycleEvent,
	intent CurrentConfigurationLifecycleIntent,
) (CurrentConfigurationLifecycleReconcileResult, error) {
	if ctx == nil || r == nil || r.resolver == nil || r.status == nil || r.renames == nil || r.deletedLLM == nil {
		return currentConfigurationLifecycleDeadResult(currentLifecycleIntentInvalidCode), nil
	}
	if err := ctx.Err(); err != nil {
		return CurrentConfigurationLifecycleReconcileResult{}, err
	}
	if !validCurrentConfigurationLifecycleEffectsIntent(event, intent) {
		return currentConfigurationLifecycleDeadResult(currentLifecycleIntentInvalidCode), nil
	}

	switch intent.Operation {
	case CurrentConfigurationCreated:
		return r.reconcileCurrentConfigurationCreated(ctx, event, *intent.After)
	case CurrentConfigurationUpdated:
		return r.reconcileCurrentConfigurationUpdated(ctx, event, *intent.Before, *intent.After)
	case CurrentConfigurationDeleted:
		return r.reconcileCurrentConfigurationDeleted(ctx, event, *intent.Before)
	default:
		return currentConfigurationLifecycleDeadResult(currentLifecycleIntentInvalidCode), nil
	}
}

func (r *CurrentConfigurationLifecycleEffectsReconciler) reconcileCurrentConfigurationCreated(
	ctx context.Context,
	event CurrentConfigurationLifecycleEvent,
	after CurrentConfigurationLifecycleSnapshot,
) (CurrentConfigurationLifecycleReconcileResult, error) {
	if !currentConfigurationNeedsProviderResolution(after) {
		return currentConfigurationLifecycleSuccessResult(), nil
	}
	if result, err, failed := r.setCurrentConfigurationLifecycleStatus(
		ctx, event, after, false, currentLifecycleEffectStatusPending, currentLifecycleStatusReconcilingCode,
	); failed {
		return result, err
	}
	// A project the policy does not admit stops here, holding status_ok at
	// false. That is the whole enforcement: every reader of a provider row —
	// the Bifrost gateway's credential query, the model catalog, the embedding
	// binding — selects on status_ok = true.
	if !r.currentProviderConfigurationAllowed(after.ProjectID) {
		return currentConfigurationLifecycleSuccessResult(), nil
	}
	if result, err, failed := r.resolveCurrentProviderConfiguration(ctx, event, after); failed {
		return result, err
	}
	if result, err, failed := r.setCurrentConfigurationLifecycleStatus(
		ctx, event, after, true, currentLifecycleEffectStatusHealthy, currentLifecycleStatusReconciledCode,
	); failed {
		return result, err
	}
	return currentConfigurationLifecycleSuccessResult(), nil
}

func (r *CurrentConfigurationLifecycleEffectsReconciler) reconcileCurrentConfigurationUpdated(
	ctx context.Context,
	event CurrentConfigurationLifecycleEvent,
	before CurrentConfigurationLifecycleSnapshot,
	after CurrentConfigurationLifecycleSnapshot,
) (CurrentConfigurationLifecycleReconcileResult, error) {
	beforeManaged := currentConfigurationNeedsProviderResolution(before)
	afterManaged := currentConfigurationNeedsProviderResolution(after)
	// The pending write is still keyed off the BEFORE snapshot too: a row that
	// stops being a provider row (its ai_credentials was cleared, say) must
	// drop back to status_ok = false, because the new payload was never
	// resolved. There is no remote copy to retract any more — withdrawing the
	// row from every reader is exactly this status write.
	if beforeManaged || afterManaged {
		if result, err, failed := r.setCurrentConfigurationLifecycleStatus(
			ctx, event, after, false, currentLifecycleEffectStatusPending, currentLifecycleStatusReconcilingCode,
		); failed {
			return result, err
		}
	}
	resolvedAfter := false
	if afterManaged && r.currentProviderConfigurationAllowed(after.ProjectID) {
		if result, err, failed := r.resolveCurrentProviderConfiguration(ctx, event, after); failed {
			return result, err
		}
		resolvedAfter = true
	}
	if before.EliteaTitle != after.EliteaTitle {
		effect := CurrentConfigurationRenameEffect{
			EffectID: currentConfigurationLifecycleEffectID(event.EventID, currentLifecycleEffectRename),
			EventID:  event.EventID, Revision: event.Revision, ProjectID: after.ProjectID,
			ConfigurationUUID: after.UUID, Type: after.Type,
			BeforeTitle: before.EliteaTitle, AfterTitle: after.EliteaTitle,
		}
		if result, err, failed := currentConfigurationLifecycleEffectFailure(
			ctx,
			r.renames.RenameCurrentConfigurationReferences(ctx, effect),
			currentLifecycleRenameFailedCode,
		); failed {
			return result, err
		}
	}
	if resolvedAfter {
		if result, err, failed := r.setCurrentConfigurationLifecycleStatus(
			ctx, event, after, true, currentLifecycleEffectStatusHealthy, currentLifecycleStatusReconciledCode,
		); failed {
			return result, err
		}
	}
	return currentConfigurationLifecycleSuccessResult(), nil
}

func (r *CurrentConfigurationLifecycleEffectsReconciler) reconcileCurrentConfigurationDeleted(
	ctx context.Context,
	event CurrentConfigurationLifecycleEvent,
	before CurrentConfigurationLifecycleSnapshot,
) (CurrentConfigurationLifecycleReconcileResult, error) {
	// Deleting the configuration row is the withdrawal. Every consumer reads the
	// row, so once it is gone the credential or model is gone with it; there is
	// no external registration left behind to unregister.
	if before.Section == string(CurrentModelSectionLLM) {
		modelName, ok := currentConfigurationLifecycleModelSourceName(before)
		if !ok {
			return currentConfigurationLifecycleDeadResult(currentLifecycleIntentInvalidCode), nil
		}
		effect := CurrentDeletedLLMEffect{
			EffectID: currentConfigurationLifecycleEffectID(event.EventID, currentLifecycleEffectDeletedLLMRepair),
			EventID:  event.EventID, Revision: event.Revision, ProjectID: before.ProjectID,
			ModelName: modelName,
		}
		if result, err, failed := currentConfigurationLifecycleEffectFailure(
			ctx,
			r.deletedLLM.RepairCurrentDeletedLLMReferences(ctx, effect),
			currentLifecycleDeletedLLMFailedCode,
		); failed {
			return result, err
		}
	}
	return currentConfigurationLifecycleSuccessResult(), nil
}

func (r *CurrentConfigurationLifecycleEffectsReconciler) resolveCurrentProviderConfiguration(
	ctx context.Context,
	event CurrentConfigurationLifecycleEvent,
	configuration CurrentConfigurationLifecycleSnapshot,
) (CurrentConfigurationLifecycleReconcileResult, error, bool) {
	kind := currentConfigurationLifecycleKind(configuration)
	if kind == currentConfigurationLifecyclePassive {
		return currentConfigurationLifecycleSuccessResult(), nil, false
	}
	if kind == currentConfigurationLifecycleProviderModel {
		// A managed model with no data.name is unusable and always was: the
		// catalog keys on that name, and so does the deleted-LLM repair. It is
		// a malformed intent, not a transient failure, so it must not retry.
		if _, ok := currentConfigurationLifecycleModelSourceName(configuration); !ok {
			return currentConfigurationLifecycleDeadResult(currentLifecycleIntentInvalidCode), nil, true
		}
	}
	resolution := CurrentProviderConfigurationResolution{
		EffectID:          currentConfigurationLifecycleEffectID(event.EventID, currentLifecycleEffectProviderResolve),
		Revision:          event.Revision,
		ProjectID:         configuration.ProjectID,
		ConfigurationUUID: configuration.UUID,
		Section:           configuration.Section,
		Configuration:     configuration,
	}
	return currentConfigurationLifecycleEffectFailure(
		ctx,
		r.resolver.ResolveCurrentProviderConfiguration(ctx, resolution),
		currentLifecycleProviderResolutionFailed,
	)
}

func (r *CurrentConfigurationLifecycleEffectsReconciler) setCurrentConfigurationLifecycleStatus(
	ctx context.Context,
	event CurrentConfigurationLifecycleEvent,
	configuration CurrentConfigurationLifecycleSnapshot,
	statusOK bool,
	effectSuffix string,
	safeCode string,
) (CurrentConfigurationLifecycleReconcileResult, error, bool) {
	update := CurrentConfigurationLifecycleStatusUpdate{
		EffectID: currentConfigurationLifecycleEffectID(event.EventID, effectSuffix),
		EventID:  event.EventID, Revision: event.Revision,
		ProjectID: configuration.ProjectID, ConfigurationID: configuration.ID,
		ConfigurationUUID: configuration.UUID, StatusOK: statusOK, SafeCode: safeCode,
	}
	return currentConfigurationLifecycleEffectFailure(
		ctx, r.status.SetCurrentConfigurationLifecycleStatus(ctx, update), currentLifecycleStatusFailedCode,
	)
}

func (r *CurrentConfigurationLifecycleEffectsReconciler) currentProviderConfigurationAllowed(projectID int32) bool {
	return r.policy.admits(projectID)
}

func currentConfigurationLifecycleKind(
	configuration CurrentConfigurationLifecycleSnapshot,
) currentConfigurationLifecycleEntityKind {
	if configuration.Section == "ai_credentials" && currentProviderCredentialType(configuration.Type) {
		return currentConfigurationLifecycleProviderCredential
	}
	if currentConfigurationLifecycleModelSection(configuration.Section) {
		return currentConfigurationLifecycleProviderModel
	}
	return currentConfigurationLifecyclePassive
}

// currentProviderCredentialType lists the p_{projectID}.configuration `type`
// values that the LLM data plane can consume as a provider credential.
//
// It is deliberately NOT currentLiteLLMCredentialType. That predicate is the
// LiteLLM provider table, and it stays where it is: it decides which create
// normalizer owns a registry entry. The data plane is the Bifrost gateway now,
// and its table is larger — see providerConfigTypes in
// services/elitea-llm-gateway/internal/account/credentials.go, which adds
// open_ai_azure, anthropic and vllm. A credential of one of those three types
// was left passive by the lifecycle, so it never reached status_ok = true, so
// the gateway could never read it. The standalone stack seeds a vllm
// credential, which is one reason its seed writes status_ok in raw SQL.
//
// A type outside both tables stays passive on purpose. No runtime can use it,
// so marking it usable would say something untrue about it.
// TestCurrentProviderCredentialTypeCoversGatewayProviderTable reads the gateway
// source and fails when the two tables drift apart. It also requires the pinned
// catalogue to describe every type in this list: a dispatchable type with no
// catalogue entry gets `section = ”` and no data schema, so the row is
// invisible to the gateway and its key cannot be sealed (#G3).
//
// EXPORTED through CurrentProviderCredentialType because the admin global
// provider surface needs the same set: a platform credential of a type no
// runtime can dispatch to is a control with no effect, and a third copy of this
// list is a third place for it to drift from the gateway's.
func currentProviderCredentialType(typeName string) bool {
	switch typeName {
	case "open_ai", "azure_open_ai", "open_ai_azure", "ai_dial", "anthropic",
		"ollama", "amazon_bedrock", "vertex_ai", "vllm":
		return true
	default:
		return false
	}
}

func currentConfigurationLifecycleModelSection(section string) bool {
	switch section {
	case string(CurrentModelSectionLLM), string(CurrentModelSectionEmbedding),
		string(CurrentModelSectionImageGeneration), string(CurrentModelSectionTTS), string(CurrentModelSectionASR):
		return true
	default:
		return false
	}
}

// currentConfigurationNeedsProviderResolution selects the rows whose usability
// this lifecycle decides. An imported model (no ai_credentials) declares no
// references and holds no secrets, so there is nothing to resolve and its
// status is left exactly as the writer stored it.
func currentConfigurationNeedsProviderResolution(configuration CurrentConfigurationLifecycleSnapshot) bool {
	switch currentConfigurationLifecycleKind(configuration) {
	case currentConfigurationLifecycleProviderCredential:
		return true
	case currentConfigurationLifecycleProviderModel:
		return currentConfigurationLifecycleTruthy(configuration.Data["ai_credentials"])
	default:
		return false
	}
}

func currentConfigurationLifecycleTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case map[string]any:
		return len(typed) != 0
	case []any:
		return len(typed) != 0
	case float64:
		return typed != 0
	default:
		return true
	}
}

func currentConfigurationLifecycleModelSourceName(
	configuration CurrentConfigurationLifecycleSnapshot,
) (string, bool) {
	name, ok := configuration.Data["name"].(string)
	return name, ok && name != ""
}

func currentConfigurationLifecycleEffectID(eventID, suffix string) string {
	return eventID + ":" + suffix
}

func currentConfigurationLifecycleEffectFailure(
	ctx context.Context,
	effectErr error,
	safeCode string,
) (CurrentConfigurationLifecycleReconcileResult, error, bool) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return CurrentConfigurationLifecycleReconcileResult{}, ctxErr, true
	}
	if effectErr == nil {
		return currentConfigurationLifecycleSuccessResult(), nil, false
	}
	if errors.Is(effectErr, context.Canceled) {
		return CurrentConfigurationLifecycleReconcileResult{}, context.Canceled, true
	}
	if errors.Is(effectErr, context.DeadlineExceeded) {
		return CurrentConfigurationLifecycleReconcileResult{}, context.DeadlineExceeded, true
	}
	return CurrentConfigurationLifecycleReconcileResult{
		Disposition: CurrentConfigurationLifecycleRetry,
		ErrorCode:   safeCode,
	}, nil, true
}

func currentConfigurationLifecycleSuccessResult() CurrentConfigurationLifecycleReconcileResult {
	return CurrentConfigurationLifecycleReconcileResult{Disposition: CurrentConfigurationLifecycleReconciled}
}

func currentConfigurationLifecycleDeadResult(code string) CurrentConfigurationLifecycleReconcileResult {
	return CurrentConfigurationLifecycleReconcileResult{
		Disposition: CurrentConfigurationLifecycleDead,
		ErrorCode:   code,
	}
}

func validCurrentConfigurationLifecycleEffectsIntent(
	event CurrentConfigurationLifecycleEvent,
	intent CurrentConfigurationLifecycleIntent,
) bool {
	if event.EventID == "" || intent.ID != event.EventID || intent.Operation != event.Operation ||
		event.ProjectID <= 0 || event.ConfigurationUUID == "" || event.Revision <= 0 {
		return false
	}
	validSnapshot := func(snapshot *CurrentConfigurationLifecycleSnapshot) bool {
		return snapshot != nil && snapshot.ID > 0 && snapshot.ProjectID == event.ProjectID &&
			snapshot.UUID == event.ConfigurationUUID && snapshot.Type != "" && snapshot.Section != ""
	}
	switch intent.Operation {
	case CurrentConfigurationCreated:
		return intent.Before == nil && validSnapshot(intent.After)
	case CurrentConfigurationDeleted:
		return validSnapshot(intent.Before) && intent.After == nil
	case CurrentConfigurationUpdated:
		if !validSnapshot(intent.Before) || !validSnapshot(intent.After) {
			return false
		}
		return intent.Before.ID == intent.After.ID && intent.Before.UUID == intent.After.UUID &&
			intent.Before.ProjectID == intent.After.ProjectID && intent.Before.Type == intent.After.Type &&
			intent.Before.Section == intent.After.Section
	default:
		return false
	}
}

var _ CurrentConfigurationLifecycleReconciler = (*CurrentConfigurationLifecycleEffectsReconciler)(nil)

// CurrentProviderCredentialType reports whether a configuration type names an
// LLM provider credential the gateway can dispatch to.
//
// The one authority, shared by the two callers that need it: this package's
// lifecycle reconciler, which decides `status_ok`, and the admin global
// provider surface, which refuses to publish a platform credential of any other
// type. TestCurrentProviderCredentialTypeCoversGatewayProviderTable reads the
// gateway's own table and fails when the two drift apart.
func CurrentProviderCredentialType(typeName string) bool {
	return currentProviderCredentialType(typeName)
}
