package repos

import (
	"context"
	"fmt"
	"slices"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

// admissionSafetyRecord is what an admission records on its job for the
// runtime safety controls (shared 0201), in the admission's transaction:
//
//   - toolkit_types: the run's canonical toolkit types, so a toolkit_type
//     suspension can cancel it in flight (ADR-0041 decision 1, #1498);
//   - browser_session_ref: the browser session whose request admitted it,
//     so the run joins that session's audit events (ADR-0042 decision 5).
//     It is read from the authenticated principal on ctx, which only the
//     browser session reader sets; a token, schedule or trigger run has none.
type admissionSafetyRecord struct {
	// ToolkitTypes nil leaves the column NULL ("not recorded"); an empty,
	// non-nil list records "no toolkit".
	ToolkitTypes []string
}

// toolkitAdmissionTypes is the record of a direct toolkit run: its one type,
// from the kill switch's admission subject. nil when no gate marked ctx.
func toolkitAdmissionTypes(ctx context.Context) []string {
	subject, ok := safety.AdmissionSubject(ctx)
	if !ok {
		return nil
	}
	if key := guardrails.CanonicalKey(subject.ToolkitType); key != "" {
		return []string{key}
	}
	return nil
}

// stampAdmissionSafety writes the record onto one job generation. It writes
// nothing when there is nothing to record.
func stampAdmissionSafety(
	ctx context.Context, tx sqlExecutor, executionID string, generation int64, record admissionSafetyRecord,
) error {
	var sessionRef *int64
	if user, ok := auth.UserFromContext(ctx); ok && user.BrowserSessionRef > 0 {
		ref := user.BrowserSessionRef
		sessionRef = &ref
	}
	var types []string
	if record.ToolkitTypes != nil {
		types = make([]string, 0, len(record.ToolkitTypes))
		for _, value := range record.ToolkitTypes {
			if key := guardrails.CanonicalKey(value); key != "" {
				types = append(types, key)
			}
		}
		slices.Sort(types)
		types = slices.Compact(types)
	}
	if sessionRef == nil && types == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET toolkit_types = COALESCE($3::text[], toolkit_types),
    browser_session_ref = COALESCE($4::bigint, browser_session_ref)
WHERE execution_id = $1 AND generation = $2`,
		executionID, generation, types, sessionRef); err != nil {
		return fmt.Errorf("record the run's safety admission record: %w", err)
	}
	return nil
}

// inheritContinuedRunStart sets a continuation's run_started_at to the
// whole-run start of the execution it continues: the newest earlier
// execution bound to the same conversation and response message. A
// regeneration of that response is a new run (it records no start of its
// own), so the newest one is the right predecessor. With no predecessor the
// column stays NULL (admitted_at).
func inheritContinuedRunStart(
	ctx context.Context, tx sqlExecutor, executionID string, generation int64, streamID, messageID string,
) error {
	if streamID == "" || messageID == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs AS j
SET run_started_at = previous.started_at
FROM (
    SELECT COALESCE(pj.run_started_at, pj.admitted_at) AS started_at
    FROM elitea_runtime.agent_execution_jobs AS pa
    JOIN elitea_runtime.execution_jobs AS pj
      ON pj.execution_id = pa.execution_id AND pj.generation = pa.generation
    WHERE pa.client_stream_id = $3
      AND pa.client_message_id = $4
      AND pa.execution_id <> $1
    ORDER BY pj.admitted_at DESC
    LIMIT 1
) AS previous
WHERE j.execution_id = $1 AND j.generation = $2`,
		executionID, generation, streamID, messageID); err != nil {
		return fmt.Errorf("record the continued run's start: %w", err)
	}
	return nil
}
