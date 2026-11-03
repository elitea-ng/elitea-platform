package repos

// The run brief and the goal check of an unattended chain (ADR-0045
// decision 11, #1564; shared 0222).
//
// The brief is frozen: SaveBrief moves brief_state from pending to ready,
// failed or off once, in one conditional UPDATE. A check is written once per
// link: RecordCheck sets run_chain_links.done_check only while it is NULL,
// and the chain's consecutive not_met counts move in the same transaction,
// so a replayed settle hook neither counts twice nor checks twice.
//
// A cloud chain's transcript shows both: meta.run_brief on the root's
// answer and meta.goal_check on the checked run's answer. The tenant
// trigger stamps sync_at on the update, so changes_since carries them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// runBriefMeta is meta.run_brief of a chain root's answer.
type runBriefMeta struct {
	State string          `json:"state"`
	Brief *runbrief.Brief `json:"brief,omitempty"`
}

// markRunBrief writes meta.run_brief on a cloud answer.
func markRunBrief(ctx context.Context, tx pgx.Tx, projectID int64, messageID string, value runBriefMeta) error {
	return markMessageMeta(ctx, tx, projectID, messageID, map[string]any{"run_brief": value})
}

// markMessageMeta merges fields into one answer's meta.
func markMessageMeta(ctx context.Context, tx pgx.Tx, projectID int64, messageID string, fields map[string]any) error {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return err
	}
	mark, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.chat_message_group
SET meta = COALESCE(meta, '{}'::jsonb) || $2::jsonb
WHERE uuid = $1::uuid`, schema), messageID, string(mark)); err != nil {
		return fmt.Errorf("run brief: mark the answer: %w", err)
	}
	return nil
}

// ChainBrief reads a chain's brief, its counts and its latest check.
func (r *RunChainsRepository) ChainBrief(ctx context.Context, root string) (runbrief.ChainBrief, bool, error) {
	return readChainBrief(ctx, r.pool, root)
}

func readChainBrief(ctx context.Context, queryer rowQueryer, root string) (runbrief.ChainBrief, bool, error) {
	chain := runbrief.ChainBrief{RootExecutionID: root}
	var brief, failures, last []byte
	err := queryer.QueryRow(ctx, `
SELECT c.project_id, c.owner_user_id, c.origin, c.brief_state, c.brief, c.goal_check_failures,
       (SELECT k.done_check FROM elitea_runtime.run_chain_links AS k
        WHERE k.root_execution_id = c.root_execution_id AND k.done_check IS NOT NULL
        ORDER BY k.continuation_index DESC LIMIT 1)
FROM elitea_runtime.run_chains AS c
WHERE c.root_execution_id = $1`, root).Scan(
		&chain.ProjectID, &chain.OwnerUserID, &chain.Origin, &chain.State, &brief, &failures, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return runbrief.ChainBrief{}, false, nil
	}
	if err != nil {
		return runbrief.ChainBrief{}, false, fmt.Errorf("run brief: read the chain: %w", err)
	}
	if len(brief) != 0 {
		chain.Brief = &runbrief.Brief{}
		if err := json.Unmarshal(brief, chain.Brief); err != nil || !chain.Brief.Valid() {
			return runbrief.ChainBrief{}, false, fmt.Errorf("run brief: the stored brief is not valid: %v", err)
		}
	}
	chain.Failures = map[string]int{}
	if len(failures) != 0 {
		if err := json.Unmarshal(failures, &chain.Failures); err != nil {
			return runbrief.ChainBrief{}, false, fmt.Errorf("run brief: decode the counts: %w", err)
		}
	}
	if len(last) != 0 {
		chain.LastCheck = &runbrief.Result{}
		if err := json.Unmarshal(last, chain.LastCheck); err != nil {
			return runbrief.ChainBrief{}, false, fmt.Errorf("run brief: decode the last check: %w", err)
		}
	}
	return chain, true, nil
}

// SaveBrief moves a pending brief to state once and answers the stored
// chain. A brief that is not pending stays as it is.
func (r *RunChainsRepository) SaveBrief(
	ctx context.Context, root, state string, brief *runbrief.Brief,
) (runbrief.ChainBrief, error) {
	var (
		raw    []byte
		digest *string
	)
	switch {
	case state == unattended.BriefReady && brief != nil:
		stored, sum, err := runbrief.Digest(*brief)
		if err != nil {
			return runbrief.ChainBrief{}, err
		}
		raw, digest = stored, &sum
	case state == unattended.BriefFailed || state == unattended.BriefOff:
		brief = nil
	default:
		return runbrief.ChainBrief{}, errors.New("run brief: a brief moves only to ready, failed or off")
	}
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var (
			origin   string
			project  int64
			response *string
		)
		err := tx.QueryRow(ctx, `
UPDATE elitea_runtime.run_chains AS c
SET brief = $3::jsonb, brief_sha256 = $4, brief_state = $2, updated_at = clock_timestamp()
WHERE c.root_execution_id = $1 AND c.brief_state = 'pending'
RETURNING c.origin, c.project_id,
          (SELECT k.response_message_id FROM elitea_runtime.run_chain_links AS k
           WHERE k.execution_id = c.root_execution_id)`,
			root, state, nullableJSON(raw), digest).Scan(&origin, &project, &response)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("run brief: save: %w", err)
		}
		if origin != unattended.OriginCloud || response == nil {
			return nil
		}
		return markRunBrief(ctx, tx, project, *response, runBriefMeta{State: state, Brief: brief})
	})
	if err != nil {
		return runbrief.ChainBrief{}, err
	}
	chain, found, err := r.ChainBrief(ctx, root)
	if err == nil && !found {
		err = errors.New("run brief: the chain is missing")
	}
	return chain, err
}

// LinkCheck answers whether executionID is a link of root, and its stored
// check.
func (r *RunChainsRepository) LinkCheck(
	ctx context.Context, root, executionID string,
) (*runbrief.Result, bool, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx, `
SELECT done_check FROM elitea_runtime.run_chain_links
WHERE execution_id = $2 AND root_execution_id = $1`, root, executionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("goal check: read the link: %w", err)
	}
	if len(raw) == 0 {
		return nil, true, nil
	}
	var result runbrief.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, true, fmt.Errorf("goal check: decode the stored check: %w", err)
	}
	return &result, true, nil
}

// RecordCheck writes a link's check and the chain's counts once. A second
// call answers the first check and changes nothing.
func (r *RunChainsRepository) RecordCheck(
	ctx context.Context, root, executionID string, result runbrief.Result, failures map[string]int,
) (runbrief.Result, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return runbrief.Result{}, err
	}
	if failures == nil {
		failures = map[string]int{}
	}
	counts, err := json.Marshal(failures)
	if err != nil {
		return runbrief.Result{}, err
	}
	stored := result
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var (
			response *string
			origin   string
			project  int64
		)
		err := tx.QueryRow(ctx, `
WITH linked AS (
    UPDATE elitea_runtime.run_chain_links
    SET done_check = $3::jsonb
    WHERE execution_id = $2 AND root_execution_id = $1 AND done_check IS NULL
    RETURNING root_execution_id, response_message_id
)
UPDATE elitea_runtime.run_chains AS c
SET goal_check_failures = $4::jsonb, updated_at = clock_timestamp()
FROM linked
WHERE c.root_execution_id = linked.root_execution_id
RETURNING linked.response_message_id, c.origin, c.project_id`,
			root, executionID, string(raw), string(counts)).Scan(&response, &origin, &project)
		if errors.Is(err, pgx.ErrNoRows) {
			var previous []byte
			if err := tx.QueryRow(ctx, `
SELECT done_check FROM elitea_runtime.run_chain_links
WHERE execution_id = $2 AND root_execution_id = $1`, root, executionID).Scan(&previous); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return runbrief.ErrNotLink
				}
				return fmt.Errorf("goal check: read the stored check: %w", err)
			}
			if len(previous) == 0 {
				return runbrief.ErrNotLink
			}
			return json.Unmarshal(previous, &stored)
		}
		if err != nil {
			return fmt.Errorf("goal check: record: %w", err)
		}
		if origin != unattended.OriginCloud || response == nil {
			return nil
		}
		return markMessageMeta(ctx, tx, project, *response, map[string]any{"goal_check": result.Public()})
	})
	return stored, err
}

func nullableJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// transcriptGroups bounds how many message groups a transcript reads.
const transcriptGroups = 200

// transcriptToolOutputRunes bounds each tool output in a transcript.
const transcriptToolOutputRunes = 1000

// RootRequest is the person's first message of a cloud chain: the text of
// the question that the root's answer replies to.
func (r *RunChainsRepository) RootRequest(ctx context.Context, root string) (string, error) {
	project, response, err := r.rootAnswer(ctx, root)
	if err != nil || response == "" {
		return "", err
	}
	schema, err := tenantSchema(strconv.FormatInt(project, 10))
	if err != nil {
		return "", err
	}
	var text string
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(string_agg(t.content, E'\n' ORDER BY i.order_index), '')
FROM %[1]s.chat_message_group AS answer
JOIN %[1]s.chat_message_group AS question ON question.id = answer.reply_to_id
JOIN %[1]s.chat_message_items AS i ON i.message_group_id = question.id
JOIN %[1]s.chat_messages_text AS t ON t.id = i.id
WHERE answer.uuid = $1::uuid`, schema), response).Scan(&text)
	if err != nil {
		return "", fmt.Errorf("run brief: read the request: %w", err)
	}
	return text, nil
}

// Transcript is a cloud chain's transcript from the root's question on,
// oldest first: each question's text, and each answer's tool calls and
// text. It reads at most transcriptGroups groups, newest first, and stops
// once it holds runbrief.MaxTranscriptBytes.
func (r *RunChainsRepository) Transcript(ctx context.Context, root string) (string, error) {
	project, response, err := r.rootAnswer(ctx, root)
	if err != nil || response == "" {
		return "", err
	}
	schema, err := tenantSchema(strconv.FormatInt(project, 10))
	if err != nil {
		return "", err
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
WITH anchor AS (
    SELECT conversation_id, COALESCE(reply_to_id, id) AS first_id
    FROM %[1]s.chat_message_group WHERE uuid = $1::uuid
)
SELECT g.reply_to_id IS NULL,
       COALESCE((SELECT string_agg('[tool ' || s.tool_name || '] ' || left(COALESCE(s.tool_output, ''), $3),
                                   E'\n' ORDER BY s.id)
                 FROM %[1]s.chat_message_trace_step AS s
                 WHERE s.message_group_id = g.id AND s.tool_name IS NOT NULL), ''),
       COALESCE((SELECT string_agg(t.content, E'\n' ORDER BY i.order_index)
                 FROM %[1]s.chat_message_items AS i
                 JOIN %[1]s.chat_messages_text AS t ON t.id = i.id
                 WHERE i.message_group_id = g.id), '')
FROM %[1]s.chat_message_group AS g, anchor
WHERE g.conversation_id = anchor.conversation_id AND g.id >= anchor.first_id
ORDER BY g.id DESC
LIMIT $2`, schema), response, transcriptGroups, transcriptToolOutputRunes)
	if err != nil {
		return "", fmt.Errorf("goal check: read the transcript: %w", err)
	}
	defer rows.Close()
	var (
		parts []string
		size  int
	)
	for rows.Next() && size < runbrief.MaxTranscriptBytes {
		var (
			question    bool
			tools, text string
		)
		if err := rows.Scan(&question, &tools, &text); err != nil {
			return "", fmt.Errorf("goal check: read the transcript: %w", err)
		}
		part := transcriptPart(question, tools, text)
		if part == "" {
			continue
		}
		parts = append(parts, part)
		size += len(part) + 2
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("goal check: read the transcript: %w", err)
	}
	slices.Reverse(parts)
	return strings.Join(parts, "\n\n"), nil
}

func transcriptPart(question bool, tools, text string) string {
	tools, text = strings.TrimSpace(tools), strings.TrimSpace(text)
	if question {
		if text == "" {
			return ""
		}
		return "User:\n" + text
	}
	body := strings.TrimSpace(tools + "\n" + text)
	if body == "" {
		return ""
	}
	return "Agent:\n" + body
}

// rootAnswer is a chain's project and its root's answer message.
func (r *RunChainsRepository) rootAnswer(ctx context.Context, root string) (int64, string, error) {
	var (
		project  int64
		response *string
	)
	err := r.pool.QueryRow(ctx, `
SELECT c.project_id, k.response_message_id
FROM elitea_runtime.run_chains AS c
JOIN elitea_runtime.run_chain_links AS k ON k.execution_id = c.root_execution_id
WHERE c.root_execution_id = $1`, root).Scan(&project, &response)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("run brief: read the root: %w", err)
	}
	if response == nil {
		return project, "", nil
	}
	return project, *response, nil
}
