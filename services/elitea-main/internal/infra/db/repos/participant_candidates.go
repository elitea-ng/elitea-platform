package repos

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

// ParticipantCandidatesRepo answers the people a caller may add to a
// conversation (client contract 1.1, listParticipantCandidates): the members
// of the project.
//
// The population is the one ChatMentionNotificationRepo.ProjectMemberUserIDs
// resolves `@everyone` to — auth_core__project_user_role, platform system
// users (`%@centry.user`) excluded — so the picker offers exactly the people a
// mention can reach. Suspended accounts are excluded too: they cannot sign in
// to read the conversation they would be added to.
type ParticipantCandidatesRepo struct {
	pool *pgxpool.Pool
}

func NewParticipantCandidatesRepo(pool *pgxpool.Pool) *ParticipantCandidatesRepo {
	return &ParticipantCandidatesRepo{pool: pool}
}

// candidateSortKey is lower(display name), falling back to the e-mail when
// the name is empty. The cursor carries the same expression's value, so the
// keyset predicate and the ORDER BY can never disagree.
const candidateSortKey = `lower(coalesce(nullif(account.name, ''), account.email, ''))`

// ListProjectMemberCandidates implements conversations.ParticipantCandidateStore.
func (repo *ParticipantCandidatesRepo) ListProjectMemberCandidates(
	ctx context.Context,
	projectID int64,
	query string,
	after *conversations.CandidateCursor,
	limit int,
) ([]conversations.ParticipantCandidate, []string, error) {
	if repo == nil || repo.pool == nil {
		return nil, nil, fmt.Errorf("participant candidates: no database")
	}
	args := []any{projectID}
	conditions := []string{
		"member.project_id = $1",
		"account.email NOT LIKE '%@centry.user'",
		"account.suspended IS NOT TRUE",
	}
	if query != "" {
		// The needle is matched literally: LIKE metacharacters in it are
		// escaped, so "%" finds a "%" rather than every member.
		args = append(args, "%"+escapeLike(strings.ToLower(query))+"%")
		position := len(args)
		conditions = append(conditions, fmt.Sprintf(
			`(lower(coalesce(account.name, '')) LIKE $%[1]d ESCAPE '\' OR lower(coalesce(account.email, '')) LIKE $%[1]d ESCAPE '\')`,
			position))
	}
	if after != nil {
		args = append(args, after.SortKey, after.UserID)
		conditions = append(conditions, fmt.Sprintf("(%s, account.id) > ($%d, $%d)", candidateSortKey, len(args)-1, len(args)))
	}
	args = append(args, limit)
	statement := fmt.Sprintf(`
SELECT DISTINCT ON (%[1]s, account.id)
       account.id, coalesce(account.name, ''), coalesce(account.email, ''), %[1]s
FROM public.auth_core__project_user_role AS member
JOIN public.auth_core__user AS account ON account.id = member.user_id
WHERE %[2]s
ORDER BY %[1]s, account.id
LIMIT $%[3]d`, candidateSortKey, strings.Join(conditions, " AND "), len(args))

	rows, err := repo.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("participant candidates of project %d: %w", projectID, err)
	}
	defer rows.Close()
	candidates := make([]conversations.ParticipantCandidate, 0, limit)
	keys := make([]string, 0, limit)
	for rows.Next() {
		var candidate conversations.ParticipantCandidate
		var key string
		if err := rows.Scan(&candidate.UserID, &candidate.Name, &candidate.Email, &key); err != nil {
			return nil, nil, fmt.Errorf("scan participant candidate: %w", err)
		}
		candidates = append(candidates, candidate)
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("participant candidates of project %d: %w", projectID, err)
	}
	return candidates, keys, nil
}

func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

var _ conversations.ParticipantCandidateStore = (*ParticipantCandidatesRepo)(nil)
