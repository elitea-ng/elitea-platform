package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/conversationactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/chatactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// ConversationActivityRepo reads the committed state a conversation activity
// frame carries (client contract 1.12): the conversation's list-row activity
// and the audience that may see it, by the visibility rule of the list
// (chatauthority.Predicate, Listing):
//
//   - a public conversation: every member who may list conversations
//     (Audience.Project; the SSE stream checks the permission);
//   - a private one: its user participants, and the project admins when it
//     has at least one user participant (the admin listing rule).
type ConversationActivityRepo struct {
	pool *pgxpool.Pool
}

var _ conversationactivity.Store = (*ConversationActivityRepo)(nil)

func NewConversationActivityRepo(pool *pgxpool.Pool) *ConversationActivityRepo {
	if pool == nil {
		return nil
	}
	return &ConversationActivityRepo{pool: pool}
}

// ConversationByUUID reads the conversation with that uuid.
func (r *ConversationActivityRepo) ConversationByUUID(ctx context.Context, projectID int64, conversationUUID string) (conversationactivity.Snapshot, bool, error) {
	if !validUUIDText(conversationUUID) {
		return conversationactivity.Snapshot{}, false, nil
	}
	return r.snapshot(ctx, projectID, "c.uuid = $1::uuid", conversationUUID)
}

// ConversationByMessage reads the conversation that holds the message group.
func (r *ConversationActivityRepo) ConversationByMessage(ctx context.Context, projectID int64, messageUUID string) (conversationactivity.Snapshot, bool, error) {
	if !validUUIDText(messageUUID) {
		return conversationactivity.Snapshot{}, false, nil
	}
	s, err := tenantschema.Quote(strconv.FormatInt(projectID, 10))
	if err != nil {
		return conversationactivity.Snapshot{}, false, err
	}
	return r.snapshot(ctx, projectID, fmt.Sprintf(
		"c.id = (SELECT g.conversation_id FROM %s.chat_message_group g WHERE g.uuid = $1::uuid)", s), messageUUID)
}

// ConversationByExecution reads the conversation an agent execution answers
// in: its binding's client stream (the conversation uuid) and its job's
// projection project.
func (r *ConversationActivityRepo) ConversationByExecution(ctx context.Context, executionID string) (conversationactivity.Snapshot, bool, error) {
	var stream string
	var projectID int64
	err := r.pool.QueryRow(ctx, `SELECT binding.client_stream_id, job.projection_project_id
		FROM elitea_runtime.agent_execution_jobs binding
		JOIN elitea_runtime.execution_jobs job
		  ON job.execution_id = binding.execution_id AND job.generation = binding.generation
		WHERE binding.execution_id = $1
		ORDER BY binding.generation DESC LIMIT 1`, executionID).Scan(&stream, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversationactivity.Snapshot{}, false, nil
	}
	if err != nil {
		return conversationactivity.Snapshot{}, false, fmt.Errorf("conversation activity: execution: %w", err)
	}
	return r.ConversationByUUID(ctx, projectID, stream)
}

// snapshot reads one conversation selected by where (alias c, $1 bound to
// arg) with its audience and activity.
func (r *ConversationActivityRepo) snapshot(ctx context.Context, projectID int64, where string, arg any) (conversationactivity.Snapshot, bool, error) {
	project := strconv.FormatInt(projectID, 10)
	s, err := tenantschema.Quote(project)
	if err != nil {
		return conversationactivity.Snapshot{}, false, err
	}
	var (
		id            int64
		uuid, source  string
		private       bool
		hidden        bool
		users         []int64
		hasUserMember bool
	)
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`SELECT c.id, COALESCE(c.uuid::text, ''), COALESCE(c.source, ''), c.is_private,
		COALESCE(c.meta->>'is_hidden', '') = 'true',
		ARRAY(SELECT DISTINCT (p.entity_meta->>'id')::bigint
		        FROM %[1]s.chat_participant_mapping m
		        JOIN %[1]s.chat_participants p ON p.id = m.participant_id
		       WHERE m.conversation_id = c.id AND p.entity_name = 'user'
		         AND p.entity_meta->>'id' ~ '^[0-9]{1,18}$'),
		EXISTS (SELECT 1 FROM %[1]s.chat_participant_mapping m
		        JOIN %[1]s.chat_participants p ON p.id = m.participant_id
		       WHERE m.conversation_id = c.id AND p.entity_name = 'user')
		FROM %[1]s.chat_conversations c WHERE %[2]s`, s, where), arg).
		Scan(&id, &uuid, &source, &private, &hidden, &users, &hasUserMember)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversationactivity.Snapshot{}, false, nil
	}
	if err != nil {
		return conversationactivity.Snapshot{}, false, fmt.Errorf("conversation activity: conversation: %w", err)
	}
	audience := conversationactivity.Audience{Project: !private}
	if private {
		audience.Users = users
		if hasUserMember {
			admins, err := r.projectAdmins(ctx, projectID)
			if err != nil {
				return conversationactivity.Snapshot{}, false, err
			}
			audience.Users = mergeUserIDs(audience.Users, admins)
		}
	}
	activity, err := chatactivity.Load(ctx, r.pool, s, 0, []int64{id})
	if err != nil {
		return conversationactivity.Snapshot{}, false, err
	}
	a, ok := activity[id]
	if !ok {
		return conversationactivity.Snapshot{}, false, nil
	}
	return conversationactivity.Snapshot{
		ProjectID: projectID,
		Audience:  audience,
		Hidden:    hidden,
		Frame: conversationactivity.Frame{
			ConversationID:   id,
			ConversationUUID: uuid,
			Source:           source,
			State:            a.State,
			Since:            chatactivity.FormatTime(a.Since),
			LastActivityAt:   chatactivity.FormatTime(a.LastActivityAt),
		},
	}, true, nil
}

// projectAdmins reads the project's admins, by the role rule chatauthority
// applies. An absent role projection has no admins.
func (r *ConversationActivityRepo) projectAdmins(ctx context.Context, projectID int64) ([]int64, error) {
	var present bool
	if err := r.pool.QueryRow(ctx, `SELECT to_regclass('public.auth_core__project_user_role') IS NOT NULL
		AND to_regclass('public.auth_core__project_role') IS NOT NULL`).Scan(&present); err != nil {
		return nil, fmt.Errorf("conversation activity: role projection: %w", err)
	}
	if !present {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT DISTINCT assignment.user_id::bigint
		FROM public.auth_core__project_user_role assignment
		JOIN public.auth_core__project_role role ON role.id = assignment.role_id AND role.project_id = assignment.project_id
		WHERE assignment.project_id = $1 AND lower(role.name) IN ('admin', 'super_admin')`, projectID)
	if err != nil {
		return nil, fmt.Errorf("conversation activity: admins: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func mergeUserIDs(a, b []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(a)+len(b))
	for _, list := range [][]int64{a, b} {
		for _, id := range list {
			if id > 0 && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}
