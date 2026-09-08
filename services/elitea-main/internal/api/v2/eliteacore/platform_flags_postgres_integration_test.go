package eliteacore_test

// Unit A14 acceptance for the admin FEATURES page's flags (issue #200) — from
// the CONSUMER's end.
//
// The companion file `internal/api/v2/admin/features_postgres_integration_test.go`
// proves the page can write these rows. This one proves the rows change what the
// platform does, which is the only thing that makes the controls honest. A flag
// that round-trips through its own admin endpoint and nothing else is precisely
// the defect this unit exists to remove, and it is invisible to a test that only
// exercises the page.
//
// So every case here writes into `centry.platform_config` DIRECTLY — never
// through the admin handler — and then asserts on a PRODUCT endpoint. Going
// through the admin PUT would let a single shared helper satisfy both sides and
// prove nothing about the wiring between them.
//
// What was true before this unit, for each of the three:
//
//   - `mcp_enabled` — `PlatformSettings` returned a hardcoded `true` and the
//     three MCP proxy/sync routes never asked, so the field whose own
//     description promises it "removes all MCP-related functionality across the
//     entire application including API endpoints" removed nothing at all.
//   - `mcp_in_menu` — not on the wire in any form. All four `useIsMcpVisible`
//     hooks in apps/elitea-web carry a doc comment naming it as missing.
//   - `is_publish_blocked` / `publish_whitelist_project_ids` — `Publish`
//     validated the version name, the agent type and the publish status, and
//     never consulted the guardrail.
//   - `agent_categories` — `AgentCategories` looked for a `publishing_guardrail`
//     row in the PROJECT's `configuration` table. Nothing writes that row, so
//     the lookup could only ever miss and every deployment saw the nine
//     hardcoded defaults.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

/* ── harness ───────────────────────────────────────────────────────────── */

// flagsRouter mounts the product routes this page's flags are supposed to
// change, exactly as internal/api/router.go mounts them.
func flagsRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/elitea_core/platform_settings/{mode}", handler.PlatformSettings)
	router.Get("/elitea_core/platform_settings/{mode}/{projectID}", handler.PlatformSettings)
	router.Get("/elitea_core/agent_categories/{mode}/{projectID}", handler.AgentCategories)
	router.Post("/elitea_core/mcp_oauth_proxy/{projectID}", handler.MCPOAuthProxy)
	router.Post("/elitea_core/mcp_dcr_proxy/{projectID}", handler.MCPDCRProxy)
	router.Post("/elitea_core/mcp_sync_tools/prompt_lib/{projectID}", handler.MCPSyncTools)
	router.Post("/elitea_core/publish/prompt_lib/{projectID}/{versionID}", handler.Publish)
	return router
}

func flagsDo(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// setFlag writes one row the way the admin page's PUT would, but WITHOUT going
// through it — see the file header.
func setFlag(t *testing.T, pool *pgxpool.Pool, section, key string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s.%s: %v", section, key, err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO centry.platform_config (section, key, value)
		VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value`,
		section, key, string(encoded)); err != nil {
		t.Fatalf("set %s.%s: %v", section, key, err)
	}
}

func decodeMap(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return body
}

/* ── MCP: the master switch ────────────────────────────────────────────── */

// TestMCPMasterSwitchReachesPlatformSettings — the flag the UI gates on.
func TestMCPMasterSwitchReachesPlatformSettings(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))

	// Unconfigured: MCP is on, and the new key is present rather than absent,
	// so a client can tell "in menu" from "the deployment predates the field".
	body := decodeMap(t, flagsDo(t, router, http.MethodGet, "/elitea_core/platform_settings/prompt_lib", nil))
	if body["mcp_enabled"] != true {
		t.Errorf("mcp_enabled = %v on a fresh install, want true", body["mcp_enabled"])
	}
	if body["mcp_in_menu_enabled"] != true {
		t.Errorf("mcp_in_menu_enabled = %v on a fresh install, want true", body["mcp_in_menu_enabled"])
	}

	setFlag(t, pool, "mcp_configuration", "mcp_enabled", false)
	body = decodeMap(t, flagsDo(t, router, http.MethodGet, "/elitea_core/platform_settings/prompt_lib", nil))
	if body["mcp_enabled"] != false {
		t.Errorf("mcp_enabled = %v after the admin switched MCP off, want false", body["mcp_enabled"])
	}
	// Off implies out of the menu, whatever the second flag says. Otherwise a
	// client would render an MCP entry whose endpoints answer 403.
	if body["mcp_in_menu_enabled"] != false {
		t.Errorf("mcp_in_menu_enabled = %v while MCP is off, want false", body["mcp_in_menu_enabled"])
	}
}

// TestMCPInMenuIsIndependentWhileMCPIsOn — the finer distinction the reference
// had and this platform did not: API on, UI hidden.
func TestMCPInMenuIsIndependentWhileMCPIsOn(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))

	setFlag(t, pool, "mcp_configuration", "mcp_in_menu", false)

	body := decodeMap(t, flagsDo(t, router, http.MethodGet, "/elitea_core/platform_settings/prompt_lib", nil))
	if body["mcp_enabled"] != true {
		t.Errorf("mcp_enabled = %v, want true — hiding the menu must not disable the API", body["mcp_enabled"])
	}
	if body["mcp_in_menu_enabled"] != false {
		t.Errorf("mcp_in_menu_enabled = %v, want false", body["mcp_in_menu_enabled"])
	}
	// And the API really is still open.
	recorder := flagsDo(t, router, http.MethodPost, "/elitea_core/mcp_dcr_proxy/1",
		map[string]any{"registration_endpoint": "https://example.invalid/register"})
	if recorder.Code == http.StatusForbidden {
		t.Error("hiding MCP from the menu closed the API; the two switches are not independent")
	}
}

// TestMCPMasterSwitchClosesTheAPIRoutes is the half that cannot be done in the
// client, and the half whose absence made the field's own description false.
func TestMCPMasterSwitchClosesTheAPIRoutes(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))

	routes := []struct {
		target string
		body   any
	}{
		{"/elitea_core/mcp_oauth_proxy/1", map[string]any{"token_endpoint": "https://example.invalid/token"}},
		{"/elitea_core/mcp_dcr_proxy/1", map[string]any{"registration_endpoint": "https://example.invalid/register"}},
		{"/elitea_core/mcp_sync_tools/prompt_lib/1", map[string]any{"toolkit": "x"}},
	}

	// While on, none of them answers 403 — so the 403s below are the switch's
	// doing and not the route's ordinary behaviour.
	for _, route := range routes {
		if recorder := flagsDo(t, router, http.MethodPost, route.target, route.body); recorder.Code == http.StatusForbidden {
			t.Fatalf("%s answered 403 while MCP is ENABLED; the test cannot distinguish the gate", route.target)
		}
	}

	setFlag(t, pool, "mcp_configuration", "mcp_enabled", false)

	for _, route := range routes {
		recorder := flagsDo(t, router, http.MethodPost, route.target, route.body)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s status = %d with MCP disabled, want 403 (body %s)",
				route.target, recorder.Code, recorder.Body.String())
		}
	}
}

// TestMCPGateRefusesBeforeReadingTheBody — a malformed payload must get the same
// answer as a well-formed one, or the refusal itself becomes an oracle for which
// routes exist and what they parse.
func TestMCPGateRefusesBeforeReadingTheBody(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))
	setFlag(t, pool, "mcp_configuration", "mcp_enabled", false)

	request := httptest.NewRequest(http.MethodPost, "/elitea_core/mcp_sync_tools/prompt_lib/1",
		bytes.NewReader([]byte("this is not json")))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Errorf("malformed body with MCP disabled = %d, want the same 403 a valid one gets", recorder.Code)
	}
}

/* ── voice features ────────────────────────────────────────────────────── */

// TestVoiceFlagsReachPlatformSettings.
//
// The consumer is `widgets/chat/ui/chat-button/VoiceButton.tsx`, which is
// mounted on `/chat` through `ChatBox`'s slot bundle and which used to hardcode
// both of these as module constants — `true` and `false` — so the admin
// Features page's Voice Features section named a control it could not change.
func TestVoiceFlagsReachPlatformSettings(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))

	settings := func() map[string]any {
		return decodeMap(t, flagsDo(t, router, http.MethodGet, "/elitea_core/platform_settings/prompt_lib", nil))
	}

	// Unconfigured: voice on, not admin-disabled — and both keys PRESENT, so a
	// client can tell an unconfigured deployment from one that predates the
	// fields.
	body := settings()
	if body["voice_features_enabled"] != true {
		t.Errorf("voice_features_enabled = %v on a fresh install, want true", body["voice_features_enabled"])
	}
	if body["voice_features_temporarily_disabled"] != false {
		t.Errorf("voice_features_temporarily_disabled = %v, want false", body["voice_features_temporarily_disabled"])
	}

	// "Visible but disabled" — the state the button's admin tooltip exists for.
	setFlag(t, pool, "voice_features", "vite_voice_features_temporarily_disabled", true)
	body = settings()
	if body["voice_features_enabled"] != true {
		t.Errorf("temporarily disabling must not HIDE the control: enabled = %v", body["voice_features_enabled"])
	}
	if body["voice_features_temporarily_disabled"] != true {
		t.Errorf("voice_features_temporarily_disabled = %v, want true", body["voice_features_temporarily_disabled"])
	}

	// Off entirely. "Visible but disabled" is not a state a hidden control can
	// be in, so the second flag is reported false — a client that combined the
	// two itself would be inventing the rule.
	setFlag(t, pool, "voice_features", "vite_voice_features_enabled", false)
	body = settings()
	if body["voice_features_enabled"] != false {
		t.Errorf("voice_features_enabled = %v after switching voice off", body["voice_features_enabled"])
	}
	if body["voice_features_temporarily_disabled"] != false {
		t.Errorf("voice_features_temporarily_disabled = %v while voice is off, want false", body["voice_features_temporarily_disabled"])
	}
}

/* ── publishing: the guardrail ─────────────────────────────────────────── */

// seedPublishableVersion creates the minimum a publish attempt needs, so that a
// 403 below is the guardrail and not a missing row.
func seedPublishableVersion(t *testing.T, pool *pgxpool.Pool, projectID int, versionID int) {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("p_%d", projectID)
	statements := []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, schema),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %q.applications (id int PRIMARY KEY, name text)`, schema),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %q.application_versions (
			id int PRIMARY KEY, application_id int, name text, status text, agent_type text)`, schema),
		fmt.Sprintf(`INSERT INTO %q.applications (id, name) VALUES (1, 'agent')
			ON CONFLICT (id) DO NOTHING`, schema),
		fmt.Sprintf(`INSERT INTO %q.application_versions (id, application_id, name, status, agent_type)
			VALUES (%d, 1, 'base', 'draft', 'agent') ON CONFLICT (id) DO NOTHING`, schema, versionID),
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

// TestPublishIsRefusedWhilePublishingIsBlocked — the switch the reference page
// offered and this service ignored.
func TestPublishIsRefusedWhilePublishingIsBlocked(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))
	seedPublishableVersion(t, pool, 4, 40)

	publish := func() *httptest.ResponseRecorder {
		return flagsDo(t, router, http.MethodPost, "/elitea_core/publish/prompt_lib/4/40",
			map[string]any{"version_name": "v-one"})
	}

	// Unblocked, the request gets past the guardrail. It need not SUCCEED — the
	// seeded schema is a stub — but it must not be a 403, or the assertion
	// below would pass against a handler that refuses everything.
	if recorder := publish(); recorder.Code == http.StatusForbidden {
		t.Fatalf("publish refused while unblocked (body %s)", recorder.Body.String())
	}

	setFlag(t, pool, "agent_publishing", "is_publish_blocked", true)

	recorder := publish()
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("publish status = %d while blocked, want 403 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if got := decodeMap(t, recorder)["error"]; got != "publishing is blocked on this deployment" {
		t.Errorf("error = %v, want the guardrail's own sentence", got)
	}
}

// TestWhitelistedProjectMayStillPublish, and an empty whitelist blocks
// everybody — the reading that inverts this control if it is got wrong.
func TestWhitelistedProjectMayStillPublish(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))
	seedPublishableVersion(t, pool, 4, 40)
	seedPublishableVersion(t, pool, 5, 50)

	setFlag(t, pool, "agent_publishing", "is_publish_blocked", true)

	// Empty whitelist: nobody publishes.
	if got := flagsDo(t, router, http.MethodPost, "/elitea_core/publish/prompt_lib/4/40",
		map[string]any{"version_name": "v-one"}).Code; got != http.StatusForbidden {
		t.Fatalf("project 4 with an EMPTY whitelist = %d, want 403", got)
	}

	setFlag(t, pool, "agent_publishing", "publish_whitelist_project_ids", []int{4})

	if got := flagsDo(t, router, http.MethodPost, "/elitea_core/publish/prompt_lib/4/40",
		map[string]any{"version_name": "v-one"}).Code; got == http.StatusForbidden {
		t.Error("whitelisted project 4 was still refused")
	}
	if got := flagsDo(t, router, http.MethodPost, "/elitea_core/publish/prompt_lib/5/50",
		map[string]any{"version_name": "v-one"}).Code; got != http.StatusForbidden {
		t.Errorf("unlisted project 5 = %d, want 403", got)
	}
}

// TestGuardrailRefusesAProjectIdItCannotParse — the bypass a permissive parse
// would open. chi's `{projectID}` matches any segment, so a caller can send one
// `strconv` rejects; resolving that to 0 (or to "allowed") would let the block
// be walked around with a URL.
func TestGuardrailRefusesAProjectIdItCannotParse(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))
	setFlag(t, pool, "agent_publishing", "is_publish_blocked", true)
	setFlag(t, pool, "agent_publishing", "publish_whitelist_project_ids", []int{0, 4})

	recorder := flagsDo(t, router, http.MethodPost, "/elitea_core/publish/prompt_lib/not-a-number/40",
		map[string]any{"version_name": "v-one"})
	if recorder.Code != http.StatusForbidden {
		t.Errorf("unparseable project id = %d while blocked, want 403", recorder.Code)
	}
}

// TestGuardrailIgnoresANonIntegralWhitelistEntry — 4.5 is not project 4.
func TestGuardrailIgnoresANonIntegralWhitelistEntry(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))
	seedPublishableVersion(t, pool, 4, 40)
	setFlag(t, pool, "agent_publishing", "is_publish_blocked", true)
	setFlag(t, pool, "agent_publishing", "publish_whitelist_project_ids", []float64{4.5})

	if got := flagsDo(t, router, http.MethodPost, "/elitea_core/publish/prompt_lib/4/40",
		map[string]any{"version_name": "v-one"}).Code; got != http.StatusForbidden {
		t.Errorf("project 4 = %d with only 4.5 whitelisted, want 403", got)
	}
}

/* ── agent categories ──────────────────────────────────────────────────── */

// TestAdminAgentCategoriesReachTheHub — the read `useAgentHubData` performs.
func TestAdminAgentCategoriesReachTheHub(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))
	seedPublishableVersion(t, pool, 4, 40) // creates p_4 so the project read does not error

	names := func() map[string]bool {
		recorder := flagsDo(t, router, http.MethodGet, "/elitea_core/agent_categories/prompt_lib/4", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("agent_categories status = %d (body %s)", recorder.Code, recorder.Body.String())
		}
		var body struct {
			Categories []struct {
				Name      string `json:"name"`
				IsDefault bool   `json:"is_default"`
			} `json:"categories"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[string]bool{}
		for _, category := range body.Categories {
			out[category.Name] = category.IsDefault
		}
		return out
	}

	before := names()
	if _, present := before["Security Review"]; present {
		t.Fatal("the category exists before it was configured")
	}
	if !before["Development"] {
		t.Error("a built-in default is missing or not marked as one")
	}

	setFlag(t, pool, "agent_publishing", "agent_categories", []string{"Security Review", "Development"})

	after := names()
	isDefault, present := after["Security Review"]
	if !present {
		t.Fatal("a category configured on the admin Features page never reached the hub")
	}
	if isDefault {
		t.Error("an operator-added category is marked is_default")
	}
	// A configured name that duplicates a built-in must not appear twice.
	count := 0
	for name := range after {
		if name == "Development" {
			count++
		}
	}
	if count != 1 || !after["Development"] {
		t.Error("a configured name colliding with a built-in changed the built-in or duplicated it")
	}
}

// TestABlankCategoryNeverReachesTheHub.
//
// `Values.Strings` drops empty entries, and this is the test that says so. It
// exists because a mutation removing that filter survived everything else:
// nothing asserted what an empty category does, so the defence was real and
// unpinned.
//
// It is reachable. The page's editor drops blank rows on save
// (`withoutBlankListEntries`) and the server's `validateArrayItems` checks the
// element TYPE and not its emptiness, so `{"agent_categories": [""]}` is a
// well-typed body the admin endpoint accepts. Without the read-side filter that
// row becomes a nameless entry in the Agents Hub filter bar — a control the
// operator cannot see, name, or work out how to remove.
func TestABlankCategoryNeverReachesTheHub(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))
	seedPublishableVersion(t, pool, 4, 40)

	setFlag(t, pool, "agent_publishing", "agent_categories",
		[]string{"", "   ", "Security Review"})

	recorder := flagsDo(t, router, http.MethodGet, "/elitea_core/agent_categories/prompt_lib/4", nil)
	var body struct {
		Categories []struct {
			Name string `json:"name"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, category := range body.Categories {
		if category.Name == "" {
			t.Error("an empty category name reached the hub")
		}
	}
	// The real one still arrives — a filter that dropped everything would pass
	// the assertion above and be useless.
	found := false
	for _, category := range body.Categories {
		if category.Name == "Security Review" {
			found = true
		}
	}
	if !found {
		t.Error("the non-empty category was dropped along with the blanks")
	}
}

/* ── failure is permissive, deliberately ───────────────────────────────── */

// TestAnUnreadableStoreDoesNotDisableMCPOrBlockPublishing.
//
// These reads sit on ordinary product traffic. Resolving a store failure to
// "MCP off" or "publishing blocked" would turn a database fault into a
// platform-wide outage of a subsystem nobody switched off — so the flags fail
// OPEN here, which is the opposite of the admin page's own read
// (`config_values.go` reports the failure, because an operator editing
// configuration must never be shown defaults as if they were stored state).
func TestAnUnreadableStoreDoesNotDisableMCPOrBlockPublishing(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))
	seedPublishableVersion(t, pool, 4, 40)

	if _, err := pool.Exec(context.Background(), `DROP TABLE centry.platform_config`); err != nil {
		t.Fatalf("drop the store: %v", err)
	}

	body := decodeMap(t, flagsDo(t, router, http.MethodGet, "/elitea_core/platform_settings/prompt_lib", nil))
	if body["mcp_enabled"] != true {
		t.Errorf("mcp_enabled = %v with an unreadable store, want true", body["mcp_enabled"])
	}
	if body["voice_features_enabled"] != true {
		t.Errorf("voice_features_enabled = %v with an unreadable store, want true", body["voice_features_enabled"])
	}
	if got := flagsDo(t, router, http.MethodPost, "/elitea_core/publish/prompt_lib/4/40",
		map[string]any{"version_name": "v-one"}).Code; got == http.StatusForbidden {
		t.Error("an unreadable store blocked publishing")
	}
}

/* ── pool ──────────────────────────────────────────────────────────────── */

func newFlagsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_flags_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
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
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	// Only the platform-config table is needed; each test seeds whatever
	// project schema it exercises.
	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS centry;
		CREATE TABLE centry.platform_config (
			section    text NOT NULL,
			key        text NOT NULL,
			value      jsonb NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT now(),
			updated_by text,
			PRIMARY KEY (section, key)
		);`); err != nil {
		t.Fatalf("create centry.platform_config: %v", err)
	}
	return pool
}

// TestMaintenanceSplashHTMLReachesThePublicSettings is the splash-body port,
// from the consumer's end.
//
// pylon stored `splash_template` in a tunable and served it as a whole document
// (legacy/plugins/bootstrap/tools/splash.py). Here it is one
// `centry.platform_config` row that the SPA's app shell and the standalone
// splash entry BOTH read off `platform_settings`, so the two screens cannot
// show different words for the same window.
//
// The row is written directly, never through the admin PUT — see the file
// header for why every case in this file does that.
func TestMaintenanceSplashHTMLReachesThePublicSettings(t *testing.T) {
	pool := newFlagsPool(t)
	router := flagsRouter(eliteacore.NewHandler(pool))

	read := func() map[string]any {
		t.Helper()
		body := decodeMap(t, flagsDo(t, router, http.MethodGet,
			"/elitea_core/platform_settings/prompt_lib", nil))
		maintenance, ok := body["maintenance"].(map[string]any)
		if !ok {
			t.Fatalf("platform_settings carries no `maintenance` object: %v", body)
		}
		return maintenance
	}

	t.Run("an unconfigured deployment reports an empty body, never a default one", func(t *testing.T) {
		// A default here would put THIS platform's markup inside a field whose
		// whole purpose is to hold the operator's. Empty is how a renderer
		// knows to fall back to its own splash.
		maintenance := read()
		if html, _ := maintenance["html"].(string); html != "" {
			t.Fatalf("an unconfigured deployment reports a splash body %q, want empty", html)
		}
	})

	t.Run("the stored body is published verbatim", func(t *testing.T) {
		const body = `<p>Upgrading the database.</p><p><a href="https://status.example.com">Status</a></p>`
		setFlag(t, pool, "maintenance", "maintenance_enabled", true)
		setFlag(t, pool, "maintenance", "maintenance_title", "Scheduled upgrade")
		setFlag(t, pool, "maintenance", "maintenance_html", body)

		maintenance := read()
		if got, _ := maintenance["html"].(string); got != body {
			t.Fatalf("splash body = %q, want %q", got, body)
		}
		// The title and the flag travel with it: a client reads one object.
		if got, _ := maintenance["title"].(string); got != "Scheduled upgrade" {
			t.Fatalf("splash title = %q, want %q", got, "Scheduled upgrade")
		}
		if enabled, _ := maintenance["enabled"].(bool); !enabled {
			t.Fatal("maintenance reported as disabled while its row says enabled")
		}
		// The markdown message keeps its own default, because the two fields
		// are alternatives and a client picks between them.
		if got, _ := maintenance["message"].(string); got == "" {
			t.Fatal("the markdown message lost its default when an HTML body was stored")
		}
	})

	t.Run("a whitespace-only body reads as empty, so the renderer falls back", func(t *testing.T) {
		setFlag(t, pool, "maintenance", "maintenance_html", "   \n\t ")
		if html, _ := read()["html"].(string); html != "" {
			t.Fatalf("a whitespace-only splash body reads as %q, want empty", html)
		}
	})
}
