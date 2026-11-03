package repos

// The cloud half of unattended chains at admission (ADR-0045 decision 4,
// #1436).

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// admitAgentChain writes the chain of an agent admission that carries a
// grant: the chain row and link 0 for the first run, or the chain check and
// the link for a continuation. A continuation marks its messages with
// meta.continued_automatically.
func admitAgentChain(
	ctx context.Context, tx pgx.Tx, admission agentexecutionapp.Admission, projectionProject int64,
) error {
	chain := admission.Unattended
	if chain == nil {
		return nil
	}
	executionID := admission.Record.Job.ID
	switch {
	case chain.Start != nil:
		start := *chain.Start
		start.RootExecutionID = executionID
		if err := startRunChain(ctx, tx, start); err != nil {
			return err
		}
		if start.BriefState != unattended.BriefPending || start.ResponseMessageID == "" {
			return nil
		}
		// The transcript shows "Writing the brief…" until the brief is
		// ready (ADR-0045 decision 11). A replayed admission finds the
		// chain already there: its brief may be ready or failed by now, and
		// the stored meta stays as SaveBrief wrote it.
		pending, err := runBriefStillPending(ctx, tx, start.RootExecutionID)
		if err != nil || !pending {
			return err
		}
		return markRunBrief(ctx, tx, projectionProject, start.ResponseMessageID,
			runBriefMeta{State: unattended.BriefPending})
	case chain.Continue != nil:
		next := *chain.Continue
		next.ExecutionID = executionID
		link, err := continueRunChain(ctx, tx, next)
		if err != nil {
			return err
		}
		return markContinuedMessages(ctx, tx, projectionProject, chain.MarkMessageIDs, link.ContinuationIndex,
			link.RootExecutionID, link.GrantID)
	}
	return nil
}

// markContinuedMessages adds continued_automatically and the chain position
// to the meta of a continuation's messages.
func markContinuedMessages(
	ctx context.Context, tx pgx.Tx, projectID int64, messageIDs []string, index int, root, grantID string,
) error {
	if len(messageIDs) == 0 {
		return nil
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return err
	}
	mark, err := json.Marshal(map[string]any{
		"continued_automatically": true,
		"unattended": map[string]any{
			"grant_id": grantID, "chain_root_execution_id": root, "continuation_index": index,
		},
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.chat_message_group
SET meta = COALESCE(meta, '{}'::jsonb) || $2::jsonb
WHERE uuid = ANY($1::uuid[])`, schema), messageIDs, string(mark)); err != nil {
		return fmt.Errorf("unattended chain: mark the continued messages: %w", err)
	}
	return nil
}

// markStepLimitReached writes step_limit_reached on a response: set when
// the run stopped at its step limit under an unattended grant, removed when
// it did not (an output-limit continuation reuses the response). The
// after-settle hook reads it to start the continuation.
func markStepLimitReached(ctx context.Context, tx sqlExecutor, messageGroupID int64, reached bool) error {
	statement := `
UPDATE chat_message_group
SET meta = meta - 'step_limit_reached'
WHERE id = $1 AND meta ? 'step_limit_reached'`
	if reached {
		statement = `
UPDATE chat_message_group
SET meta = COALESCE(meta, '{}'::jsonb) || '{"step_limit_reached": true}'::jsonb
WHERE id = $1`
	}
	if _, err := tx.Exec(ctx, statement, messageGroupID); err != nil {
		return fmt.Errorf("mark the step limit on the response: %w", err)
	}
	return nil
}
