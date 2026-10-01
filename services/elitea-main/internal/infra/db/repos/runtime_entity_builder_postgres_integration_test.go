package repos

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRuntimeEntityBuilderPostgresSkillCreateAndUpdateKeepOwnerAndAuthorSeparate(t *testing.T) {
	pool := newSkillsTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	repo, err := NewCurrentRuntimeEntityBuilderRepository(pool)
	require.NoError(t, err)

	created, err := repo.UpsertRuntimeSkillByName(ctx, 1, 11, "Runtime Skill", "Initial description", "Initial instructions")
	require.NoError(t, err)
	require.True(t, created.Created)
	require.NotEmpty(t, created.SkillID)
	assertRuntimeSkillAuthor(t, ctx, repo, created.SkillID, 11, "Initial instructions")

	// An update keeps the original authors and existing version tags.
	_, err = pool.Exec(ctx, `
		WITH tag AS (INSERT INTO p_1.tags (name) VALUES ('retained') RETURNING id)
		INSERT INTO p_1.skill_version_tag_association (version_id, tag_id)
		SELECT sv.id, tag.id FROM p_1.skill_versions sv, tag
		WHERE sv.skill_id = $1 AND sv.name = 'base'`, created.SkillID)
	require.NoError(t, err)
	updated, err := repo.UpsertRuntimeSkillByName(ctx, 1, 17, "Runtime Skill", "Updated description", "Updated instructions")
	require.NoError(t, err)
	require.False(t, updated.Created)
	require.Equal(t, created.SkillID, updated.SkillID)
	assertRuntimeSkillAuthor(t, ctx, repo, updated.SkillID, 11, "Updated instructions")
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM p_1.skills WHERE name = 'Runtime Skill'`).Scan(&count))
	require.Equal(t, 1, count)
	var tagName string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT t.name FROM p_1.tags t
		JOIN p_1.skill_version_tag_association a ON a.tag_id = t.id
		JOIN p_1.skill_versions sv ON sv.id = a.version_id
		WHERE sv.skill_id = $1 AND sv.name = 'base'`, updated.SkillID).Scan(&tagName))
	require.Equal(t, "retained", tagName)
}

func assertRuntimeSkillAuthor(
	t *testing.T,
	ctx context.Context,
	repo *CurrentRuntimeEntityBuilderRepository,
	skillID string,
	authorID int64,
	instructions string,
) {
	t.Helper()
	var owner, skillAuthor, versionAuthor int64
	var storedInstructions string
	require.NoError(t, repo.pool.QueryRow(ctx, `
		SELECT sk.owner_id, sk.author_id, sv.author_id, sv.instructions
		FROM p_1.skills sk JOIN p_1.skill_versions sv ON sv.skill_id = sk.id
		WHERE sk.id = $1 AND sv.name = 'base'`, skillID).
		Scan(&owner, &skillAuthor, &versionAuthor, &storedInstructions))
	require.Equal(t, int64(1), owner)
	require.Equal(t, authorID, skillAuthor)
	require.Equal(t, authorID, versionAuthor)
	require.Equal(t, instructions, storedInstructions)
}

func TestRuntimeEntityBuilderRejectsInvalidSkillAuthorBeforeOpeningStorage(t *testing.T) {
	t.Parallel()
	for _, actorID := range []int64{0, -1, 1 << 31} {
		t.Run(strconv.FormatInt(actorID, 10), func(t *testing.T) {
			t.Parallel()
			repo := &CurrentRuntimeEntityBuilderRepository{}
			_, err := repo.UpsertRuntimeSkillByName(t.Context(), 1, actorID, "Name", "Description", "Instructions")
			require.Error(t, err)
		})
	}
}
