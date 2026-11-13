package repos

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// diagnoseCurrentApplicationTurnSQL answers WHICH condition of
// ResolveCurrentApplicationTurn (agent_chat.sql) made it return no row.
//
// That query is one statement with about twenty-five ways to match nothing,
// and an empty result says none of them. The people who hit it see a 422 and
// cannot read the server log, so a refusal used to be unanswerable. This runs
// only AFTER a refusal, inside the same read-only transaction, so a healthy
// start never pays for it.
//
// The branches are ORDERED the way a person would debug it: is the
// conversation there, is the caller in it, is the agent in it, is the agent
// usable from this project, and only then the conversation-state gates. The
// first branch that is true names the reason; the codes map to fixed sentences
// below. A branch must stay in step with the same condition of the resolver --
// the integration test seeds each one.
//
// Parameters: $1 conversation uuid, $2 caller user id, $3 target participant
// id, $4 conversation's project id, $5 catalogue (public) project id,
// $6 question uuid, $7 the authorable internal tool names
// (currentAuthorableInternalTools, the list the resolver SQL also carries; a
// test keeps the two equal).
//
// The conversation is looked up by uuid alone because chat_conversations is a
// TENANT table: the transaction is bound to the caller's project, so a
// conversation of another project is not visible here and reads as missing.
const diagnoseCurrentApplicationTurnSQL = `
WITH conversation AS (
    SELECT c.id, c.meta
    FROM chat_conversations AS c
    WHERE c.uuid = $1::uuid
),
target AS (
    SELECT p.id AS participant_id,
           p.entity_name,
           CASE WHEN p.entity_meta ->> 'project_id' ~ '^[0-9]+$'
                THEN (p.entity_meta ->> 'project_id')::bigint END AS project_id,
           CASE WHEN p.entity_meta ->> 'id' ~ '^[0-9]+$'
                THEN (p.entity_meta ->> 'id')::bigint END AS application_id,
           CASE WHEN m.entity_settings ->> 'version_id' ~ '^[0-9]+$'
                THEN (m.entity_settings ->> 'version_id')::bigint END AS version_id
    FROM conversation AS c
    JOIN chat_participant_mapping AS m
      ON m.conversation_id = c.id AND m.participant_id = $3::integer
    JOIN chat_participants AS p ON p.id = m.participant_id
)
SELECT CASE
    WHEN NOT EXISTS (SELECT 1 FROM conversation)
        THEN 'conversation_missing'
    WHEN NOT EXISTS (
        SELECT 1
        FROM conversation AS c
        JOIN chat_participant_mapping AS m ON m.conversation_id = c.id
        JOIN chat_participants AS p
          ON p.id = m.participant_id AND p.entity_name = 'user'
         AND p.entity_meta ->> 'id' = $2::bigint::text
    ) THEN 'caller_not_participant'
    WHEN NOT EXISTS (SELECT 1 FROM target)
        THEN 'agent_not_in_conversation'
    WHEN NOT EXISTS (SELECT 1 FROM target WHERE entity_name = 'application')
        THEN 'participant_not_agent'
    WHEN EXISTS (
        SELECT 1 FROM target
        WHERE project_id IS DISTINCT FROM $4::bigint
          AND project_id IS DISTINCT FROM $5::bigint
    ) THEN 'foreign_project'
    WHEN EXISTS (
        SELECT 1 FROM target
        WHERE project_id = $4::bigint
          AND NOT EXISTS (
              SELECT 1 FROM application_versions AS v
              WHERE v.id = target.version_id AND v.application_id = target.application_id
          )
    ) THEN 'version_missing'
    WHEN EXISTS (
        SELECT 1
        FROM conversation AS c,
             LATERAL jsonb_array_elements(
                 CASE WHEN jsonb_typeof(c.meta -> 'internal_tools') = 'array'
                      THEN c.meta -> 'internal_tools' ELSE '[]'::jsonb END
             ) AS tool(value)
        WHERE jsonb_typeof(tool.value) <> 'string'
           OR tool.value #>> '{}' <> ALL ($7::text[])
    ) OR EXISTS (
        SELECT 1 FROM conversation AS c
        WHERE c.meta ? 'internal_tools' AND jsonb_typeof(c.meta -> 'internal_tools') <> 'array'
    ) THEN 'conversation_internal_tools'
    WHEN EXISTS (
        SELECT 1
        FROM target
        JOIN application_versions AS v ON v.id = target.version_id
        CROSS JOIN LATERAL jsonb_array_elements(
            CASE WHEN jsonb_typeof(v.meta::jsonb -> 'internal_tools') = 'array'
                 THEN v.meta::jsonb -> 'internal_tools' ELSE '[]'::jsonb END
        ) AS tool(value)
        WHERE jsonb_typeof(tool.value) <> 'string'
           OR tool.value #>> '{}' <> ALL ($7::text[])
    ) OR EXISTS (
        SELECT 1
        FROM target
        JOIN application_versions AS v ON v.id = target.version_id
        WHERE v.meta::jsonb ? 'internal_tools'
          AND jsonb_typeof(v.meta::jsonb -> 'internal_tools') <> 'array'
    ) THEN 'agent_internal_tools'
    WHEN EXISTS (
        SELECT 1 FROM conversation AS c
        WHERE COALESCE(c.meta #>> '{context_analytics,last_summarization,summary_content}', '') <> ''
    ) THEN 'conversation_summarized'
    WHEN EXISTS (
        SELECT 1
        FROM conversation AS c
        JOIN chat_participant_mapping AS m ON m.conversation_id = c.id
        JOIN chat_participants AS p
          ON p.id = m.participant_id AND p.entity_name = 'toolkit'
    ) THEN 'toolkit_participant'
    WHEN EXISTS (
        SELECT 1
        FROM conversation AS c
        JOIN chat_message_group AS pending ON pending.conversation_id = c.id
        WHERE pending.is_streaming
          AND NOT EXISTS (
              SELECT 1 FROM chat_message_group AS newer
              WHERE newer.conversation_id = pending.conversation_id
                AND newer.reply_to_id IS NOT NULL
                AND (newer.created_at > pending.created_at
                     OR (newer.created_at = pending.created_at AND newer.id > pending.id))
          )
          AND NOT EXISTS (
              SELECT 1 FROM chat_message_group AS retried
              WHERE retried.id = pending.reply_to_id AND retried.uuid = $6::uuid
          )
    ) THEN 'answer_in_progress'
    WHEN EXISTS (
        SELECT 1
        FROM conversation AS c
        JOIN chat_message_group AS g ON g.conversation_id = c.id
        JOIN chat_message_items AS i ON i.message_group_id = g.id
        WHERE i.item_type = 'context_message'
    ) THEN 'context_message'
    ELSE 'unknown'
END
`

// currentApplicationTurnRefusal is the error for a resolve that found no row.
// A diagnosis that itself fails (or finds nothing) falls back to the generic
// reason: naming the wrong cause is worse than naming none.
//
// The diagnostic runs inside a SAVEPOINT: a failing statement aborts the
// surrounding transaction (SQLSTATE 25P02) for every later statement, and the
// caller's transaction must survive a diagnostic that cannot run. The failure
// is logged (error only, no conversation data).
func currentApplicationTurnRefusal(
	ctx context.Context,
	tx sqlExecutor,
	conversationUUID pgtype.UUID,
	actorUserID int64,
	targetParticipantID int32,
	projectID int32,
	catalogueProjectID int32,
	questionID pgtype.UUID,
) error {
	code, err := runTurnDiagnosis(ctx, tx, conversationUUID, actorUserID, targetParticipantID,
		projectID, catalogueProjectID, questionID)
	if err != nil {
		slog.WarnContext(ctx, "agent start: refusal diagnosis failed", "err", err)
		return agentexecutionapp.UnsupportedStartRefused(agentexecutionapp.RefusalNoTurnRow)
	}
	refusal := agentexecutionapp.StartRefusal(code)
	if agentexecutionapp.StartRefusalSentence(refusal) != "" {
		return agentexecutionapp.UnsupportedStartRefused(refusal)
	}
	return agentexecutionapp.UnsupportedStartRefused(agentexecutionapp.RefusalNoTurnRow)
}

func runTurnDiagnosis(
	ctx context.Context,
	tx sqlExecutor,
	conversationUUID pgtype.UUID,
	actorUserID int64,
	targetParticipantID int32,
	projectID int32,
	catalogueProjectID int32,
	questionID pgtype.UUID,
) (string, error) {
	const savepoint = "agent_start_diagnose"
	if _, err := tx.Exec(ctx, "SAVEPOINT "+savepoint); err != nil {
		return "", err
	}
	var code string
	err := tx.QueryRow(
		ctx, diagnoseCurrentApplicationTurnSQL,
		conversationUUID, actorUserID, targetParticipantID,
		int64(projectID), int64(catalogueProjectID), questionID,
		authorableInternalToolNames(),
	).Scan(&code)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		if _, rollbackErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); rollbackErr != nil {
			slog.WarnContext(ctx, "agent start: diagnosis savepoint rollback failed", "err", rollbackErr)
		}
		return "", err
	}
	if _, releaseErr := tx.Exec(ctx, "RELEASE SAVEPOINT "+savepoint); releaseErr != nil {
		return "", releaseErr
	}
	return code, nil
}

// authorableInternalToolNames is the resolver's allowlist, sorted, as the
// diagnosis parameter.
func authorableInternalToolNames() []string {
	names := make([]string, 0, len(currentAuthorableInternalTools))
	for name := range currentAuthorableInternalTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
