package repos

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/memories"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// MemoriesRepo backs BOTH persistent personal memory's CRUD
// (memories.Repository, the Settings > Memory panel — #870) and its RECALL
// into a chat turn (agentexecutionapp.CurrentMemoryRecallResolver, the
// runtime context-injection path). One struct, two interfaces: recall reads
// the exact rows CRUD writes, over the same tenant table
// (personal_memory_entries, tenant/0137_personal_memory_entries.sql), so a
// second repository type would only be a second copy of the same SQL.
type MemoriesRepo struct {
	pool *pgxpool.Pool
}

func NewMemoriesRepo(pool *pgxpool.Pool) *MemoriesRepo {
	return &MemoriesRepo{pool: pool}
}

var (
	_ memories.Repository                           = (*MemoriesRepo)(nil)
	_ agentexecutionapp.CurrentMemoryRecallResolver = (*MemoriesRepo)(nil)
)

const memoryColumns = `id::text, content, tags, COALESCE(source_conversation_uuid::text, ''), enabled, created_at, updated_at`

func scanMemoryEntry(row pgx.Row) (memories.MemoryEntry, error) {
	var entry memories.MemoryEntry
	if err := row.Scan(
		&entry.ID, &entry.Content, &entry.Tags, &entry.SourceConversationID,
		&entry.Enabled, &entry.CreatedAt, &entry.UpdatedAt,
	); err != nil {
		return memories.MemoryEntry{}, err
	}
	if entry.Tags == nil {
		entry.Tags = []string{}
	}
	return entry, nil
}

// List answers the caller's own memories, most recent first. search is
// applied in GO, not SQL — same call `folders.containsInsensitive` already
// makes (folders/handler.go) — over a per-user row count small enough
// (personal notes, not a shared corpus) that a substring scan costs nothing
// and this sidesteps ILIKE wildcard-escaping entirely.
func (r *MemoriesRepo) List(ctx context.Context, projectID, userID, search string) ([]memories.MemoryEntry, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT %s FROM %s.personal_memory_entries WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, memoryColumns, s)
	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("memories: list: %w", err)
	}
	defer rows.Close()

	result := make([]memories.MemoryEntry, 0)
	for rows.Next() {
		entry, err := scanMemoryEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("memories: scan: %w", err)
		}
		entry.ProjectID = projectID
		if search == "" || matchesMemorySearch(entry, search) {
			result = append(result, entry)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memories: list rows: %w", err)
	}
	return result, nil
}

func matchesMemorySearch(entry memories.MemoryEntry, search string) bool {
	needle := strings.ToLower(search)
	if strings.Contains(strings.ToLower(entry.Content), needle) {
		return true
	}
	for _, tag := range entry.Tags {
		if strings.Contains(strings.ToLower(tag), needle) {
			return true
		}
	}
	return false
}

func (r *MemoriesRepo) Create(ctx context.Context, projectID, userID string, entry memories.MemoryEntry) (memories.MemoryEntry, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return memories.MemoryEntry{}, err
	}
	source := parsedMemorySourceUUID(entry.SourceConversationID)
	q := fmt.Sprintf(`
		INSERT INTO %s.personal_memory_entries (user_id, content, tags, source_conversation_uuid, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now(), now())
		RETURNING %s`, s, memoryColumns)
	created, err := scanMemoryEntry(r.pool.QueryRow(ctx, q, userID, entry.Content, nonNilMemoryTags(entry.Tags), source, entry.Enabled))
	if err != nil {
		return memories.MemoryEntry{}, fmt.Errorf("memories: create: %w", err)
	}
	created.ProjectID = projectID
	return created, nil
}

func (r *MemoriesRepo) Update(ctx context.Context, projectID, userID, memoryID string, entry memories.MemoryEntry) (memories.MemoryEntry, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return memories.MemoryEntry{}, err
	}
	if !isNumericRowID(memoryID) {
		return memories.MemoryEntry{}, apierr.NotFound("memory not found")
	}
	source := parsedMemorySourceUUID(entry.SourceConversationID)
	q := fmt.Sprintf(`
		UPDATE %s.personal_memory_entries
		SET content = $3, tags = $4, source_conversation_uuid = $5, enabled = $6, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING %s`, s, memoryColumns)
	updated, err := scanMemoryEntry(r.pool.QueryRow(ctx, q, memoryID, userID, entry.Content, nonNilMemoryTags(entry.Tags), source, entry.Enabled))
	if errors.Is(err, pgx.ErrNoRows) {
		return memories.MemoryEntry{}, apierr.NotFound("memory not found")
	}
	if err != nil {
		return memories.MemoryEntry{}, fmt.Errorf("memories: update: %w", err)
	}
	updated.ProjectID = projectID
	return updated, nil
}

func (r *MemoriesRepo) Delete(ctx context.Context, projectID, userID, memoryID string) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	if !isNumericRowID(memoryID) {
		// Deleting a memory that never existed is the same observable
		// outcome as deleting one that did and is now gone — DELETE's usual
		// idempotent shape, matching DeleteMessageFeedback's own comment.
		return nil
	}
	q := fmt.Sprintf(`DELETE FROM %s.personal_memory_entries WHERE id = $1 AND user_id = $2`, s)
	if _, err := r.pool.Exec(ctx, q, memoryID, userID); err != nil {
		return fmt.Errorf("memories: delete: %w", err)
	}
	return nil
}

func (r *MemoriesRepo) ClearAll(ctx context.Context, projectID, userID string) (int, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return 0, err
	}
	q := fmt.Sprintf(`DELETE FROM %s.personal_memory_entries WHERE user_id = $1`, s)
	tag, err := r.pool.Exec(ctx, q, userID)
	if err != nil {
		return 0, fmt.Errorf("memories: clear all: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// nonNilMemoryTags coalesces a nil Go slice to an empty one before binding
// to `tags`: pgx binds a nil []string as SQL NULL, and the column is
// `NOT NULL DEFAULT '{}'` (tenant/0136) — a caller that never sets Tags
// (memories.MemoryEntry's zero value) must still satisfy that constraint,
// not 500 on it.
func nonNilMemoryTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

func parsedMemorySourceUUID(raw string) *string {
	if raw == "" {
		return nil
	}
	if _, err := uuid.Parse(raw); err != nil {
		// An unparsable source id is treated as "no known source" rather
		// than a request error — this field is informational (tenant/0136's
		// header) and must never block saving the memory itself.
		return nil
	}
	return &raw
}

// ── Recall (agentexecutionapp.CurrentMemoryRecallResolver) ─────────────────
//
// This half of the file answers the OTHER question: given a project, the
// acting user, and the text they just sent, which of their ENABLED memories
// (if any) belong in this turn's prompt. See
// internal/application/agentexecution/memories.go for how the answer is
// spliced into the `instructions` text both workers already consume, and why
// that mechanism was chosen over a new wire field.

const (
	// currentMemoryRecallPoolSize bounds how many of a user's most-recent
	// enabled memories are even considered for one turn — a ceiling on the
	// read, independent of how many are ultimately injected.
	currentMemoryRecallPoolSize = 200
	// currentMemoryRecallMaxEntries and currentMemoryRecallMaxChars bound
	// what actually reaches the prompt: at most this many entries, and at
	// most this much text, so a user with hundreds of memories never turns
	// one turn's context window into a memory dump.
	currentMemoryRecallMaxEntries = 8
	currentMemoryRecallMaxChars   = 2000
)

func (r *MemoriesRepo) ResolveCurrentMemoryRecall(
	ctx context.Context,
	projectID, actorUserID int64,
	userInput string,
) (agentexecutionapp.CurrentMemoryRecall, error) {
	s, err := tenantSchema(fmt.Sprintf("%d", projectID))
	if err != nil {
		return agentexecutionapp.CurrentMemoryRecall{}, err
	}
	q := fmt.Sprintf(`
		SELECT id::text, content, tags
		FROM %s.personal_memory_entries
		WHERE user_id = $1 AND enabled = TRUE
		ORDER BY created_at DESC, id DESC
		LIMIT %d`, s, currentMemoryRecallPoolSize)
	rows, err := r.pool.Query(ctx, q, fmt.Sprintf("%d", actorUserID))
	if err != nil {
		return agentexecutionapp.CurrentMemoryRecall{}, fmt.Errorf("memories: resolve recall: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id      string
		content string
		rank    int // position in the most-recent-first read; lower is more recent
	}
	candidates := make([]candidate, 0, currentMemoryRecallPoolSize)
	for rows.Next() {
		var id, content string
		var tags []string
		if err := rows.Scan(&id, &content, &tags); err != nil {
			return agentexecutionapp.CurrentMemoryRecall{}, fmt.Errorf("memories: scan recall row: %w", err)
		}
		candidates = append(candidates, candidate{id: id, content: content, rank: len(candidates)})
		_ = tags // scored via keywordOverlapScore below, which reads content only
	}
	if err := rows.Err(); err != nil {
		return agentexecutionapp.CurrentMemoryRecall{}, fmt.Errorf("memories: recall rows: %w", err)
	}
	if len(candidates) == 0 {
		return agentexecutionapp.CurrentMemoryRecall{}, nil
	}

	// No embedding index plane is wired for personal memory (#870's own
	// scope note: an embedding-backed match would need every memory
	// indexed into pgvector on write, which is a materially larger change
	// than this issue's recall path). Keyword overlap against the user's
	// current turn, THEN recency, is the fallback the issue asked for.
	keywords := memoryKeywordSet(userInput)
	type scored struct {
		candidate
		score int
	}
	pool := make([]scored, len(candidates))
	for i, c := range candidates {
		pool[i] = scored{candidate: c, score: keywordOverlapScore(c.content, keywords)}
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].score != pool[j].score {
			return pool[i].score > pool[j].score
		}
		return pool[i].rank < pool[j].rank // tie: most recent first
	})

	var (
		selected []string
		ids      []string
		budget   = currentMemoryRecallMaxChars
	)
	for _, c := range pool {
		if len(selected) >= currentMemoryRecallMaxEntries || budget <= 0 {
			break
		}
		text := c.content
		if len(text) > budget {
			text = text[:budget]
		}
		selected = append(selected, text)
		ids = append(ids, c.id)
		budget -= len(text)
	}
	if len(selected) == 0 {
		return agentexecutionapp.CurrentMemoryRecall{}, nil
	}

	var builder strings.Builder
	builder.WriteString("What you remember about this user from earlier conversations:\n")
	for _, text := range selected {
		builder.WriteString("- ")
		builder.WriteString(text)
		builder.WriteString("\n")
	}
	return agentexecutionapp.CurrentMemoryRecall{
		Text:  strings.TrimRight(builder.String(), "\n"),
		Count: len(selected),
		IDs:   ids,
	}, nil
}

// RecordCurrentMemoryUsage stamps how many memories a turn actually used
// onto the RESPONSE message group's own `meta` (chat_message_group.meta) —
// the SAME per-message metadata bag entities/message/lib/wire.ts already
// decodes arbitrary keys from (thinking_steps, tool_calls, context, ...), so
// no new API surface is needed for the web's "Using N memories" indicator to
// read it back off the ordinary GetMessage/ListMessages response.
//
// Best-effort BY DESIGN: called AFTER the turn has already been admitted
// (see start.go/adhoc.go's own comments), so a failure here must never be
// reported as a turn failure — the caller logs and moves on. A merge
// (`meta || jsonb_build_object(...)`) rather than an overwrite, so this
// never clobbers a key the admission write, or the worker's own later
// stream projection, already set.
func (r *MemoriesRepo) RecordCurrentMemoryUsage(
	ctx context.Context,
	projectID int64,
	responseMessageID string,
	count int,
) error {
	if count <= 0 {
		return nil
	}
	s, err := tenantSchema(fmt.Sprintf("%d", projectID))
	if err != nil {
		return err
	}
	q := fmt.Sprintf(`
		UPDATE %s.chat_message_group
		SET meta = meta || jsonb_build_object('memories_used', $2::int)
		WHERE uuid = $1::uuid`, s)
	if _, err := r.pool.Exec(ctx, q, responseMessageID, count); err != nil {
		return fmt.Errorf("memories: record usage: %w", err)
	}
	return nil
}

// memoryKeywordSet lowercases and tokenizes the user's turn into a set of
// words 3 runes or longer — short words ("the", "a", "is") are noise for
// overlap scoring and would make nearly every memory "match".
func memoryKeywordSet(userInput string) map[string]struct{} {
	words := strings.FieldsFunc(strings.ToLower(userInput), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	set := make(map[string]struct{}, len(words))
	for _, w := range words {
		if len(w) >= 3 {
			set[w] = struct{}{}
		}
	}
	return set
}

func keywordOverlapScore(content string, keywords map[string]struct{}) int {
	if len(keywords) == 0 {
		return 0
	}
	contentWords := strings.FieldsFunc(strings.ToLower(content), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	score := 0
	seen := make(map[string]struct{}, len(contentWords))
	for _, w := range contentWords {
		if _, already := seen[w]; already {
			continue
		}
		seen[w] = struct{}{}
		if _, ok := keywords[w]; ok {
			score++
		}
	}
	return score
}
