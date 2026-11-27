package repos

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// AgentMemorySeedWriter is the querier a memory seed is written through: the
// import's pool, or the team import's one transaction (a pgx.Tx), so a refused
// team keeps none of its seeds.
type AgentMemorySeedWriter interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AgentMemorySeed is one `memory_seeds` entry of an imported agent file
// (issue 3137): what the author wants the agent to start out knowing.
type AgentMemorySeed struct {
	Kind    agentmemory.Kind
	Content string
	Pinned  bool
}

// InsertAgentMemorySeed stores one seed as a PENDING entry written by the
// importing person, with its first history row.
//
// Pending, never active: a seed comes from a file, and nothing the agent
// recalls may enter memory without a person of this project agreeing to it
// (ADR-0050 decision 1b's review rule). A pending entry is never recalled; the
// owner approves or deletes it on the agent's Memory tab, where it shows as
// written by the importing person. Pending entries do not count against the
// active limit, so no episode is archived by an import.
//
// The content rule is applied here as well as by the import's validation, so
// no caller of this function can store a credential-shaped seed: the check is
// agentmemory.CheckContent, the one rule of every memory write.
func InsertAgentMemorySeed(ctx context.Context, q AgentMemorySeedWriter, projectID, applicationID, writerUserID int64, seed AgentMemorySeed) (int64, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return 0, err
	}
	if !seed.Kind.AgentWritable() {
		return 0, fmt.Errorf("agent memory seed: kind %q cannot be seeded", seed.Kind)
	}
	content, err := agentmemory.CheckContent(seed.Content)
	if err != nil {
		return 0, fmt.Errorf("agent memory seed: %w", err)
	}
	var id int64
	if err := q.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.agent_memory_entries
    (application_id, kind, content, pinned, status, written_by_kind, written_by_user_id)
VALUES ($1, $2, $3, $4, 'pending', 'person', $5)
RETURNING id`, schema),
		applicationID, string(seed.Kind), content, seed.Pinned, writerUserID).Scan(&id); err != nil {
		return 0, fmt.Errorf("agent memory seed: insert: %w", err)
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.agent_memory_entry_versions
    (entry_id, version, content, kind, status, pinned, written_by_kind, written_by_user_id)
SELECT id, version, content, kind, status, pinned, written_by_kind, written_by_user_id
FROM %[1]s.agent_memory_entries WHERE id = $1
ON CONFLICT (entry_id, version) DO NOTHING`, schema), id); err != nil {
		return 0, fmt.Errorf("agent memory seed: history: %w", err)
	}
	return id, nil
}
