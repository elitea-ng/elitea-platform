package repos

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// The migration of existing compositions to delegation grants, and the sync
// that keeps new ones covered (ADR-0044 amendment 1, A1.3; delegation epic
// 2156 story S6).
//
// WHAT AN EDGE IS. Every composition edge is stored the same way: an
// `elitea_tools` row of type `application` carrying the callee's ids, bound to
// a version of the caller by `entity_tool_mapping` (the relation route writes
// the pair, and a version save, an import and a Marketplace install write the
// same rows; a legacy-migrated tenant also holds them in `application_tools`).
// The kind of the edge follows the caller:
//
//   - an agent that holds the row: `agent_tool`;
//   - a pipeline whose YAML has an Agent node that runs the row's alias:
//     `pipeline_node`; the pipeline's YAML is where those edges are DEFINED, so
//     they are read from it and not from the attach dialog, which they never
//     pass through;
//   - a pipeline that holds the row without an Agent node using it:
//     `pipeline_child`, as the attach path writes it.
//
// The worker reads the grant between a pair whatever its kind, so one grant per
// (caller, callee) pair is enough, and a pair that already has an active grant
// of any kind is left alone. A `sub_agent` is the same stored row as an agent
// tool (an agent offers a sub-agent through an application tool), so it needs
// no kind of its own here.
//
// WHICH EDGES NEED A GRANT. Only an edge whose callee holds more than its
// caller (GrantSet.Missing): where the callee's set is contained in the caller's
// nothing is refused today and nothing needs approving. The grant is PENDING
// and never approved: a migration that approved would be a blanket approval
// nobody read, and under the governed profile it would bypass maker-checker.
// Under the governed profile the grant is governed, records its change request
// like any attach, and needs a proposer (the check constraint of tenant 0252):
// the person who last wrote the caller's version.

// DelegationEdgesMigrationName is the run the startup job records in
// delegation_migration_runs.
const DelegationEdgesMigrationName = "0254_delegation_legacy_grants"

// NotifyLegacyPending is the event type of the one notification per calling
// agent that lists its pending legacy delegations.
const NotifyLegacyPending = dg.NotifyLegacyPending

// DelegationEdgeOptions says how one run behaves.
type DelegationEdgeOptions struct {
	// Origin is the grants' origin: dg.OriginMigration for the migration and the
	// admin scan, dg.OriginAttach for the sync after a save.
	Origin dg.Origin
	// Legacy flags the grants as written by the migration.
	Legacy bool
	// Actor is the proposer of a sync's grants. A migration has none.
	Actor int64
	// Notify tells the editors of each calling agent, once.
	Notify bool
	// Marker, when set, makes the run happen once per project: it is skipped
	// when delegation_migration_runs holds the name and records it on success.
	Marker string
}

// DelegationEdgeResult counts what a run did.
type DelegationEdgeResult struct {
	// Skipped is a run the marker said had happened.
	Skipped bool
	// Edges is the (caller, callee) pairs found, Contained those whose callee
	// holds nothing its caller does not, Existing those that already have a
	// grant (or a legacy grant that was decided), Created the grants written.
	Edges, Contained, Existing, Created int
	// Unattributed is the edges a governed project could not propose because no
	// person could be named as the proposer; the next run tries again.
	Unattributed int
	// Notified is the notifications written.
	Notified int
	Grants   []dg.Grant
}

// edgeCandidate is one stored attachment of a callee to a version of a caller.
type edgeCandidate struct {
	callerID, callerVersionID, calleeID, calleeVersionID int64
	pipeline                                             bool
	alias                                                string
}

type edgePair struct{ caller, callee int64 }

// DelegationEdges is the migration and the sync.
type DelegationEdges struct{ grants *DelegationGrants }

// NewDelegationEdges builds it over the grant store, which must be composed
// with the callee reader (WithDelegationCallees).
func NewDelegationEdges(grants *DelegationGrants) (*DelegationEdges, error) {
	if grants == nil || grants.pool == nil || grants.audit == nil || grants.callees == nil {
		return nil, errors.New("delegation edges: a grant store with a pool, an audit appender and a callee reader is required")
	}
	return &DelegationEdges{grants: grants}, nil
}

// Run discovers the project's edges and writes a pending grant for each one
// whose callee exceeds its caller and that has none.
func (e *DelegationEdges) Run(ctx context.Context, projectID int64, options DelegationEdgeOptions) (DelegationEdgeResult, error) {
	var result DelegationEdgeResult
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return result, err
	}
	if options.Origin == "" || (options.Origin != dg.OriginMigration && options.Actor <= 0) {
		return result, fmt.Errorf("%w: a sync names its proposer", dg.ErrInvalid)
	}
	var present bool
	if err := e.grants.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL AND to_regclass($2) IS NOT NULL AND to_regclass($3) IS NOT NULL`,
		schema+".delegation_grants", schema+".entity_tool_mapping", schema+".application_versions").Scan(&present); err != nil {
		return result, fmt.Errorf("delegation edges: probe the project's tables: %w", err)
	}
	if !present {
		result.Skipped = true
		return result, nil
	}
	if options.Marker != "" {
		done, err := e.marked(ctx, e.grants.pool, schema, options.Marker)
		if err != nil || done {
			result.Skipped = done
			return result, err
		}
	}
	candidates, err := e.candidates(ctx, schema)
	if err != nil {
		return result, err
	}
	pairs, kinds, versions := groupEdges(candidates)
	result.Edges = len(pairs)
	covered, err := e.coveredPairs(ctx, schema)
	if err != nil {
		return result, err
	}
	governed := e.grants.governed != nil && e.grants.governed(ctx)

	type proposal struct {
		pair     edgePair
		callee   dg.Callee
		proposer int64
	}
	var proposals []proposal
	for _, pair := range pairs {
		if covered[pair] {
			result.Existing++
			continue
		}
		callee, exceeds, err := e.exceeding(ctx, projectID, pair, versions[pair])
		if err != nil {
			return result, err
		}
		if !exceeds {
			result.Contained++
			continue
		}
		proposer := options.Actor
		if governed {
			// The maker is the person who last wrote the caller, not whoever
			// armed the debounced sync: another person's later write may have
			// made the edge, and maker-checker keys on the proposer.
			if author := e.lastAuthor(ctx, schema, pair.caller); author > 0 {
				proposer = author
			}
			if proposer <= 0 {
				result.Unattributed++
				continue
			}
		}
		proposals = append(proposals, proposal{pair: pair, callee: callee, proposer: proposer})
	}

	err = e.grants.inTx(ctx, func(tx pgx.Tx) error {
		// One writer per project at a time, across replicas: a second run waits
		// here and then finds the grants and the marker the first one wrote.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "delegation-edges/"+schema); err != nil {
			return fmt.Errorf("delegation edges: lock the project: %w", err)
		}
		if options.Marker != "" {
			done, err := e.marked(ctx, tx, schema, options.Marker)
			if err != nil {
				return err
			}
			if done {
				result.Skipped = true
				return nil
			}
		}
		for _, p := range proposals {
			input := ProposeInput{
				ProjectID: projectID, Edge: dg.Edge{CallerEntityID: p.pair.caller, CalleeEntityID: p.pair.callee, Kind: kinds[p.pair]},
				CalleeVersionID: p.callee.VersionID, Requested: p.callee.Set,
				Origin: options.Origin, Legacy: options.Legacy, Governed: governed,
			}
			if p.proposer > 0 {
				proposer := p.proposer
				input.ProposedBy = &proposer
			}
			grant, err := e.grants.ProposeTx(ctx, tx, input)
			if errors.Is(err, dg.ErrActiveEdgeExists) {
				result.Existing++
				continue
			}
			if err != nil {
				return fmt.Errorf("delegation edges: propose the edge %d to %d: %w", p.pair.caller, p.pair.callee, err)
			}
			result.Created++
			result.Grants = append(result.Grants, grant)
		}
		if options.Notify {
			told, err := e.notifyCallers(ctx, tx, schema, projectID, result.Grants)
			if err != nil {
				return err
			}
			result.Notified = told
		}
		// An edge a governed project could not attribute leaves the run
		// incomplete: the marker stays unwritten and the next start tries again.
		if options.Marker != "" && result.Unattributed == 0 {
			if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.delegation_migration_runs (name, edges, contained, created, notified) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (name) DO NOTHING`, schema), options.Marker, result.Edges, result.Contained, result.Created, result.Notified); err != nil {
				return fmt.Errorf("delegation edges: record the run: %w", err)
			}
		}
		return nil
	})
	if result.Skipped {
		result.Created, result.Notified, result.Grants = 0, 0, nil
	}
	return result, err
}

func (e *DelegationEdges) marked(ctx context.Context, q grantQuerier, schema, name string) (bool, error) {
	var done bool
	if err := q.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.delegation_migration_runs WHERE name = $1)`, schema), name).Scan(&done); err != nil {
		return false, fmt.Errorf("delegation edges: read the run marker: %w", err)
	}
	return done, nil
}

// candidates reads every stored attachment of an agent or pipeline to a
// version of another, with the kind of caller it is and, for a pipeline, the
// alias its Agent nodes use.
func (e *DelegationEdges) candidates(ctx context.Context, schema string) ([]edgeCandidate, error) {
	// Only the versions a run can use: each caller's newest version and its
	// published ones. A version that once attached an agent and was replaced
	// since does not make an edge. A tool row that pins no callee version runs
	// the callee's newest.
	query := fmt.Sprintf(`
SELECT DISTINCT version.application_id::bigint, version.id::bigint, COALESCE(version.agent_type, '') = 'pipeline',
       callee.id::bigint,
       CASE WHEN (%[2]s) ~ '^[0-9]{1,18}$' THEN (%[2]s)::bigint
            ELSE (SELECT max(newest.id)::bigint FROM %[1]s.application_versions AS newest WHERE newest.application_id = callee.id) END,
       COALESCE(tool.name, '')
FROM %[1]s.entity_tool_mapping AS mapping
JOIN %[1]s.elitea_tools AS tool ON tool.id = mapping.tool_id
JOIN %[1]s.application_versions AS version ON version.id = mapping.entity_version_id
JOIN %[1]s.applications AS callee ON callee.id = (tool.settings->>'application_id')::bigint
WHERE mapping.entity_type = 'agent' AND tool.type = 'application'
  AND tool.settings->>'application_id' ~ '^[0-9]{1,18}$'
  AND version.application_id <> callee.id
  AND (version.status = 'published' OR version.id = (SELECT max(latest.id) FROM %[1]s.application_versions AS latest WHERE latest.application_id = version.application_id))`, schema, ApplicationToolSettingsVersionSQL)
	legacy, err := legacyApplicationToolsExists(ctx, pgxReader{e.grants.pool}, schema)
	if err != nil {
		return nil, err
	}
	if legacy {
		query += fmt.Sprintf(`
UNION
SELECT DISTINCT version.application_id::bigint, version.id::bigint, COALESCE(version.agent_type, '') = 'pipeline',
       callee.id::bigint,
       CASE WHEN (%[2]s) ~ '^[0-9]{1,18}$' THEN (%[2]s)::bigint
            ELSE (SELECT max(newest.id)::bigint FROM %[1]s.application_versions AS newest WHERE newest.application_id = callee.id) END,
       COALESCE(tool.name, '')
FROM %[1]s.application_tools AS tool
JOIN %[1]s.application_versions AS version ON version.id = tool.application_version_id
JOIN %[1]s.applications AS callee ON callee.id = (tool.settings->>'application_id')::bigint
WHERE tool.type = 'application'
  AND tool.settings->>'application_id' ~ '^[0-9]{1,18}$'
  AND version.application_id <> callee.id
  AND (version.status = 'published' OR version.id = (SELECT max(latest.id) FROM %[1]s.application_versions AS latest WHERE latest.application_id = version.application_id))`, schema, ApplicationToolSettingsVersionSQL)
	}
	rows, err := e.grants.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("delegation edges: read the attachments: %w", err)
	}
	defer rows.Close()
	var out []edgeCandidate
	for rows.Next() {
		var c edgeCandidate
		if err := rows.Scan(&c.callerID, &c.callerVersionID, &c.pipeline, &c.calleeID, &c.calleeVersionID, &c.alias); err != nil {
			return nil, fmt.Errorf("delegation edges: scan an attachment: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("delegation edges: read the attachments: %w", err)
	}
	// The Agent nodes of each pipeline caller version are in its YAML.
	var pipelineVersions []int64
	for _, c := range out {
		if c.pipeline && !slices.Contains(pipelineVersions, c.callerVersionID) {
			pipelineVersions = append(pipelineVersions, c.callerVersionID)
		}
	}
	aliases, err := e.nodeAliases(ctx, schema, pipelineVersions)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].pipeline && !aliases[out[i].callerVersionID][out[i].alias] {
			out[i].alias = "" // attached, but no Agent node of its YAML runs it
		}
	}
	return out, nil
}

// nodeAliases reads the tool aliases the Agent nodes of each pipeline version's
// YAML run. A definition the save-time admission refuses or that does not parse
// has none (applicationcomposition.ReadPipeline).
func (e *DelegationEdges) nodeAliases(ctx context.Context, schema string, versionIDs []int64) (map[int64]map[string]bool, error) {
	out := map[int64]map[string]bool{}
	if len(versionIDs) == 0 {
		return out, nil
	}
	rows, err := e.grants.pool.Query(ctx, fmt.Sprintf(`
SELECT id::bigint, instructions FROM %s.application_versions
WHERE id = ANY($1::bigint[]) AND octet_length(COALESCE(instructions, '')) <= $2`, schema),
		versionIDs, pipelinelimits.MaxInstructionsBytes)
	if err != nil {
		return nil, fmt.Errorf("delegation edges: read the pipeline definitions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var instructions *string
		if err := rows.Scan(&id, &instructions); err != nil {
			return nil, fmt.Errorf("delegation edges: scan a pipeline definition: %w", err)
		}
		if instructions == nil {
			continue
		}
		nodes, _, _, _ := applicationcomposition.ReadPipeline(*instructions)
		aliases := map[string]bool{}
		for _, node := range nodes {
			aliases[node.Alias] = true
		}
		out[id] = aliases
	}
	return out, rows.Err()
}

// groupEdges folds the attachments into (caller, callee) pairs in a stable
// order, with the kind each pair is granted as and the version pairs to compare.
func groupEdges(candidates []edgeCandidate) ([]edgePair, map[edgePair]dg.EdgeKind, map[edgePair][]edgeCandidate) {
	kinds := map[edgePair]dg.EdgeKind{}
	versions := map[edgePair][]edgeCandidate{}
	for _, c := range candidates {
		pair := edgePair{caller: c.callerID, callee: c.calleeID}
		versions[pair] = append(versions[pair], c)
		kind := dg.EdgeAgentTool
		switch {
		case c.pipeline && c.alias != "":
			kind = dg.EdgePipelineNode
		case c.pipeline:
			kind = dg.EdgePipelineChild
		}
		// A pair that any pipeline YAML node runs is a pipeline_node edge.
		if existing, ok := kinds[pair]; !ok || kind == dg.EdgePipelineNode || (existing == dg.EdgeAgentTool && kind == dg.EdgePipelineChild) {
			kinds[pair] = kind
		}
	}
	pairs := make([]edgePair, 0, len(versions))
	for pair := range versions {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].caller != pairs[j].caller {
			return pairs[i].caller < pairs[j].caller
		}
		return pairs[i].callee < pairs[j].callee
	})
	return pairs, kinds, versions
}

// coveredPairs reads the pairs that need nothing written: those with an active
// grant of any kind, and those the migration already wrote a grant for that an
// owner then decided (a rejected legacy edge stays rejected: it is not
// proposed again on every run).
func (e *DelegationEdges) coveredPairs(ctx context.Context, schema string) (map[edgePair]bool, error) {
	rows, err := e.grants.pool.Query(ctx, fmt.Sprintf(`
SELECT DISTINCT caller_entity_id, callee_entity_id FROM %s.delegation_grants
WHERE state IN ('pending', 'approved', 'stale') OR legacy`, schema))
	if err != nil {
		return nil, fmt.Errorf("delegation edges: read the existing grants: %w", err)
	}
	defer rows.Close()
	covered := map[edgePair]bool{}
	for rows.Next() {
		var pair edgePair
		if err := rows.Scan(&pair.caller, &pair.callee); err != nil {
			return nil, fmt.Errorf("delegation edges: scan a grant: %w", err)
		}
		covered[pair] = true
	}
	return covered, rows.Err()
}

// exceeding reads what the pair's versions hold and answers the callee version
// whose set exceeds its caller's by the most, when any does.
func (e *DelegationEdges) exceeding(ctx context.Context, projectID int64, pair edgePair, attachments []edgeCandidate) (dg.Callee, bool, error) {
	var (
		best   dg.Callee
		most   int
		exceed bool
	)
	for _, attachment := range attachments {
		callee, err := e.grants.callees.Snapshot(ctx, projectID, pair.callee, attachment.calleeVersionID)
		if errors.Is(err, dg.ErrCalleeNotFound) {
			continue // a deleted version holds nothing
		}
		if err != nil {
			return dg.Callee{}, false, err
		}
		caller, err := e.grants.callees.Snapshot(ctx, projectID, pair.caller, attachment.callerVersionID)
		if errors.Is(err, dg.ErrCalleeNotFound) {
			continue
		}
		if err != nil {
			return dg.Callee{}, false, err
		}
		missing := len(caller.Set.Missing(callee.Set))
		if missing == 0 {
			continue
		}
		if !exceed || missing > most || (missing == most && callee.VersionID > best.VersionID) {
			best, most, exceed = callee, missing, true
		}
	}
	return best, exceed, nil
}

// lastAuthor is the person who last wrote a version of the caller, the proposer
// of a governed legacy grant: the maker whose approval another person checks.
func (e *DelegationEdges) lastAuthor(ctx context.Context, schema string, callerID int64) int64 {
	var author *int64
	if err := e.grants.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT author_id::bigint FROM %s.application_versions WHERE application_id = $1 AND author_id IS NOT NULL ORDER BY id DESC LIMIT 1`, schema),
		callerID).Scan(&author); err != nil || author == nil {
		return 0
	}
	return *author
}

// notifyCallers tells the editors of each calling agent, once, which of its
// delegations await review. The key carries the first grant of the caller in
// this run, so a run that finds nothing new repeats nothing and a later run
// that finds a new edge is a new notice.
func (e *DelegationEdges) notifyCallers(ctx context.Context, tx pgx.Tx, schema string, projectID int64, grants []dg.Grant) (int, error) {
	byCaller := map[int64][]dg.Grant{}
	var order []int64
	for _, grant := range grants {
		caller := grant.Edge.CallerEntityID
		if _, seen := byCaller[caller]; !seen {
			order = append(order, caller)
		}
		byCaller[caller] = append(byCaller[caller], grant)
	}
	told := 0
	for _, caller := range order {
		group := byCaller[caller]
		people, err := callerEditors(ctx, tx, schema, group[0])
		if err != nil {
			return 0, err
		}
		callerName := agentName(ctx, tx, schema, caller)
		meta := map[string]any{
			"message":               legacyMessage(ctx, tx, schema, callerName, group),
			"project_id":            projectID,
			"caller_application_id": caller,
			"count":                 len(group),
		}
		ids := make([]int64, 0, len(group))
		for _, grant := range group {
			ids = append(ids, grant.ID)
		}
		meta["grant_ids"] = ids
		for _, person := range people {
			key := fmt.Sprintf("delegation-legacy-%d-%d-%d-%d", projectID, caller, group[0].ID, person)
			if err := marketplace.Notify(ctx, tx, key, projectID, person, NotifyLegacyPending, meta); err != nil {
				return 0, err
			}
			told++
		}
	}
	return told, nil
}

func legacyMessage(ctx context.Context, q grantQuerier, schema, callerName string, group []dg.Grant) string {
	names := ""
	for index, grant := range group {
		if index == 3 {
			names += fmt.Sprintf(" and %d more", len(group)-3)
			break
		}
		if index > 0 {
			names += ", "
		}
		names += agentName(ctx, q, schema, grant.Edge.CalleeEntityID)
	}
	return fmt.Sprintf("Delegations awaiting review: %s calls %s, which hold more than it does. Nothing was approved, "+
		"and the agent keeps what it could use before. Review each delegation so %s can use what it brings.",
		callerName, names, callerName)
}

// DelegationProjectQueue is one project's share of the review queue.
type DelegationProjectQueue struct {
	ProjectID   int64
	ProjectName string
	// Pending are grants awaiting a first approval, Legacy those of them the
	// migration wrote, Stale grants whose callee has since changed.
	Pending, Legacy, Stale int
	// Callers are the agents with the most grants to review, for the page to
	// open.
	Callers []DelegationQueueCaller
}

// DelegationQueueCaller is a calling agent with grants to review.
type DelegationQueueCaller struct {
	ApplicationID int64
	Name          string
	Pending       int
	Stale         int
}

// maxQueueCallers bounds the callers listed per project.
const maxQueueCallers = 20

// ReviewQueue counts, per project, the delegation grants that await review:
// the pending and the stale ones. A project with none is not listed.
func (s *DelegationGrants) ReviewQueue(ctx context.Context) ([]DelegationProjectQueue, error) {
	rows, err := s.pool.Query(ctx, `
SELECT p.id, COALESCE(to_jsonb(p)->>'name', '')
FROM centry.project AS p
WHERE to_regclass(format('%I.delegation_grants', 'p_' || p.id)) IS NOT NULL
ORDER BY p.id`)
	if undefinedTable(err) {
		return []DelegationProjectQueue{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delegation review: list the projects: %w", err)
	}
	var projects []DelegationProjectQueue
	for rows.Next() {
		var project DelegationProjectQueue
		if err := rows.Scan(&project.ProjectID, &project.ProjectName); err != nil {
			rows.Close()
			return nil, err
		}
		projects = append(projects, project)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	queue := []DelegationProjectQueue{}
	// One counting query per batch of projects (a union over their tenant
	// schemas), and the calling agents only for a project that has a queue.
	for start := 0; start < len(projects); start += queueBatch {
		end := min(start+queueBatch, len(projects))
		batch := projects[start:end]
		var union strings.Builder
		for index, project := range batch {
			schema, err := tenantSchema(strconv.FormatInt(project.ProjectID, 10))
			if err != nil {
				continue
			}
			if index > 0 && union.Len() > 0 {
				union.WriteString(" UNION ALL ")
			}
			fmt.Fprintf(&union, `SELECT %d::bigint AS project_id,
       count(*) FILTER (WHERE state = 'pending')::int AS pending,
       count(*) FILTER (WHERE state = 'pending' AND legacy)::int AS legacy,
       count(*) FILTER (WHERE state = 'stale')::int AS stale
FROM %s.delegation_grants WHERE state IN ('pending', 'stale')`, project.ProjectID, schema)
		}
		if union.Len() == 0 {
			continue
		}
		counts, err := s.pool.Query(ctx, union.String())
		if err != nil {
			return nil, fmt.Errorf("delegation review: count the projects: %w", err)
		}
		byProject := map[int64]DelegationProjectQueue{}
		for counts.Next() {
			var counted DelegationProjectQueue
			if err := counts.Scan(&counted.ProjectID, &counted.Pending, &counted.Legacy, &counted.Stale); err != nil {
				counts.Close()
				return nil, err
			}
			byProject[counted.ProjectID] = counted
		}
		counts.Close()
		if err := counts.Err(); err != nil {
			return nil, err
		}
		for _, project := range batch {
			counted, ok := byProject[project.ProjectID]
			if !ok || counted.Pending+counted.Stale == 0 {
				continue
			}
			project.Pending, project.Legacy, project.Stale = counted.Pending, counted.Legacy, counted.Stale
			callers, err := s.queueCallers(ctx, project.ProjectID)
			if err != nil {
				return nil, err
			}
			project.Callers = callers
			queue = append(queue, project)
		}
	}
	return queue, nil
}

// queueBatch is how many projects one counting query covers.
const queueBatch = 100

// queueCallers lists the calling agents of a project with the most delegations
// to review.
func (s *DelegationGrants) queueCallers(ctx context.Context, projectID int64) ([]DelegationQueueCaller, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT grants.caller_entity_id, COALESCE(application.name, ''),
       count(*) FILTER (WHERE grants.state = 'pending'), count(*) FILTER (WHERE grants.state = 'stale')
FROM %[1]s.delegation_grants AS grants
LEFT JOIN %[1]s.applications AS application ON application.id = grants.caller_entity_id
WHERE grants.state IN ('pending', 'stale')
GROUP BY grants.caller_entity_id, application.name
ORDER BY count(*) DESC, grants.caller_entity_id
LIMIT %[2]d`, schema, maxQueueCallers))
	if err != nil {
		return nil, fmt.Errorf("delegation review: list the callers of project %d: %w", projectID, err)
	}
	defer rows.Close()
	var callers []DelegationQueueCaller
	for rows.Next() {
		var caller DelegationQueueCaller
		if err := rows.Scan(&caller.ApplicationID, &caller.Name, &caller.Pending, &caller.Stale); err != nil {
			return nil, err
		}
		callers = append(callers, caller)
	}
	return callers, rows.Err()
}
