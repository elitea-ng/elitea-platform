package repos

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialuse"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Shared 0208 (ADR-0044 decision 5, issue #1501): the credential inventory,
// its last use, the staleness routine and the permission trimming report.
// CI-only: skipped without the integration database. The fixture is the
// agent releases fixture (project 1, agent 31, draft version 41, toolkit 51
// selecting list_products of list_products and list_features) plus a
// personal access token, a vault secret placeholder and step records.

const reviewTestSecret = "plain-secret"

func seedCredentialReviewFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedAgentReleaseFixture(t, pool)
	if _, err := pool.Exec(t.Context(), `
ALTER TABLE centry.project ADD COLUMN owner_id INTEGER NOT NULL DEFAULT 11;
-- The real tenant mapping's timestamps and upsert key (001_initial.sql).
ALTER TABLE p_1.entity_tool_mapping
    ADD COLUMN created_at TIMESTAMP, ADD COLUMN updated_at TIMESTAMP,
    ADD CONSTRAINT _entity_tool_unique UNIQUE (entity_version_id, tool_id, entity_type);
CREATE TABLE IF NOT EXISTS public.auth_core__token (
    id SERIAL PRIMARY KEY, uuid VARCHAR(36) UNIQUE, expires TIMESTAMP,
    user_id INTEGER, name TEXT
);
INSERT INTO public.auth_core__token (id, uuid, expires, user_id, name) VALUES
    (501, 'b0b0b0b0-0000-4000-8000-000000000501', NULL, 11, 'ci-token'),
    (502, 'b0b0b0b0-0000-4000-8000-000000000502', NULL, 12, 'old-token');
INSERT INTO elitea_identity.token_lifecycle (token_id, issued_at) VALUES
    (501, now() - interval '10 days'),
    (502, now() - interval '200 days');
-- A toolkit naming a vault secret by placeholder: the value is never in
-- the database, the configuration value below is a stored secret the
-- inventory must not return either.
UPDATE p_1.elitea_tools
SET settings = settings || '{"api_key":"{{secret.aha_token}}"}'::jsonb
WHERE id = 51;`); err != nil {
		t.Fatal(err)
	}
}

func newCredentialReviewTestStore(t *testing.T, pool *pgxpool.Pool, now time.Time, policy platformconfig.CredentialReviewPolicy) *CredentialReview {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main-test")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewCredentialReview(pool, appender, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.WithClock(func() time.Time { return now })
	store.policy = func(context.Context) (platformconfig.CredentialReviewPolicy, error) { return policy, nil }
	return store
}

func reviewAuditActions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT event->'data'->>'action' FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 ORDER BY seq`, string(securityaudit.TypeCredentialReview))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
	}
	return actions
}

func findCredential(rows []InventoryCredential, kind credentialuse.Kind, id string) *InventoryCredential {
	for i := range rows {
		if rows[i].Kind == kind && rows[i].CredentialID == id {
			return &rows[i]
		}
	}
	return nil
}

// Two uses inside an hour are one write, through the recorder and through
// the SQL guard a second replica meets.
func TestCredentialLastUse_TwoUsesInAnHourAreOneWrite(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	now := time.Now().UTC().Truncate(time.Second)
	store := newCredentialReviewTestStore(t, pool, now, platformconfig.DefaultCredentialReviewPolicy())
	recorder := credentialuse.New(store, nil).WithClock(func() time.Time { return now })

	recorder.TouchID(credentialuse.KindPAT, 501, 0)
	now = now.Add(20 * time.Minute)
	recorder.TouchID(credentialuse.KindPAT, 501, 0)
	written, err := recorder.Flush(t.Context())
	if err != nil || written != 1 {
		t.Fatalf("first flush wrote %d (err %v), want 1", written, err)
	}
	now = now.Add(20 * time.Minute)
	recorder.TouchID(credentialuse.KindPAT, 501, 0)
	if written, err := recorder.Flush(t.Context()); err != nil || written != 0 {
		t.Fatalf("a use inside the hour wrote %d (err %v)", written, err)
	}

	// Another replica (its own recorder) inside the same hour: the SQL guard
	// keeps the stored value.
	at := now.Add(-40 * time.Minute)
	if written, err := WriteCredentialLastUse(t.Context(), pool, []credentialuse.Use{
		{Kind: credentialuse.KindPAT, CredentialID: "501", At: at.Add(30 * time.Minute)},
	}); err != nil || written != 0 {
		t.Fatalf("a second replica inside the hour wrote %d (err %v)", written, err)
	}
	var stored time.Time
	if err := pool.QueryRow(t.Context(), `
SELECT last_used_at FROM elitea_identity.credential_last_use WHERE kind = 'pat' AND credential_id = '501'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(at) {
		t.Fatalf("last_used_at = %s, want the first use %s", stored, at)
	}
	// After the hour, the next use is written.
	if written, err := WriteCredentialLastUse(t.Context(), pool, []credentialuse.Use{
		{Kind: credentialuse.KindPAT, CredentialID: "501", At: at.Add(61 * time.Minute)},
	}); err != nil || written != 1 {
		t.Fatalf("a use after the hour wrote %d (err %v)", written, err)
	}
}

// The inventory lists every kind it finds with its owner, project, agents
// and age, and never a secret value.
func TestCredentialInventory_ListsCredentialsWithoutSecrets(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCredentialReviewFixture(t, pool)
	now := time.Now().UTC()
	store := newCredentialReviewTestStore(t, pool, now, platformconfig.DefaultCredentialReviewPolicy())

	// No pass has seen the credentials yet: nothing is judged stale, not
	// even PAT 502 issued 200 days ago (its use before 0208 is unknown).
	unseen, _, err := store.Inventory(t.Context(), InventoryFilter{Kind: credentialuse.KindPAT})
	if err != nil {
		t.Fatal(err)
	}
	if old := findCredential(unseen, credentialuse.KindPAT, "502"); old == nil || old.Stale {
		t.Fatalf("pat 502 before any pass = %+v, want listed and not stale", old)
	}
	// A pass 100 days ago saw them: PAT 502's clock started then, PAT 501's
	// at its creation 10 days ago (the later of the two).
	earlier := newCredentialReviewTestStore(t, pool, now.Add(-100*24*time.Hour), platformconfig.DefaultCredentialReviewPolicy())
	if _, err := earlier.StalenessPass(t.Context()); err != nil {
		t.Fatal(err)
	}

	rows, total, err := store.Inventory(t.Context(), InventoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != len(rows) || total < 3 {
		t.Fatalf("total = %d, rows = %+v", total, rows)
	}
	pat := findCredential(rows, credentialuse.KindPAT, "501")
	if pat == nil || derefInt(pat.OwnerID) != 11 || pat.Name != "ci-token" || pat.CreatedAt == nil || pat.Stale || !pat.Revocable {
		t.Fatalf("pat 501 = %+v", pat)
	}
	old := findCredential(rows, credentialuse.KindPAT, "502")
	if old == nil || !old.Stale {
		t.Fatalf("pat 502 unused since a pass saw it 100 days ago is not stale: %+v", old)
	}
	secret := findCredential(rows, credentialuse.KindToolkitSecret, "1:aha_token")
	if secret == nil || secret.Revocable || derefInt(secret.ProjectID) != 1 || len(secret.Agents) != 1 ||
		secret.Agents[0].ApplicationID != 31 || secret.Agents[0].Name != "Parent Agent" {
		t.Fatalf("toolkit secret = %+v", secret)
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), reviewTestSecret) || strings.Contains(string(encoded), "{{secret.") {
		t.Fatalf("the inventory returned a secret value or settings: %s", encoded)
	}

	stale := true
	staleRows, _, err := store.Inventory(t.Context(), InventoryFilter{Kind: credentialuse.KindPAT, Stale: &stale})
	if err != nil {
		t.Fatal(err)
	}
	if len(staleRows) != 1 || staleRows[0].CredentialID != "502" {
		t.Fatalf("stale PATs = %+v", staleRows)
	}
	project, _, err := store.Inventory(t.Context(), InventoryFilter{ProjectID: 1, Kind: credentialuse.KindToolkitSecret})
	if err != nil || len(project) != 1 {
		t.Fatalf("project 1 secrets = %+v (err %v)", project, err)
	}
}

// The routine flags a stale credential once and notifies its owner once;
// under revoke it revokes a stale PAT (audited, owner notified) and only
// flags a stale toolkit secret.
func TestCredentialStaleness_FlagsThenRevokesWithNotification(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCredentialReviewFixture(t, pool)
	now := time.Now().UTC()
	policy := platformconfig.DefaultCredentialReviewPolicy()
	store := newCredentialReviewTestStore(t, pool, now, policy)

	summary, err := store.StalenessPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The first pass only starts the clocks: last use is unknown before it.
	if summary.Flagged != 0 || summary.Revoked != 0 || summary.Notified != 0 {
		t.Fatalf("first pass = %s", summary)
	}
	// 91 days on, nothing used: both PATs and the toolkit secret are stale.
	flagging := newCredentialReviewTestStore(t, pool, now.Add(91*24*time.Hour), policy)
	summary, err = flagging.StalenessPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Flagged != 3 || summary.Revoked != 0 || summary.Notified < 2 {
		t.Fatalf("flag pass = %s", summary)
	}
	again, err := flagging.StalenessPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if again.Flagged != 0 || again.Notified != 0 {
		t.Fatalf("a second pass flagged or notified again: %s", again)
	}
	var notices int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM centry.notifications WHERE event_type = $1 AND user_id = 12`, CredentialStaleNotificationEventType).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices != 1 {
		t.Fatalf("stale notices to the owner = %d, want 1", notices)
	}

	// 100 days on under revoke: both PATs are revoked (deleted); the
	// toolkit secret stays flagged only.
	later := now.Add(100 * 24 * time.Hour)
	policy.StaleAction = platformconfig.StaleActionRevoke
	revoking := newCredentialReviewTestStore(t, pool, later, policy)
	summary, err = revoking.StalenessPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Revoked != 2 || summary.Errors != 0 {
		t.Fatalf("revoke pass = %s", summary)
	}
	var tokens int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.auth_core__token WHERE id IN (501, 502)`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Fatalf("%d stale tokens were not revoked", tokens)
	}
	var secretFlagged, secretRevoked bool
	if err := pool.QueryRow(t.Context(), `
SELECT flagged_at IS NOT NULL, revoked_at IS NOT NULL FROM elitea_identity.credential_review_state
WHERE kind = 'toolkit_secret' AND credential_id = '1:aha_token'`).Scan(&secretFlagged, &secretRevoked); err != nil {
		t.Fatal(err)
	}
	if !secretFlagged || secretRevoked {
		t.Fatalf("toolkit secret flagged=%v revoked=%v, want flagged only", secretFlagged, secretRevoked)
	}
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM centry.notifications WHERE event_type = $1`, CredentialRevokedNotificationEventType).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices != 2 {
		t.Fatalf("revoked notices = %d, want 2", notices)
	}
	actions := strings.Join(reviewAuditActions(t, pool), ",")
	if strings.Count(actions, "credential.revoked") != 2 || !strings.Contains(actions, "credential.stale.flagged") {
		t.Fatalf("audit actions = %s", actions)
	}
	// A revoked PAT is evidence in the review state, not in the inventory.
	rows, _, err := revoking.Inventory(t.Context(), InventoryFilter{Kind: credentialuse.KindPAT})
	if err != nil || len(rows) != 0 {
		t.Fatalf("PATs after revoke = %+v (err %v)", rows, err)
	}
	if _, err := revoking.Revoke(t.Context(), RevokeRequest{Kind: credentialuse.KindToolkitSecret, CredentialID: "1:aha_token", ActorID: 11}); err != ErrCredentialNotRevocable {
		t.Fatalf("revoking a toolkit secret = %v, want ErrCredentialNotRevocable", err)
	}
}

func seedTrimmingSteps(t *testing.T, pool *pgxpool.Pool, at time.Time, tools ...string) {
	t.Helper()
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.run_step_heads (run_id, project_id, application_id, last_seq, updated_at)
VALUES ('trim-run', 1, 31, $1, $2)
ON CONFLICT (run_id) DO NOTHING`, len(tools), at); err != nil {
		t.Fatal(err)
	}
	for i, tool := range tools {
		if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.run_steps (run_id, seq, step_id, generation, project_id, application_id, kind, actor,
    name, toolkit_id, started_at, ended_at, prev_hash, hash)
VALUES ('trim-run', $1, gen_random_uuid(), 1, 1, 31, 'tool_call', 'user:11', $2, '51', $3, $3,
    decode(repeat('00', 32), 'hex'), decode(repeat('11', 32), 'hex'))`, i+1, tool, at); err != nil {
			t.Fatal(err)
		}
	}
}

func findTrim(items []TrimmingItem, kind, key string) *TrimmingItem {
	for i := range items {
		if items[i].Kind == kind && items[i].Key == key {
			return &items[i]
		}
	}
	return nil
}

// The report compares the agent's toolkit and selected tools with the step
// records in the window; a removal goes through the toolkit relation path,
// is audited and resolves the candidate.
func TestPermissionTrimming_ReportFromStepsAndAuditedRemoval(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCredentialReviewFixture(t, pool)
	// The agent selects both tools of the toolkit.
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.elitea_tools SET settings = settings || '{"selected_tools":["list_products","list_features"]}'::jsonb WHERE id = 51;
UPDATE p_1.entity_tool_mapping SET selected_tools = '["list_products","list_features"]'::jsonb
WHERE entity_version_id = 41 AND tool_id = 51;`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedTrimmingSteps(t, pool, now.Add(-2*24*time.Hour), "Product___list_products")
	review := newCredentialReviewTestStore(t, pool, now, platformconfig.DefaultCredentialReviewPolicy())
	trimming, err := NewPermissionTrimming(review)
	if err != nil {
		t.Fatal(err)
	}

	report, err := trimming.Report(t.Context(), TrimmingFilter{ProjectID: 1, ApplicationID: 31})
	if err != nil {
		t.Fatal(err)
	}
	if report.WindowDays != 30 {
		t.Fatalf("window = %d", report.WindowDays)
	}
	toolkit := findTrim(report.Items, TrimToolkit, "41/51")
	used := findTrim(report.Items, TrimTool, "41/51/list_products")
	unused := findTrim(report.Items, TrimTool, "41/51/list_features")
	if toolkit == nil || !toolkit.Used || used == nil || !used.Used || used.Uses != 1 || unused == nil || !unused.Candidate || !unused.Removable {
		t.Fatalf("report = %+v", report.Items)
	}

	if _, err := trimming.Remove(t.Context(), RemoveRequest{ProjectID: 1, ApplicationID: 31, Kind: TrimTool,
		Key: "41/51/list_features", ActorID: 11}); err != nil {
		t.Fatal(err)
	}
	var selected string
	if err := pool.QueryRow(t.Context(), `
SELECT selected_tools::text FROM p_1.entity_tool_mapping WHERE entity_version_id = 41 AND tool_id = 51`).Scan(&selected); err != nil {
		t.Fatal(err)
	}
	if selected != `["list_products"]` {
		t.Fatalf("selected_tools = %s", selected)
	}
	// The last selected tool is not removable: an empty selection is every tool.
	if _, err := trimming.Remove(t.Context(), RemoveRequest{ProjectID: 1, ApplicationID: 31, Kind: TrimTool,
		Key: "41/51/list_products", ActorID: 11}); err == nil || !strings.Contains(err.Error(), "last selected tool") {
		t.Fatalf("removing the last tool = %v", err)
	}
	// A published version is refused by the shared relation guard.
	if _, err := pool.Exec(t.Context(), `UPDATE p_1.application_versions SET status = 'published' WHERE id = 41`); err != nil {
		t.Fatal(err)
	}
	if _, err := trimming.Remove(t.Context(), RemoveRequest{ProjectID: 1, ApplicationID: 31, Kind: TrimToolkit,
		Key: "41/51", ActorID: 11}); err == nil {
		t.Fatal("a published version's toolkit was removed")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE p_1.application_versions SET status = 'draft' WHERE id = 41`); err != nil {
		t.Fatal(err)
	}
	if _, err := trimming.Remove(t.Context(), RemoveRequest{ProjectID: 1, ApplicationID: 31, Kind: TrimToolkit,
		Key: "41/51", ActorID: 11}); err != nil {
		t.Fatal(err)
	}
	var mappings int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM p_1.entity_tool_mapping WHERE entity_version_id = 41`).Scan(&mappings); err != nil {
		t.Fatal(err)
	}
	if mappings != 0 {
		t.Fatalf("the toolkit is still attached")
	}
	if actions := reviewAuditActions(t, pool); len(actions) != 2 || actions[0] != "permission.trimmed" || actions[1] != "permission.trimmed" {
		t.Fatalf("audit actions = %v", actions)
	}
}

// The weekly pass notifies the agent's owners of new candidates only.
func TestPermissionTrimming_WeeklyPassNotifiesNewCandidatesOnly(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCredentialReviewFixture(t, pool)
	now := time.Now().UTC()
	review := newCredentialReviewTestStore(t, pool, now, platformconfig.DefaultCredentialReviewPolicy())
	trimming, err := NewPermissionTrimming(review)
	if err != nil {
		t.Fatal(err)
	}
	notices := func() int {
		var n int
		if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM centry.notifications WHERE event_type = $1`, PermissionTrimmingNotificationEventType).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	first, err := trimming.TrimmingPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Nothing ran: the toolkit and its one selected tool are candidates; the
	// agent's owner (11, no registry row) is told once.
	if first.Candidates != 2 || first.New != 2 || first.Notified != 1 || notices() != 1 {
		t.Fatalf("first pass = %s, notices = %d", first, notices())
	}
	second, err := trimming.TrimmingPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.New != 0 || second.Notified != 0 || notices() != 1 {
		t.Fatalf("second pass notified known candidates: %s", second)
	}
	// A second toolkit attached and unused: one new notification, naming
	// only the new candidate.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, meta, created_at)
VALUES (52, 'github', 'repo', 'GitHub', '{}'::jsonb, 11, '{}'::jsonb, now() AT TIME ZONE 'UTC');
INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type) VALUES (52, 31, 41, 'agent');`); err != nil {
		t.Fatal(err)
	}
	third, err := trimming.TrimmingPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if third.New != 1 || third.Notified != 1 || notices() != 2 {
		t.Fatalf("third pass = %s, notices = %d", third, notices())
	}
	var items string
	if err := pool.QueryRow(t.Context(), `
SELECT meta->'items'::text FROM centry.notifications WHERE event_type = $1 ORDER BY id DESC LIMIT 1`,
		PermissionTrimmingNotificationEventType).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(items, "toolkit 41/52") || strings.Contains(items, "41/51") {
		t.Fatalf("the new notice names %s, want only the new candidate", items)
	}
}
