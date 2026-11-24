package adminaccess

import (
	"slices"
	"strings"
)

// Classifier names a request-dependent classifier: a function of what the
// request changes that picks one of a route's actions. The danger of a
// retention write depends on whether it shortens or lengthens the policy, so
// the route is not marked critical as a whole (ADR-0059 decision 6).
//
// Each classifier is a pure function below. The gate that enforces the table
// (R1.2, R2.8, R3.8) computes its inputs from the stored row and the request,
// calls it, and looks the entry up with ActionName(selector value, outcome).
type Classifier string

// The classifiers and their outcomes.
const (
	// ClassifyRetention: RetentionAction.
	ClassifyRetention Classifier = "retention_bounds"
	// ClassifyGlobalProvider: GlobalProviderUpdateAction.
	ClassifyGlobalProvider Classifier = "global_provider_fields"
	// ClassifyIdentityProvider: IdentityProviderSaveAction.
	ClassifyIdentityProvider Classifier = "identity_provider_fields"
	// ClassifyGovernanceRow: GovernanceRowAction.
	ClassifyGovernanceRow Classifier = "governance_row_type"
	// ClassifyMaintenance: MaintenanceAction.
	ClassifyMaintenance Classifier = "maintenance_switch"
	// ClassifySuspensionScope: SuspensionAction.
	ClassifySuspensionScope Classifier = "suspension_scope"
	// ClassifyUserSuspend: UserSuspendAction.
	ClassifyUserSuspend Classifier = "user_suspend"
)

// The outcomes.
const (
	OutcomeShorten  = "shorten"
	OutcomeLengthen = "lengthen"

	OutcomeRoute    = "route"
	OutcomeSettings = "settings"

	OutcomeTrust   = "trust"
	OutcomeDisable = "disable"

	OutcomeGuardrail = "guardrail"
	OutcomeOther     = "other"

	OutcomeOn = "on"

	OutcomePlatform = "platform"
	OutcomeScoped   = "scoped"

	OutcomeSuspend   = "suspend"
	OutcomeUnsuspend = "unsuspend"
)

// classifierOutcomes lists every outcome of each classifier. NewActions
// requires one entry per outcome.
var classifierOutcomes = map[Classifier][]string{
	ClassifyRetention:        {OutcomeLengthen, OutcomeShorten},
	ClassifyGlobalProvider:   {OutcomeRoute, OutcomeSettings},
	ClassifyIdentityProvider: {OutcomeDisable, OutcomeSettings, OutcomeTrust},
	ClassifyGovernanceRow:    {OutcomeGuardrail, OutcomeOther},
	ClassifyMaintenance:      {OutcomeOn, OutcomeOther},
	ClassifySuspensionScope:  {OutcomePlatform, OutcomeScoped},
	ClassifyUserSuspend:      {OutcomeSuspend, OutcomeUnsuspend},
}

// RetentionBounds are the bounds of a retention policy. A nil MaxAgeDays keeps
// data forever. OverridesApply is set on a platform policy whose project
// overrides apply (the class is overridable and the policy allows them).
type RetentionBounds struct {
	MinAgeDays     int
	MaxAgeDays     *int
	OverridesApply bool
}

// RetentionAction answers OutcomeShorten when moving from old to new lets data
// be deleted sooner: the maximum drops (forever to a number counts), the
// minimum drops, or the project overrides start to apply (a stored override
// below the platform maximum then takes effect, and one may be stored while
// they did not apply). Anything else lengthens or keeps the policy.
//
// The three retention writes call it: a platform policy save (old and new
// platform rows), a project override save (old effective maximum, new
// override) and an override delete (the override, then the platform maximum
// it falls back to).
func RetentionAction(old, new RetentionBounds) string {
	if new.OverridesApply && !old.OverridesApply {
		return OutcomeShorten
	}
	if new.MinAgeDays < old.MinAgeDays {
		return OutcomeShorten
	}
	if new.MaxAgeDays != nil && (old.MaxAgeDays == nil || *new.MaxAgeDays < *old.MaxAgeDays) {
		return OutcomeShorten
	}
	return OutcomeLengthen
}

// globalProviderSettingsFields are the fields of a global provider whose
// change neither sends prompts elsewhere nor changes who the platform is to
// the provider. Every other field, named or not, is the endpoint, the
// credential or the keyless identity, so an unknown field fails toward
// critical. Only plain text a person reads is listed: a model list can carry
// a per-model base address, so `models` is not a setting.
var globalProviderSettingsFields = []string{"label", "display_name", "description", "tags"}

// GlobalProviderUpdateAction answers OutcomeRoute when an update of a global
// provider changes its endpoint, credential or keyless identity, and
// OutcomeSettings when it changes only the fields above. changed are the
// names of the fields the update changes, one level deep: a top-level field
// of the row, or a direct field of its `data`. A change anywhere inside a
// nested value is reported as the field that holds it, so a nested key that
// happens to be called `label` cannot pass as one.
func GlobalProviderUpdateAction(changed []string) string {
	for _, field := range changed {
		if !slices.Contains(globalProviderSettingsFields, strings.ToLower(field)) {
			return OutcomeRoute
		}
	}
	return OutcomeSettings
}

// IdentityProviderSaveAction classifies a save of an identity provider from
// the trust fields it changes, as identityproviders.TrustChanges names them
// ("created", "enabled", "issuer", …, plus "secret" for a replaced secret),
// and whether the provider is enabled after the save:
//
//   - OutcomeDisable when the only trust change turns the provider off;
//   - OutcomeTrust for any other trust change, turning it on included (that
//     retires the live provider of its kind);
//   - OutcomeSettings when no trust field changes.
func IdentityProviderSaveAction(trustChanges []string, enabledAfter bool) string {
	switch {
	case len(trustChanges) == 0:
		return OutcomeSettings
	case len(trustChanges) == 1 && trustChanges[0] == "enabled" && !enabledAfter:
		return OutcomeDisable
	}
	return OutcomeTrust
}

// GovernanceRowAction answers OutcomeGuardrail for a gateway governance row of
// the type `guardrail` (internal/api/gateway, guardrailType) and OutcomeOther
// for every other type. An update or a delete passes the type of the stored
// row, and an update that changes the type passes OutcomeGuardrail if either
// side is a guardrail: the caller classifies both and keeps the worse.
func GovernanceRowAction(rowType string) string {
	if rowType == "guardrail" {
		return OutcomeGuardrail
	}
	return OutcomeOther
}

// MaintenanceAction answers OutcomeOn when a save of the Maintenance section
// turns maintenance mode on, and OutcomeOther for any other save.
func MaintenanceAction(enabledBefore, enabledAfter bool) string {
	if !enabledBefore && enabledAfter {
		return OutcomeOn
	}
	return OutcomeOther
}

// SuspensionAction answers OutcomePlatform for a kill switch over the whole
// platform (scope "platform", application/safety.ScopePlatform) and
// OutcomeScoped for a project, an agent or a toolkit type.
func SuspensionAction(scope string) string {
	if scope == "platform" {
		return OutcomePlatform
	}
	return OutcomeScoped
}

// UserSuspendAction answers OutcomeSuspend or OutcomeUnsuspend from the
// `suspended` field of the request.
func UserSuspendAction(suspended bool) string {
	if suspended {
		return OutcomeSuspend
	}
	return OutcomeUnsuspend
}
