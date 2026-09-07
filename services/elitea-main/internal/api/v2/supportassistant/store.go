package supportassistant

// All of this package's SQL.
//
// It is one file on purpose. The support project is SHARED BY EVERY USER ON THE
// PLATFORM, so the difference between "your support history" and "everybody's
// support history" is a single predicate — `author_id = $caller AND source =
// 'support'` — and a predicate that decides that must not be scattered across
// five handlers where the sixth can forget it. Every statement below that
// touches `chat_conversations` carries it, and `conversationOwnedByCaller` is
// the one lookup the mutating routes go through.
//
// # Why the tenant schema is interpolated without escaping
//
// `p_%d` is built from an int64 that came out of `centry.platform_config` via
// `Values.Int`, which returns a number or nothing. No caller-supplied text
// reaches these statements, which is a stronger guarantee than escaping: there
// is no string on this path to escape.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// supportSource is the `chat_conversations.source` value that marks a row as
// belonging to this feature, verbatim from `api/v2/conversations.py`'s
// `source='support'`. It is also what keeps support transcripts OUT of the
// ordinary chat listing if the hidden project is ever opened by a person.
const supportSource = "support"

// bootstrapLockKey keys the advisory lock the project bootstrap serialises on.
// It is a fixed arbitrary constant; the only requirement is that this feature
// uses the same number in every replica and no other feature uses it.
const bootstrapLockKey int64 = 0x5507A551 // "SUPPASSI"

type store struct {
	pool        *pgxpool.Pool
	provisioner Provisioner
	logger      *slog.Logger

	// bootstrapGate rate-limits FAILED bootstrap attempts.
	//
	// Without it a deployment whose provisioning fails retries the entire
	// pipeline — tenant schema, migrations, RBAC, vault — on EVERY request,
	// under one process-wide advisory lock. `GET /support_assistant/config` is
	// ungated and the widget calls it from the app shell on every page load, so
	// every user's every navigation would queue behind a multi-second failing
	// operation. One attempt per bootstrapRetryAfter turns a self-inflicted
	// outage back into a feature that is merely off.
	bootstrapGate      sync.Mutex
	bootstrapNotBefore time.Time
}

// bootstrapRetryAfter is how long a failed bootstrap is left alone. It is long
// enough that a failing deployment is not hammered, and short enough that an
// operator who fixes the cause (attaches an object store, repairs the vault)
// sees the assistant come up without a restart.
const bootstrapRetryAfter = 5 * time.Minute

// ---------------------------------------------------------------------------
// settings and bootstrap
// ---------------------------------------------------------------------------

// settings resolves the section, bootstrapping the hidden project the first time
// an enabled deployment needs one.
func (s *store) settings(ctx context.Context) (platformconfig.SupportAssistant, error) {
	resolved, err := platformconfig.LoadSupportAssistant(ctx, s.pool)
	if err != nil {
		return platformconfig.SupportAssistant{}, err
	}
	if !resolved.Enabled || resolved.ProjectID > 0 {
		return resolved, nil
	}
	projectID, err := s.bootstrapProject(ctx)
	if err != nil {
		// Reported as NOT READY rather than as an error to the caller: an
		// operator who enabled the switch on a deployment with no provisioner
		// gets no widget, which is recoverable, instead of a 500 on every page.
		s.logger.Warn("support assistant: project bootstrap unavailable", "err", err)
		return resolved, nil
	}
	resolved.ProjectID = projectID
	return resolved, nil
}

// bootstrapProject creates the hidden support project exactly once per
// deployment and records its id back into `centry.platform_config`, so the
// Features page shows the operator which project it landed in.
func (s *store) bootstrapProject(ctx context.Context) (int64, error) {
	if s.provisioner == nil {
		return 0, ErrNoProvisioner
	}
	if !s.claimBootstrapAttempt() {
		return 0, errBootstrapCoolingDown
	}

	// PROVISIONING IS NOT CANCELLED BY THE CALLER GOING AWAY.
	//
	// The pipeline is many statements across several connections, and the
	// request that happens to trigger it is a widget poll from a page the user
	// may navigate away from at any moment. Cancelling mid-pipeline aborts
	// whichever step is in flight and leaves the compensating rollback to run
	// against a context that is already dead. The work belongs to the
	// deployment, not to the request that noticed it was needed.
	ctx = context.WithoutCancel(ctx)

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("support assistant: acquire connection: %w", err)
	}
	defer conn.Release()

	// A SESSION lock, taken on a single pinned connection and released
	// explicitly, because provisioning is many statements over its own pool
	// connections and cannot run inside one transaction.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, bootstrapLockKey); err != nil {
		return 0, fmt.Errorf("support assistant: bootstrap lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, bootstrapLockKey); err != nil {
			s.logger.Error("support assistant: bootstrap unlock", "err", err)
		}
	}()

	// Re-read INSIDE the lock. This is the whole point of the lock: the replica
	// that waited here must see the winner's write rather than provision a
	// second "Support Assistant" project.
	values, err := platformconfig.Load(ctx, s.pool, platformconfig.SectionSupportAssistant)
	if err != nil {
		return 0, err
	}
	if existing, ok := values.Int(platformconfig.KeySupportProjectID); ok && existing > 0 {
		s.releaseBootstrapCooldown()
		return existing, nil
	}

	// A project by that name from a PREVIOUS bootstrap whose id row was cleared
	// (an operator emptying the field on the Features page) is adopted rather
	// than duplicated. The reference has no equivalent and grows a new project
	// every time its state file is lost.
	if adopted, err := s.projectByName(ctx, SupportProjectName); err != nil {
		return 0, err
	} else if adopted > 0 {
		if err := s.recordProjectID(ctx, adopted); err != nil {
			return 0, err
		}
		s.releaseBootstrapCooldown()
		return adopted, nil
	}

	ownerID, err := s.ensureSystemUser(ctx)
	if err != nil {
		return 0, err
	}
	projectID, err := s.provisioner.Provision(ctx, ProvisionRequest{
		Name:       SupportProjectName,
		OwnerID:    ownerID,
		AdminEmail: SystemUserEmail,
	})
	if err != nil {
		return 0, fmt.Errorf("support assistant: provision project: %w", err)
	}
	if err := s.recordProjectID(ctx, projectID); err != nil {
		// The project EXISTS at this point. Failing to record its id would make
		// the next request provision another one, so this is reported rather
		// than swallowed — and the name-adoption branch above is what recovers
		// the deployment if it ever happens.
		return 0, err
	}
	s.releaseBootstrapCooldown()
	s.logger.Info("support assistant: hidden project created", "project_id", projectID)
	return projectID, nil
}

// claimBootstrapAttempt reports whether this caller may attempt a bootstrap now,
// and starts the cooldown if so. The cooldown is cleared on success, so a
// deployment that provisions cleanly is never delayed.
func (s *store) claimBootstrapAttempt() bool {
	s.bootstrapGate.Lock()
	defer s.bootstrapGate.Unlock()
	if time.Now().Before(s.bootstrapNotBefore) {
		return false
	}
	s.bootstrapNotBefore = time.Now().Add(bootstrapRetryAfter)
	return true
}

// releaseBootstrapCooldown clears the gate after a bootstrap that produced a
// project id, so the adopt-by-name path on a later call is not delayed by a
// cooldown started for an attempt that then succeeded.
func (s *store) releaseBootstrapCooldown() {
	s.bootstrapGate.Lock()
	defer s.bootstrapGate.Unlock()
	s.bootstrapNotBefore = time.Time{}
}

// errBootstrapCoolingDown is returned while a failed attempt is in its cooldown.
// It is not surfaced to a caller — settings() reports the assistant as not
// ready, which renders as no widget.
var errBootstrapCoolingDown = errors.New("support assistant: bootstrap is cooling down after a failure")

func (s *store) projectByName(ctx context.Context, name string) (int64, error) {
	var projectID int64
	// `create_success` is the provisioner's own "every step finished" marker
	// (see createProjectModel / markCreated). Adopting a HALF-PROVISIONED row —
	// one whose tenant schema or RBAC never landed — would point the assistant
	// at a project whose `chat_conversations` table does not exist, so the
	// listing would 500 forever with no way for an operator to see why.
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM centry.project
		 WHERE name = $1 AND create_success IS TRUE
		 ORDER BY id LIMIT 1`, name).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("support assistant: lookup project by name: %w", err)
	}
	return projectID, nil
}

// ensureSystemUser resolves the platform system identity, creating it when the
// deployment has none. `ON CONFLICT DO NOTHING` plus the UNION is the same
// race-free upsert-and-read `createSystemUser` uses in the provisioner.
func (s *store) ensureSystemUser(ctx context.Context) (int64, error) {
	var userID int64
	err := s.pool.QueryRow(ctx, `
WITH created AS (
    INSERT INTO public.auth_core__user (email, name)
    VALUES ($1, $2)
    ON CONFLICT (email) DO NOTHING
    RETURNING id
)
SELECT id FROM created
UNION ALL
SELECT id FROM public.auth_core__user WHERE email = $1
LIMIT 1`, SystemUserEmail, "System").Scan(&userID)
	if err != nil {
		return 0, fmt.Errorf("support assistant: resolve system user: %w", err)
	}
	return userID, nil
}

// recordProjectID writes the bootstrapped id into the same row the Features page
// edits, with the same statement that page uses, so the value the operator sees
// and the value this package reads are one row.
func (s *store) recordProjectID(ctx context.Context, projectID int64) error {
	encoded, err := json.Marshal(projectID)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value, updated_at, updated_by)
VALUES ($1, $2, $3::jsonb, now(), $4)
ON CONFLICT (section, key)
DO UPDATE SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by`,
		platformconfig.SectionSupportAssistant,
		platformconfig.KeySupportProjectID,
		string(encoded),
		"support_assistant_bootstrap",
	); err != nil {
		return fmt.Errorf("support assistant: record project id: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// enrolment
// ---------------------------------------------------------------------------

// ensureEnrolled grants `viewer` in the support project to a caller who holds no
// role there, mirroring `module.ensure_user_enrolled`.
//
// `viewer` and not more. Migration 0068/0070 give the default-mode viewer every
// permission this package's routes ask for — list, create, delete, details,
// messages.create, messages.delete, attachments.create — so viewer is both
// sufficient and the least this feature can ask for. Enrolling everybody as
// editor would hand every user on the platform the ability to edit whatever else
// ever lands in that project.
func (s *store) ensureEnrolled(ctx context.Context, projectID, userID int64) error {
	var exists bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM public.auth_core__project_user_role
    WHERE project_id = $1 AND user_id = $2
)`, projectID, userID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("support assistant: read membership: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT $1, $2, role.id
FROM public.auth_core__project_role AS role
WHERE role.project_id = $1 AND role.name = 'viewer'
ON CONFLICT (project_id, user_id, role_id) DO NOTHING`, projectID, userID); err != nil {
		return fmt.Errorf("support assistant: enrol: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// conversations
// ---------------------------------------------------------------------------

// Conversation is one row of the widget's history list. The field names are the
// ones `@eliteaai/elitea-assistant`'s `TConversationListItem` reads.
type Conversation struct {
	ID                int64          `json:"id"`
	UUID              string         `json:"uuid"`
	Name              string         `json:"name"`
	IsPrivate         bool           `json:"is_private"`
	AuthorID          int64          `json:"author_id"`
	Source            string         `json:"source"`
	Meta              map[string]any `json:"meta"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	MessageGroupCount int            `json:"message_groups_count"`
}

// tenantSchema returns the project's schema QUOTED as a PostgreSQL identifier,
// ready to interpolate with %s.
//
// The id comes from centry.platform_config and not from a caller, but the name
// still becomes an identifier in a statement, so it is quoted by the rule that
// applies to identifiers rather than with %q, which quotes with Go rules
// (issue #543). A value that is not a project id gives a schema that cannot
// exist, so the statement fails closed instead of reading another tenant.
func tenantSchema(projectID int64) string {
	quoted, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return `"p_!invalid"`
	}
	return quoted
}

// listConversations returns the CALLER'S support conversations, newest first.
func (s *store) listConversations(
	ctx context.Context, projectID, userID int64, limit, offset int, query string,
) ([]Conversation, int, error) {
	schema := tenantSchema(projectID)

	var total int
	countStatement := fmt.Sprintf(`
SELECT COUNT(*) FROM %s.chat_conversations c
WHERE c.author_id = $1 AND c.source = $2
  AND ($3 = '' OR c.name ILIKE '%%' || $3 || '%%' ESCAPE '\')`, schema)
	if err := s.pool.QueryRow(ctx, countStatement, userID, supportSource, escapeLikePattern(query)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("support assistant: count conversations: %w", err)
	}

	statement := fmt.Sprintf(`
SELECT c.id, c.uuid::text, c.name, c.is_private, c.author_id, c.source, c.meta,
       c.created_at, COALESCE(c.updated_at, c.created_at),
       (SELECT COUNT(*) FROM %s.chat_message_group mg WHERE mg.conversation_id = c.id)
FROM %s.chat_conversations c
WHERE c.author_id = $1 AND c.source = $2
  AND ($3 = '' OR c.name ILIKE '%%' || $3 || '%%' ESCAPE '\')
ORDER BY COALESCE(c.updated_at, c.created_at) DESC, c.id DESC
LIMIT $4 OFFSET $5`, schema, schema)

	rows, err := s.pool.Query(ctx, statement, userID, supportSource, escapeLikePattern(query), limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("support assistant: list conversations: %w", err)
	}
	defer rows.Close()

	items := make([]Conversation, 0, limit)
	for rows.Next() {
		conversation, err := scanConversation(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, conversation)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("support assistant: list conversations: %w", err)
	}
	return items, total, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanConversation(row scanner) (Conversation, error) {
	var conversation Conversation
	var meta []byte
	if err := row.Scan(
		&conversation.ID, &conversation.UUID, &conversation.Name, &conversation.IsPrivate,
		&conversation.AuthorID, &conversation.Source, &meta,
		&conversation.CreatedAt, &conversation.UpdatedAt, &conversation.MessageGroupCount,
	); err != nil {
		return Conversation{}, fmt.Errorf("support assistant: read conversation: %w", err)
	}
	conversation.Meta = map[string]any{}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &conversation.Meta) // trusted internal column
	}
	return conversation, nil
}

// createConversation inserts one support conversation.
//
// `source`, `is_private` and the `meta` document are transcribed from
// `api/v2/conversations.py`'s POST: hidden, private, typed `support`, with
// `internal_tools: ['internal_mcp']`. They are written HERE rather than accepted
// from the client for the obvious reason — a client that could choose
// `is_hidden: false` could publish its own support transcript into the shared
// project's ordinary chat listing.
func (s *store) createConversation(
	ctx context.Context, projectID, userID int64, name string,
) (Conversation, error) {
	schema := tenantSchema(projectID)
	meta, err := json.Marshal(map[string]any{
		"is_hidden":         true,
		"conversation_type": supportSource,
		"internal_tools":    []string{"internal_mcp"},
	})
	if err != nil {
		return Conversation{}, err
	}

	statement := fmt.Sprintf(`
INSERT INTO %s.chat_conversations (uuid, name, is_private, author_id, meta, source)
VALUES (gen_random_uuid(), $1, TRUE, $2, $3::jsonb, $4)
RETURNING id, uuid::text, name, is_private, author_id, source, meta,
          created_at, COALESCE(updated_at, created_at), 0`, schema)

	row := s.pool.QueryRow(ctx, statement, name, userID, string(meta), supportSource)
	return scanConversation(row)
}

// conversationOwnedByCaller resolves a conversation UUID to its row id, and is
// the ONLY way the mutating routes address a conversation.
//
// It is a single statement rather than a fetch-then-check because the two are
// not equivalent: a check performed in the handler can be skipped by the next
// route somebody adds, and "not yours" and "does not exist" must answer the same
// 404 — a distinct 403 for somebody else's conversation would confirm that the
// UUID names a real support conversation belonging to a real other user.
func (s *store) conversationOwnedByCaller(
	ctx context.Context, projectID, userID int64, conversationUUID string,
) (int64, error) {
	schema := tenantSchema(projectID)
	statement := fmt.Sprintf(`
SELECT c.id FROM %s.chat_conversations c
WHERE c.uuid = $1::uuid AND c.author_id = $2 AND c.source = $3`, schema)

	var conversationID int64
	err := s.pool.QueryRow(ctx, statement, conversationUUID, userID, supportSource).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errConversationNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("support assistant: resolve conversation: %w", err)
	}
	return conversationID, nil
}

var errConversationNotFound = errors.New("conversation not found")

// conversationByID re-reads one conversation for the details response. It is a
// second read rather than a wider first one because the ownership lookup has a
// single job — decide whether this caller may address this row — and a lookup
// that also returns a payload invites a caller to be added later that uses the
// payload and skips the decision.
func (s *store) conversationByID(ctx context.Context, projectID, conversationID int64) (Conversation, error) {
	schema := tenantSchema(projectID)
	statement := fmt.Sprintf(`
SELECT c.id, c.uuid::text, c.name, c.is_private, c.author_id, c.source, c.meta,
       c.created_at, COALESCE(c.updated_at, c.created_at),
       (SELECT COUNT(*) FROM %s.chat_message_group mg WHERE mg.conversation_id = c.id)
FROM %s.chat_conversations c WHERE c.id = $1`, schema, schema)
	return scanConversation(s.pool.QueryRow(ctx, statement, conversationID))
}

// escapeLikePattern makes a user's search term a LITERAL inside an ILIKE
// pattern.
//
// `%` and `_` are wildcards to LIKE, so an unescaped term does not mean what the
// user typed: searching for "50% CPU" matched "50 percent of the CPU budget",
// and a bare "_" matched every single-character position. Binding the value as a
// parameter prevents SQL injection but has never had anything to say about
// pattern metacharacters.
//
// The backslash is escaped FIRST, so that a term containing one does not turn
// the escape character introduced for `%` into a literal.
func escapeLikePattern(term string) string {
	if term == "" {
		return ""
	}
	replaced := strings.ReplaceAll(term, `\`, `\\`)
	replaced = strings.ReplaceAll(replaced, "%", `\%`)
	return strings.ReplaceAll(replaced, "_", `\_`)
}

// ---------------------------------------------------------------------------
// the answering agent
// ---------------------------------------------------------------------------

// agentVersion is the ONE thing about the configured agent this package has to
// know: which of its versions answers, and what kind of agent it is.
//
// It is read HERE rather than taken from the operator's configuration because
// the Features page stores an AGENT id, not a version id. A turn is addressed
// to a version: `ResolveCurrentApplicationTurn` joins `application_versions` on
// `entity_settings ->> 'version_id'`. An agent id alone therefore resolves
// nothing, which is exactly how this feature shipped broken.
type agentVersion struct {
	// ID is the `application_versions.id` the turn runs.
	ID int64
	// AgentType is the version's `agent_type`, carried into the participant's
	// entity_settings the way the chat surface carries it. It is display and
	// routing metadata for the transcript, not a second source of truth.
	AgentType string
}

// errAgentVersionNotFound reports that the configured agent has no version in
// the project the turn runs in. That covers BOTH real causes — an agent id that
// names nothing, and an agent that lives in a different project — because from
// the support project's tenant schema the two are the same absence.
var errAgentVersionNotFound = errors.New("support assistant: the configured agent has no version in the support project")

// agentVersionOf resolves which version of the configured agent answers.
//
// The order is the platform's own idea of "the agent's current version", in the
// same precedence the applications API applies:
//
//  1. the version `applications.meta.default_version_id` names, which is what
//     `ApplicationsRepo.SetDefaultVersion` writes when an operator pins one;
//  2. the version named `latest`, then the one named `base`. BOTH names are in
//     use: an imported or published agent carries `latest`, and an agent
//     authored in this app carries `base` — measured on the standalone stack,
//     where every seeded agent's only version is named `base`. A precedence
//     that knew one name would answer "no version" for half the agents an
//     operator can choose;
//  3. the newest row, so an agent whose versions were renamed still answers.
//
// Reading it per turn rather than storing it on the participant is deliberate:
// an operator who publishes a new version must have the next support message
// use it, and a version id frozen into a mapping row at first use would pin the
// assistant to whatever version existed on the day somebody first asked.
func (s *store) agentVersionOf(ctx context.Context, projectID, agentID int64) (agentVersion, error) {
	if s.pool == nil {
		return agentVersion{}, errAgentVersionNotFound
	}
	schema := tenantSchema(projectID)
	statement := fmt.Sprintf(`
SELECT version.id, COALESCE(version.agent_type, '')
FROM %s.application_versions AS version
JOIN %s.applications AS application ON application.id = version.application_id
WHERE version.application_id = $1
ORDER BY CASE
             WHEN application.meta ->> 'default_version_id' = version.id::text THEN 0
             WHEN version.name = 'latest' THEN 1
             WHEN version.name = 'base' THEN 2
             ELSE 3
         END,
         version.created_at DESC, version.id DESC
LIMIT 1`, schema, schema)

	var resolved agentVersion
	err := s.pool.QueryRow(ctx, statement, agentID).Scan(&resolved.ID, &resolved.AgentType)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentVersion{}, errAgentVersionNotFound
	}
	if err != nil {
		return agentVersion{}, fmt.Errorf("support assistant: resolve agent version: %w", err)
	}
	return resolved, nil
}
