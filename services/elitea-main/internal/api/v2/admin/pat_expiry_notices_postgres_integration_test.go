package admin_test

// Issue #940 A3 acceptance — the personal-access-token expiry notice
// (ELITEA-0750, 0751, 0752, 0754, 0755, 0756).
//
// It drives the ROUTE, not the repository, because the whole chain is the
// claim: the handler, the notifier and the SQL predicate each have a way to be
// individually right and jointly produce nothing (or produce it twice). And it
// runs against the real ledgered corpus — 001_initial.sql plus every shared
// migration — rather than a hand-made table, for the reason
// import_toolkit_owner_postgres_integration_test.go states at length: a
// hand-written table cannot carry a constraint nobody transcribed, so it can
// only ever agree with the statement under test.
//
// The fixture uses four DIFFERENT tokens on purpose. A producer that ignored
// its predicates entirely — announced every token it found — passes any test
// built from one eligible token.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/patexpiry"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	migrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	patExpiryOwnerID     = 9101 // the token owner: the only person who may be told
	patExpiryBystanderID = 9102 // a second user, who must be told nothing (ELITEA-0756)
	patExpiryProjectID   = 9100
)

type patExpiryRunResult struct {
	Examined int `json:"examined"`
	Produced int `json:"produced"`
	Skipped  int `json:"skipped"`
}

type patExpiryNotificationRow struct {
	UserID    int64
	EventType string
	Meta      map[string]any
}

// patExpiryFixtureSQL seeds two users, a project, and FOUR tokens that differ
// only in the property each predicate reads:
//
//	9001 "ci-deploy-key"  — owner, expires in 30 days, minted 40 days ago.
//	                        Outside a 24-hour window, inside a 60-day one, and
//	                        its total lifetime (70 days) is longer than either.
//	                        This is the one that must be announced.
//	9002 "short-lived"    — owner, expires in 30 days, minted TODAY. Its whole
//	                        lifetime is 30 days, so it is eligible under a
//	                        24-hour window but NOT under a 60-day one:
//	                        ELITEA-0755's rule, expressed relative to the same
//	                        window the look-ahead uses.
//	9003 "never-expires"  — owner, `expires` NULL. Never announced.
//	9004 "bystander-key"  — the OTHER user, same shape as 9001. Announced to
//	                        its own owner and to nobody else.
const patExpiryFixtureSQL = `
INSERT INTO auth_core__user (id, email, name) VALUES
    (9101, 'pat-owner@autotest.local', 'PAT Owner'),
    (9102, 'pat-bystander@autotest.local', 'PAT Bystander');
INSERT INTO centry.project (id, name, owner_id, create_success)
VALUES (9100, 'pat expiry fixture', 9101, true);
INSERT INTO auth_core__token (id, uuid, expires, user_id, name) VALUES
    (9001, '00000000-0000-4000-8000-000000009001', NOW() + INTERVAL '30 days', 9101, 'ci-deploy-key'),
    (9002, '00000000-0000-4000-8000-000000009002', NOW() + INTERVAL '30 days', 9101, 'short-lived'),
    (9003, '00000000-0000-4000-8000-000000009003', NULL,                       9101, 'never-expires'),
    (9004, '00000000-0000-4000-8000-000000009004', NOW() + INTERVAL '30 days', 9102, 'bystander-key');
INSERT INTO elitea_identity.token_lifecycle (token_id, issued_at) VALUES
    (9001, NOW() - INTERVAL '40 days'),
    (9002, NOW()),
    (9004, NOW() - INTERVAL '40 days');
`

func seedPATExpiryFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), patExpiryFixtureSQL); err != nil {
		t.Fatalf("seed the personal access token expiry fixture: %v", err)
	}
}

// runPATExpiryPass posts the run-now route and returns its counts. It fails
// here on any non-200, so a refusal stops the test where it happened instead
// of surfacing later as "nothing was produced".
func runPATExpiryPass(t *testing.T, handler *admin.Handler, within string) patExpiryRunResult {
	t.Helper()
	url := "/admin/background_jobs/administration/pat_expiry_notices:run"
	if within != "" {
		url += "?within=" + within
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, url, nil)
	handler.RunPATExpiryNotices(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("run pass status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var result patExpiryRunResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode run result %q: %v", recorder.Body.String(), err)
	}
	return result
}

func readPATExpiryNotifications(t *testing.T, pool *pgxpool.Pool) []patExpiryNotificationRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT user_id, event_type, meta
FROM centry.notifications
WHERE event_type = $1
ORDER BY id`, repos.PATExpiryNotificationEventType)
	if err != nil {
		t.Fatalf("read the produced notifications: %v", err)
	}
	defer rows.Close()

	produced := make([]patExpiryNotificationRow, 0)
	for rows.Next() {
		var row patExpiryNotificationRow
		var meta []byte
		if err := rows.Scan(&row.UserID, &row.EventType, &meta); err != nil {
			t.Fatalf("scan a produced notification: %v", err)
		}
		if err := json.Unmarshal(meta, &row.Meta); err != nil {
			t.Fatalf("decode a produced notification's meta %q: %v", string(meta), err)
		}
		produced = append(produced, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the produced notifications: %v", err)
	}
	return produced
}

func newPATExpiryHandler(t *testing.T, pool *pgxpool.Pool) *admin.Handler {
	t.Helper()
	store, err := repos.NewPATExpiryNotificationRepository(pool)
	if err != nil {
		t.Fatalf("construct the expiry repository: %v", err)
	}
	notifier, err := patexpiry.New(store)
	if err != nil {
		t.Fatalf("construct the expiry notifier: %v", err)
	}
	return admin.NewHandler(pool, admin.WithPATExpiryNotifier(notifier))
}

// TestPATExpiryNoticeAnnouncesOneTokenToItsOwnerOnly is ELITEA-0750, 0751,
// 0752 and 0756 in one pass: the eligible token produces exactly one
// notification, it carries the token's NAME and the Settings › Tokens link,
// and it belongs to the owner and to nobody else.
func TestPATExpiryNoticeAnnouncesOneTokenToItsOwnerOnly(t *testing.T) {
	pool := newPATExpiryCorpusPool(t)
	seedPATExpiryFixture(t, pool)
	handler := newPATExpiryHandler(t, pool)

	// A 60-day look-ahead, so token 9001 (30 days left, 70-day lifetime) is
	// inside it and token 9002 (30 days left, 30-day lifetime) is excluded by
	// the lifetime rule rather than by the window.
	result := runPATExpiryPass(t, handler, "1440h")
	if result.Produced != 2 {
		t.Fatalf("produced = %d, want 2 (one per owner of an eligible token): %+v", result.Produced, result)
	}

	produced := readPATExpiryNotifications(t, pool)
	if len(produced) != 2 {
		t.Fatalf("%d notification rows, want 2: %+v", len(produced), produced)
	}

	byUser := map[int64]patExpiryNotificationRow{}
	for _, row := range produced {
		if _, duplicate := byUser[row.UserID]; duplicate {
			t.Fatalf("user %d got two notifications from one pass", row.UserID)
		}
		byUser[row.UserID] = row
	}
	owner, ok := byUser[patExpiryOwnerID]
	if !ok {
		t.Fatalf("the token owner got nothing: %+v", produced)
	}
	// ELITEA-0756: the bystander's row is about the BYSTANDER's own token, and
	// there is exactly one of it. The owner's token never reaches them.
	bystander, ok := byUser[patExpiryBystanderID]
	if !ok {
		t.Fatalf("the second user's own token produced nothing: %+v", produced)
	}
	if name, _ := bystander.Meta["token_name"].(string); name != "bystander-key" {
		t.Errorf("the second user was told about %q, want their own bystander-key", name)
	}

	// ELITEA-0750: the message names the token and says what happens.
	message, _ := owner.Meta["message"].(string)
	if !strings.Contains(message, "ci-deploy-key") {
		t.Errorf("message does not name the token: %q", message)
	}
	if !strings.Contains(message, "expire") {
		t.Errorf("message does not say the token expires: %q", message)
	}
	// ELITEA-0751: the action link. The client resolves an EMPTY href on this
	// event type to /settings/tokens (features/notifications/lib/routes.ts),
	// so what the producer must write is the `[text]()` segment — a real URL
	// here would render as literal text instead of a link.
	if !strings.Contains(message, "[Manage Personal Access Tokens]()") {
		t.Errorf("message carries no resolvable action link: %q", message)
	}
	// token_name is written as well as message: it is what the LEGACY renderer
	// reads when `message` is absent. Writing one and not the other renders
	// correctly on one path and blankly on the other.
	if name, _ := owner.Meta["token_name"].(string); name != "ci-deploy-key" {
		t.Errorf("meta.token_name = %q, want ci-deploy-key", name)
	}
}

// TestPATExpiryNoticeIsNotRepeatedOnASecondPass is ELITEA-0754. The second
// pass must EXAMINE nothing — the dedupe is in the candidate query, not in a
// duplicate-insert failure downstream of it.
func TestPATExpiryNoticeIsNotRepeatedOnASecondPass(t *testing.T) {
	pool := newPATExpiryCorpusPool(t)
	seedPATExpiryFixture(t, pool)
	handler := newPATExpiryHandler(t, pool)

	first := runPATExpiryPass(t, handler, "1440h")
	if first.Produced == 0 {
		t.Fatalf("the first pass produced nothing, so the second proves nothing: %+v", first)
	}
	second := runPATExpiryPass(t, handler, "1440h")
	if second.Produced != 0 || second.Examined != 0 {
		t.Errorf("second pass = %+v, want nothing examined and nothing produced", second)
	}
	if rows := readPATExpiryNotifications(t, pool); len(rows) != first.Produced {
		t.Errorf("%d notification rows after two passes, want %d", len(rows), first.Produced)
	}
}

// TestPATExpiryNoticeSkipsATokenWhoseWholeLifetimeIsInsideTheWindow is
// ELITEA-0755. Token 9002 has the same expiry as 9001 and differs only in
// having been minted today, so the pass can only tell them apart by reading
// `issued_at` — the fact auth_core__token does not carry and shared migration
// 0125 adds beside it.
func TestPATExpiryNoticeSkipsATokenWhoseWholeLifetimeIsInsideTheWindow(t *testing.T) {
	pool := newPATExpiryCorpusPool(t)
	seedPATExpiryFixture(t, pool)
	handler := newPATExpiryHandler(t, pool)

	runPATExpiryPass(t, handler, "1440h")

	for _, row := range readPATExpiryNotifications(t, pool) {
		if name, _ := row.Meta["token_name"].(string); name == "short-lived" {
			t.Fatalf("a token whose whole lifetime is inside the window was announced: %+v", row)
		}
		if name, _ := row.Meta["token_name"].(string); name == "never-expires" {
			t.Fatalf("a token with no expiry was announced: %+v", row)
		}
	}
}

// TestPATExpiryNoticeProductionWindowExcludesADistantExpiry pins the DEFAULT.
// Every other test here widens the window to make an eligible token possible
// at all; without this one, nothing would check that the shipped rule is 24
// hours rather than the value a test happened to pass.
func TestPATExpiryNoticeProductionWindowExcludesADistantExpiry(t *testing.T) {
	pool := newPATExpiryCorpusPool(t)
	seedPATExpiryFixture(t, pool)
	handler := newPATExpiryHandler(t, pool)

	// No `within` at all — the route falls back to patexpiry.DefaultWindow.
	if result := runPATExpiryPass(t, handler, ""); result.Examined != 0 || result.Produced != 0 {
		t.Errorf("the 24-hour window examined %+v, want nothing: every fixture token expires in 30 days", result)
	}

	// The same tokens, moved to within a day, ARE announced — otherwise the
	// assertion above would pass for a pass that can never find anything.
	if _, err := pool.Exec(context.Background(),
		`UPDATE auth_core__token SET expires = NOW() + INTERVAL '3 hours' WHERE id = 9001`); err != nil {
		t.Fatalf("move the token inside the window: %v", err)
	}
	if result := runPATExpiryPass(t, handler, ""); result.Produced != 1 {
		t.Errorf("produced = %d with a token 3 hours from expiry, want 1: %+v", result.Produced, result)
	}
}

// TestPATExpiryRunNowRefusesAnUnparseableWindow guards the operator control
// itself: an unreadable `within` is refused, not silently replaced by the
// default. A caller who asked for a window they did not get would read the
// counts as an answer about the window they named.
func TestPATExpiryRunNowRefusesAnUnparseableWindow(t *testing.T) {
	pool := newPATExpiryCorpusPool(t)
	handler := newPATExpiryHandler(t, pool)

	for _, within := range []string{"soon", "0s", "-4h", "9000h"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost,
			"/admin/background_jobs/administration/pat_expiry_notices:run?within="+within, nil)
		handler.RunPATExpiryNotices(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("within=%q status = %d, want 400 (body %s)", within, recorder.Code, recorder.Body.String())
		}
	}
}

// TestPATExpiryRunNowSaysSoWhenItHasNoProducer is the 503 the option's own
// doc comment promises. "This deployment cannot run the pass" and "the pass
// ran and nobody was due" must not both render as 200 with zero produced.
func TestPATExpiryRunNowSaysSoWhenItHasNoProducer(t *testing.T) {
	handler := admin.NewHandler(nil)
	recorder := httptest.NewRecorder()
	handler.RunPATExpiryNotices(recorder,
		httptest.NewRequest(http.MethodPost, "/admin/background_jobs/administration/pat_expiry_notices:run", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body.String())
	}
	var failure map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode refusal %q: %v", recorder.Body.String(), err)
	}
	if failure["code"] != "pat_expiry_notifier_unavailable" {
		t.Errorf("refusal code = %v, want pat_expiry_notifier_unavailable", failure["code"])
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

// newPATExpiryCorpusPool builds a throwaway database holding what a deployed
// installation holds: the bootstrap schema plus every ledgered shared
// migration, applied by the runner elitea-migrate uses. No hand-written CREATE
// TABLE — 0125's guarded foreign key onto the pylon-owned auth_core__token is
// one of the things under test, and a hand-made table would not have it.
func newPATExpiryCorpusPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_pat_expiry_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated integration database: %v", err)
		}
		adminPool.Close()
	})

	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("apply the bootstrap schema: %v", err)
	}
	if err := migrate.New(pool, migrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations: %v", err)
	}
	return pool
}
