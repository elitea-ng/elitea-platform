package pipelinetriggers

// Mandate-bound schedules and triggers (ADR-0039, issue #1122).
//
// With credential governance on, the composition root hands this package a
// runmandate.Service (WithGovernedRuns). Every schedule fire and inbound call
// then runs under the mandate attached to its row, and under nothing else:
//
//   - NO MANDATE, NO RUN. A schedule with no `mandate_id` is skipped with
//     `skipped_mandate_required` (MANDATE_REQUIRED); a trigger answers 403.
//     Both append a security audit decision event. There is no fallback to
//     `author_id`, to `created_by` or to any system identity.
//   - THE MANDATE IS RE-CHECKED AT EVERY FIRE: active, not expired, the
//     epochs of the mandate, its binding and its delegation current, its
//     binding bound to the version, and its scopes still covering the
//     schedule. The admission then writes it into the run's security
//     context (repos.bindMandateAuthority), where every lease renewal checks
//     its epoch again (#1121).
//   - THE SUBJECT IS THE MANDATE'S. A delegated_background mandate runs as
//     its subject user, whose project permission is still re-resolved at
//     fire time (rule 4 of the package doc). An autonomous mandate has no
//     subject: the run is attributed to the author or creator, and no
//     person's permission is its authority.
//   - A WEBHOOK'S CALLER IS EVIDENCE, NOT AUTHORITY. The inbound credential
//     still proves the caller; the security context records it as
//     CallerContextV1 only.
//
// Without the service none of this runs, no statement here is sent, and
// both entry points behave exactly as before, including the author reset.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// The governed schedule skips (tenant 0158 adds them to the CHECK).
const (
	resultSkippedMandateRequired = "skipped_mandate_required"
	resultSkippedMandateUnusable = "skipped_mandate_not_usable"
	resultSkippedMandateScope    = "skipped_mandate_scope"
)

// WithGovernedRuns makes every schedule fire and inbound call mandate-bound.
// The composition root passes it only with credential governance on; a nil
// service leaves the handler as it was.
func WithGovernedRuns(service *runmandate.Service) Option {
	return func(h *Handler) {
		if service != nil {
			h.governed = service
		}
	}
}

// attachedMandate reads the mandate attached to one row of table.
func (h *Handler) attachedMandate(ctx context.Context, schema, table string, id int64) (string, error) {
	var mandateID *string
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT mandate_id FROM %s.%s WHERE id = $1`, schema, table), id).Scan(&mandateID); err != nil {
		return "", err
	}
	if mandateID == nil {
		return "", nil
	}
	return *mandateID, nil
}

// scheduleTarget is what a schedule's mandate must cover.
func scheduleTarget(projectID int64, schedule scheduleRow) runmandate.Target {
	input := schedule.UserInput
	return runmandate.Target{
		Origin: runmandate.OriginPipelineSchedule, TenantProjectID: projectID,
		ResourceID: strconv.FormatInt(schedule.ID, 10), ApplicationVersionID: schedule.VersionID,
		Cron: schedule.Cron, Input: &input,
	}
}

// scheduleAuthority is the governed fire-time check of one schedule. It
// answers the authority, or the last_result and detail of the skip; an
// error is a fault of this deployment, not of the schedule.
func (h *Handler) scheduleAuthority(
	ctx context.Context, tenant scheduleTenant, schedule scheduleRow,
) (securitycontext.RunAuthority, string, string, error) {
	mandateID, err := h.attachedMandate(ctx, tenant.Schema, "pipeline_schedules", schedule.ID)
	if err != nil {
		return securitycontext.RunAuthority{}, "", "", err
	}
	target := scheduleTarget(tenant.ProjectID, schedule)
	authority, err := h.governed.Admit(ctx, mandateID, target)
	if err == nil {
		return authority, "", "", nil
	}
	code := runmandate.Code(err)
	if code == "" {
		return securitycontext.RunAuthority{}, "", "", err
	}
	if auditErr := h.governed.Refuse(ctx, target, mandateID, err); auditErr != nil {
		h.log().Error("pipelinetriggers: record the mandate refusal",
			"project_id", tenant.ProjectID, "schedule_id", schedule.ID, "err", auditErr)
	}
	result, detail := mandateSkip(code)
	return securitycontext.RunAuthority{}, result, detail, nil
}

// mandateSkip is the settings-tab answer for a refusal code. The detail
// leads with the code, so a client can match it.
func mandateSkip(code string) (string, string) {
	switch code {
	case runmandate.CodeMandateRequired:
		return resultSkippedMandateRequired, code + ": attach a mandate to this schedule; governed schedules run only under one"
	case runmandate.CodeMandateScopeMismatch:
		return resultSkippedMandateScope, code + ": the mandate's scope no longer covers this schedule's cron or input"
	default:
		return resultSkippedMandateUnusable, code + ": the attached mandate is not usable for this schedule"
	}
}

// triggerAuthority is the governed check of one inbound call. ok false means
// the response is written.
func (h *Handler) triggerAuthority(
	w http.ResponseWriter, r *http.Request, schema string, projectID int64, trigger triggerRow,
	record func(status int, projectID, userID, versionID int64, reason string),
) (securitycontext.RunAuthority, bool) {
	mandateID, err := h.attachedMandate(r.Context(), schema, "pipeline_triggers", trigger.ID)
	if err != nil {
		h.log().Error("pipelinetriggers: read the trigger mandate", "err", err)
		record(http.StatusServiceUnavailable, projectID, trigger.CreatedBy, trigger.VersionID, "the mandate could not be read")
		writeError(w, http.StatusServiceUnavailable, "this trigger could not be checked")
		return securitycontext.RunAuthority{}, false
	}
	target := runmandate.Target{
		Origin: runmandate.OriginPipelineTrigger, TenantProjectID: projectID,
		ResourceID: strconv.FormatInt(trigger.ID, 10), ApplicationVersionID: trigger.VersionID,
	}
	authority, err := h.governed.Admit(r.Context(), mandateID, target)
	if err != nil {
		code := runmandate.Code(err)
		if code == "" {
			h.log().Error("pipelinetriggers: check the trigger mandate", "err", err)
			record(http.StatusServiceUnavailable, projectID, trigger.CreatedBy, trigger.VersionID, "the mandate could not be checked")
			writeError(w, http.StatusServiceUnavailable, "this trigger could not be checked")
			return securitycontext.RunAuthority{}, false
		}
		if auditErr := h.governed.Refuse(r.Context(), target, mandateID, err); auditErr != nil {
			h.log().Error("pipelinetriggers: record the mandate refusal", "err", auditErr)
		}
		// The credential was accepted, so naming the cause is no oracle.
		record(http.StatusForbidden, projectID, trigger.CreatedBy, trigger.VersionID, code)
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "this trigger has no usable mandate", "code": code,
		})
		return securitycontext.RunAuthority{}, false
	}
	// The inbound credential proved the caller; it is recorded, not trusted.
	authority.Caller = securitycontext.Caller{
		AuthenticationSource: "pipeline_trigger." + trigger.AuthMode,
		CallingApplicationID: "pipeline_trigger:" + strconv.FormatInt(trigger.ID, 10),
	}
	return authority, true
}

// saveCovered is the governed save check: a schedule with an attached
// mandate keeps it, and the new cron and input must stay inside its scope.
// ok false means the response is written.
func (h *Handler) saveCovered(
	w http.ResponseWriter, r *http.Request, schema string, projectID, versionID int64, cron, input string,
) bool {
	existing, err := h.scheduleByVersion(r.Context(), schema, versionID)
	if errors.Is(err, ErrNotFound) {
		return true
	}
	if err != nil {
		h.log().Error("pipelinetriggers: read schedule", "err", err)
		writeError(w, http.StatusInternalServerError, "the schedule could not be read")
		return false
	}
	mandateID, err := h.attachedMandate(r.Context(), schema, "pipeline_schedules", existing.ID)
	if err != nil {
		h.log().Error("pipelinetriggers: read the schedule mandate", "err", err)
		writeError(w, http.StatusInternalServerError, "the schedule could not be read")
		return false
	}
	if mandateID == "" {
		return true
	}
	existing.Cron, existing.UserInput = cron, input
	switch err := h.governed.Covers(r.Context(), mandateID, scheduleTarget(projectID, existing)); {
	case err == nil, errors.Is(err, runmandate.ErrMandateNotFound):
		// A mandate that is gone stops the next fire; it does not block
		// the save.
		return true
	case errors.Is(err, runmandate.ErrScopeMismatch):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "the schedule's mandate does not cover this cron or input",
			"code":  runmandate.CodeMandateScopeMismatch,
		})
		return false
	default:
		h.log().Error("pipelinetriggers: check the schedule mandate", "err", err)
		writeError(w, http.StatusInternalServerError, "the schedule's mandate could not be checked")
		return false
	}
}

// governedStartRefused reports a start the admission refused because the
// mandate changed after the fire-time check.
func governedStartRefused(err error) bool {
	return errors.Is(err, repos.ErrRunAuthorityNotUsable)
}
