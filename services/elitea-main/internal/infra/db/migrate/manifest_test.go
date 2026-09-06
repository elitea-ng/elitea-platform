package migrate

import (
	"crypto/sha256"
	"testing"
	"testing/fstest"

	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/stretchr/testify/require"
)

func TestLoadManifestOrdersAndHashesMigrations(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"shared/0030_second.sql": {Data: []byte("SELECT 2")},
		"shared/0001_first.sql":  {Data: []byte("SELECT 1")},
	}
	manifest, err := LoadManifest(files, ScopeShared)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 30}, []int64{manifest[0].Version, manifest[1].Version})
	require.Equal(t, sha256.Sum256([]byte("SELECT 1")), manifest[0].Checksum)
}

func TestLoadManifestRejectsInvalidAndDuplicateFiles(t *testing.T) {
	t.Parallel()

	tests := map[string]fstest.MapFS{
		"invalid name": {
			"shared/latest.sql": {Data: []byte("SELECT 1")},
		},
		"duplicate version": {
			"shared/0001_first.sql":  {Data: []byte("SELECT 1")},
			"shared/0001_second.sql": {Data: []byte("SELECT 2")},
		},
		"empty migration": {
			"shared/0001_first.sql": {Data: nil},
		},
	}

	for name, files := range tests {
		files := files
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadManifest(files, ScopeShared)
			require.Error(t, err)
		})
	}
}

func TestVerifyChecksumRejectsEditedAppliedMigration(t *testing.T) {
	t.Parallel()

	migration := Migration{Path: "shared/0001_first.sql", Checksum: sha256.Sum256([]byte("new"))}
	recorded := sha256.Sum256([]byte("old"))
	require.Error(t, verifyChecksum(migration, recorded[:]))
}

func TestValidateRecordedLedgerRejectsDatabaseAheadAndMetadataDrift(t *testing.T) {
	t.Parallel()
	first := Migration{Scope: ScopeShared, Version: 1, Name: "first", Path: "shared/0001_first.sql", Checksum: sha256.Sum256([]byte("one"))}
	second := Migration{Scope: ScopeShared, Version: 2, Name: "second", Path: "shared/0002_second.sql", Checksum: sha256.Sum256([]byte("two"))}
	futureChecksum := sha256.Sum256([]byte("future"))
	manifest := []Migration{first, second}

	for name, recorded := range map[string][]recordedMigration{
		"database ahead":  {{version: 3, name: "future", checksum: futureChecksum[:]}},
		"renamed history": {{version: 1, name: "other", checksum: first.Checksum[:]}},
		"edited history":  {{version: 1, name: first.Name, checksum: second.Checksum[:]}},
	} {
		recorded := recorded
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := validateRecordedLedger(manifest, recorded, false); err == nil {
				t.Fatal("ledger drift was accepted")
			}
		})
	}
	if _, err := validateRecordedLedger(manifest, []recordedMigration{{version: 1, name: first.Name, checksum: first.Checksum[:]}}, true); err == nil {
		t.Fatal("incomplete head was accepted")
	}
}

func TestAdvisoryLockKeyIsScopedAndStable(t *testing.T) {
	t.Parallel()

	require.Equal(t, advisoryLockKey(ScopeShared, "platform"), advisoryLockKey(ScopeShared, "platform"))
	require.NotEqual(t, advisoryLockKey(ScopeShared, "platform"), advisoryLockKey(ScopeTenant, "platform"))
	require.NotEqual(t, advisoryLockKey(ScopeTenant, "1"), advisoryLockKey(ScopeTenant, "2"))
	require.NotEqual(t, advisoryLockKey(ScopeShared, "platform"), advisoryLockKey(ScopeAgentState, "agentstate"))
}

func TestEmbeddedHistoriesHaveExpectedHeads(t *testing.T) {
	t.Parallel()

	shared, err := LoadManifest(platformmigrations.Files, ScopeShared)
	require.NoError(t, err)
	// 84: shared/0084_budget_usage_dimensions.sql, the per-request usage ledger
	// and the nullable soft-alert thresholds that issues #320, #321 and #322
	// need. It follows shared/0083_viewer_secret_list_and_own_avatar.sql, the
	// two role splits #402 corrects, which itself follows the nine per-surface
	// permission grants #386 adds in 0074 to 0082.
	// 85: shared/0085_project_member_and_role_listings_administration.sql, the
	// two administration-mode listing grants #313 needs. It follows
	// shared/0084_budget_usage_dimensions.sql above.
	// 86: shared/0086_gateway_audio_prices.sql, the four per-1,000,000-unit
	// audio price columns the /llm/v1/audio/* routes need. It arrived on main
	// while the five below were in review, so they moved up one number each.
	// 87: shared/0087_artifact_object_expiry_from_object_age.sql, the backfill
	// that re-derives every artifact object's expires_at from its own
	// created_at. Rows written before the fix carry the bucket's frozen
	// deadline, and the ones already past it are swept within 15 minutes.
	// 88: shared/0088_administration_secret_permissions.sql, the four
	// administration-mode grants the global secret vault needs. Without them
	// every global-vault route answered 403, a super_admin included.
	// 89: shared/0089_central_system_role.sql, the central default-mode
	// `system` role the per-project machine identity resolves through. With no
	// such role the scheduled-execution PAT resolved the empty set and every
	// worker callback answered 403.
	// 90: shared/0090_project_override_reconciliation.sql, the one-time
	// delivery of the corpus's default-mode grants to the projects whose own
	// permission rows suppress the central fallback.
	// 91: shared/0091_pylon_viewer_secret_list_parity.sql, which withdraws
	// 0083's `configuration.secrets.secret.list` grant from the default-mode
	// `viewer` on a pylon-managed auth_core. 0083 states that no existing
	// deployment gains anything; a pylon-backed database carries no per-project
	// permission row, so the central fallback is live there and every project
	// viewer did gain the secret listing.
	// 93: shared/0093_governance_config_type_check.sql, which constrains
	// gateway.governance_config.type to the value set every reader switches on,
	// and carries the correction 0067's checksum-immutable header could not: the
	// gateway now DOES read this table (#218). The constraint is NOT VALID so an
	// existing row with an unknown type does not fail the release.
	// 94: shared/0094_mcp_prebuilt_catalogue.sql, the platform-wide catalogue of
	// pre-built MCP servers. It replaces the indexer_worker plugin descriptor
	// block that pylon distributes over the Arbiter event bus, which this service
	// has no way to read.
	//
	// It was written as 0092 and renumbered here. 0093's header records that it
	// left 0092 free for this file, and 92 IS still free — but a free number is
	// not the rule. scripts/database/check-migration-version.sh requires an added
	// migration to be ABOVE the base head, gap or no gap, so that two deployments
	// applying the corpus at different times apply it in the same order. The
	// ledger would have tolerated the back-fill; the policy does not, and the
	// policy is the stricter of the two on purpose.
	//
	// 92 stays empty. Nothing needs to fill it: LoadManifest sorts by version and
	// Head() reads the last entry, so a gap costs nothing.
	// 95: shared/0095_identity_providers.sql, the typed identity provider
	// revision. It replaces the four environment variables that were this
	// service's only federation configuration, and gives the admin
	// Configuration page's Authentication section a store and a reader.
	// 96: shared/0096_scim_provisioning.sql, the side table SCIM 2.0 user
	// provisioning needs. It holds the identity provider's externalId and the
	// resource timestamps, keyed by user id — the account row itself is
	// pylon-owned and is read and updated, never reshaped.
	// 97: shared/0097_gateway_model_price_override.sql, which lets an operator
	// author a model price that the scheduler's price-sync UPSERT will not
	// overwrite. The column is the handshake between two writers of the same
	// row: without it the syncer's ON CONFLICT DO UPDATE reassigns every price
	// column from EXCLUDED, so an authored price is correct until the next tick
	// and then silently reverts.
	//
	// It was written as 0095 and renumbered on merge: 0095 and 0096 landed on
	// main while this was in review. Both authors were right when they wrote the
	// number and only one could stay — the same collision 0094's header records,
	// and the reason check-migration-version.sh reads the base branch at check
	// time rather than the pull request's own history.
	// 98: shared/0098_scim_group_bindings.sql, the authored binding of one SCIM
	// group to one project role, and the ledger of what a push granted. It
	// reverses 0096's refusal of /Groups: the project and the role are authored
	// by an administrator before any push, so the identity provider supplies the
	// membership and never invents the half a SCIM group cannot carry.
	//
	// It was written as 0097 and renumbered on merge, for the reason the entry
	// above records: 0097 landed on main while this was in review, and a number
	// is claimed at merge rather than at authoring.
	// 99: shared/0099_gateway_request_logs.sql, the gateway's per-request log.
	// It is a THIRD per-request table beside the money accumulator and the
	// billing ledger, and the reason is that a billing delta rides only a
	// BILLED request — so a call refused by a budget, rejected by a policy,
	// addressed to an unresolvable model or failed upstream produces no ledger
	// row at all. A log built over the ledger would list successes and no
	// failures, which is the opposite of what a log is for.
	//
	// It stores NO request or response content, including no upstream error
	// text: the failure column is a classification the gateway assigns, so
	// there is no column a prompt fragment can reach.
	// 100: shared/0100_gateway_request_log_execution_id.sql, which gives the
	// request log an AGENT dimension: the runtime execution id the request was
	// made from, signed into the identity tuple at the edge under signature
	// version v2 so it cannot be attached by a caller.
	//
	// It is an EXECUTION id and not an agent id on purpose. Resolving it to an
	// agent happens at READ time, because elitea_runtime.execution_jobs carries
	// resource_project_id AND projection_project_id and they can differ —
	// writing an agent id onto the log would have had to pick one of those two
	// project meanings and bake it in, importing the exact ambiguity 0099 was
	// built to keep out.
	//
	// THERE IS NO BACKFILL AND THERE CANNOT BE ONE: nothing on a row written
	// before this file identifies an agent. The read side reports availability
	// and omits the breakdown rather than answering "0 agent runs" for a window
	// it cannot speak for.
	// 101: shared/0101_gateway_usage_event_execution_id.sql, the same column on
	// the billing ledger, so per-agent SPEND is answerable and not only
	// per-agent volume — the log has no cost column, deliberately, so that
	// there is one money path and not two.
	//
	// A separate file from 0100 because the two tables have separate WRITERS
	// (the gateway in process; the scheduler's write-back consumer) and
	// therefore separate deployment risk — and because a migration's checksum
	// is immutable once applied, so a combined file could never be split later.
	//
	// 102: shared/0102_skill_icon_permissions.sql, the four default-mode grants
	// the skill icon route family is gated on. A separate file rather than four
	// more rows in 0068 because a migration is checksum-immutable once it has
	// run, and it is not optional: 0063's header records that gating a route on
	// a permission nothing grants is 403-for-everyone, which reads as a broken
	// page rather than as a missing grant. It grants centrally AND delivers the
	// same four strings to the projects that carry their own permission rows,
	// because the central set is discarded wholesale for those callers — see
	// shared/0090's header and migrations/project_override_reconciliation_test.go.
	//
	// RENUMBERED from 0100 at merge. It and the two gateway files above were
	// authored in parallel and each correctly claimed the next free number at
	// the time; only the merge can see the collision. The number belongs to
	// whichever lands first.
	//
	// 103: shared/0103_shared_chat_links.sql, the store behind "share a
	// conversation by link". It is SHARED rather than tenant even though every
	// other chat object is tenant-scoped, because the anonymous view is handed
	// a token and nothing else: resolving it against per-project schemas would
	// mean either scanning every `p_%` schema on each anonymous request, or
	// encoding the project into the token so a caller could steer which schema
	// is queried. One central table keyed on the token settles both, and the
	// project id is a column the reader takes from the resolved row.
	//
	// It stores SHA-256 of the token, never the token, so a database dump is
	// not a set of live links; and expires_at is NOT NULL, so a link with no
	// end of life is unrepresentable rather than merely un-offered.
	//
	// RENUMBERED from 0100 at merge, for the same reason 0102 was: three
	// streams authored a 0100 in parallel and each was correct at the time.
	//
	// 104: shared/0104_evaluation_dimension_permissions.sql, the four
	// default-mode grants the evaluation dimension library needs. It is the
	// RBAC half of the first Agent Evaluation slice; the table itself is
	// tenant/0130, because a dimension is one project's authored content and
	// these four strings are central role grants.
	//
	// It is the first file in this corpus to seed a permission the pylon
	// catalogue does not declare, and it says why in its own header: Agent
	// Evaluation is not in the plugin corpus this repository carries, so the
	// names come from the product's own UI constants rather than from a
	// `check_api` transcription, and the routes therefore gate through
	// exported constants instead of router.go's `projectPermission` helper.
	// The grant gate (router_permission_grant_gate_test.go) still binds; only
	// the pylon-provenance assertion, which would be false, does not.
	//
	// RENUMBERED from 0100 at merge — the FOURTH stream to claim that
	// number, each correct when it was written. Only the merge sees it.
	//
	// 105: shared/0105_predict_llm_permission.sql, the single default-mode
	// grant behind POST /elitea_core/predict_llm/prompt_lib/{projectID} (#194).
	// Nothing in this corpus granted `models.applications.predict.post` before
	// it, so the route it gates would have shipped registered and
	// 403-for-everyone.
	//
	// It is the first grant file here whose default-mode split reaches VIEWER,
	// and that is not a widening invented for the port: legacy's own
	// predict_llm.py declares recommended_roles viewer=True in DEFAULT_MODE and
	// viewer=False in ADMINISTRATION_MODE, and testdata/postgres/legacy-rbac-matrix.json
	// carries the same asymmetry. Only the default mode is delivered, because
	// only the default-mode route exists.
	//
	// Like 0102 and 0104 it grants centrally AND delivers the same string to
	// the projects that carry their own permission rows, because the central
	// set is discarded wholesale for those callers — see shared/0090's header
	// and migrations/project_override_reconciliation_test.go.
	//
	// 106: shared/0106_deepwiki_permissions.sql, the two default-mode grants
	// behind the DeepWiki facade (ADR-0022 phase P2). It is the SECOND file
	// here — after 0104 — whose strings are chosen rather than recovered, and
	// for a sharper reason than 0104's: the legacy `deepwiki_plugin` declares
	// no permissions at all. It has no `api/` package and no `check_api` call;
	// its five routes were reached by the pylon provider hub over mTLS and
	// authorised by the HOP. There is nothing in the pylon catalogue to
	// transcribe, so the pylon-provenance assertion would be false here for the
	// same reason it is false for 0104. The grant gate still binds.
	//
	// The split is read/generate, not read/write: a viewer may see capacity and
	// follow a running generation, and may not start one or cancel someone
	// else's. Like 0102, 0104 and 0105 it grants centrally AND delivers to the
	// projects carrying their own permission rows.
	//
	// 107: shared/0107_provider_admitted_revisions.sql, the provider ADMISSION
	// plane (ADR-0012 phase P3). It is the storage the three service-descriptor
	// routes had been refusing for want of, and it is the first file here that
	// creates a whole schema rather than granting into an existing one.
	//
	// Two of its constraints are the architecture rather than hygiene:
	// UNIQUE (project_id, provider_id) WHERE status = 'active' makes "which
	// manifest is this deployment running" answerable, and
	// CHECK (status <> 'active' OR overlay_revision IS NOT NULL) is ADR-0012's
	// "a missing overlay policy fails admission" written where Go cannot
	// bypass it — this deployment can record and show a provider and
	// physically cannot activate one.
	//
	// It claims no pylon-owned table. Every name is new, in a new schema, so
	// it cannot collide with a seeded fixture at 42P07 the way three earlier
	// migrations did.
	//
	// 108: shared/0108_inventory_permissions.sql, the two default-mode grants
	// behind the Inventory facade — the SECOND provider, and ADR-0012's
	// falsification test for the runner generalisation. It is the third file
	// here whose strings are chosen rather than recovered, for 0106's reason:
	// the legacy inventory_plugin has no api/ package and no check_api call,
	// so the pylon catalogue has nothing to transcribe.
	//
	// Its grant blocks are 0106's with two strings substituted, copied rather
	// than factored out: each migration names the permissions it grants
	// inline, and a shared procedure would put them somewhere a reader of that
	// migration cannot see.
	// 109: shared/0109_provider_policy_overlay.sql, which lifts the refusal 0107
	// wrote into the schema. 0107's CHECK — `status <> 'active' OR
	// overlay_revision IS NOT NULL` — made activation impossible because nothing
	// could issue an overlay, and `overlay_revision` was a free TEXT column with
	// nothing behind it: any string satisfied the CHECK, so a constraint reading
	// as "activation requires a reviewed policy" really said "requires a
	// non-empty string". This file adds provider_policy_overlay (insert-only,
	// keyed on 'lpo_' || left(sha256(canonical body), 32), bound to the
	// published manifest it was reviewed against) and the FOREIGN KEY that makes
	// the reference true.
	//
	// It records `created_by` and `approved_by` and deliberately does NOT
	// enforce that they differ. The specification's creator-≠-approver rule is
	// right for a reviewed deployment and would make activation impossible on
	// every single-operator one — the standalone stack, an evaluation install, a
	// laptop — turning "cannot activate, no overlay issuer" into "cannot
	// activate, no second operator". The columns are recorded so that
	// enforcement is later a policy decision rather than a migration over live
	// rows. The file's header carries the full argument.
	//
	// It also grants `provider_hub.descriptor.activate` to the
	// administration-mode super_admin and admin — a NEW string, not a reuse of
	// `.register`, because every facade registrar files a registration at boot
	// while activation is the switch that lets agents call the provider.
	//
	// 110: shared/0110_branding_permission.sql, the one administration-mode
	// grant behind the admin Branding surface (ADR-0024 decision 5):
	// `configuration.branding` to super_admin and admin, the holder set of
	// every other central configuration grant. Chosen rather than recovered:
	// white-labeling is net-new, so the legacy catalogue has no string for
	// it. A new file for 0082's reason — 0060 returns early on any configured
	// deployment, and migrations are checksum-immutable.
	//
	// 111: shared/0111_mcp_prebuilt_parameter_schema.sql adds the bounded
	// operator-owned config_schema used to publish parameterized prebuilt MCP
	// toolkit forms. Existing rows receive an empty properties object.
	//
	// 112: shared/0112_toolkit_execute_read.sql admits the read-only toolkit
	// capability and binds its request and projected result to one execution
	// generation. The tables retain the exact input entry and output event.
	require.EqualValues(t, 112, Head(shared))

	tenant, err := LoadManifest(platformmigrations.Files, ScopeTenant)
	require.NoError(t, err)
	// 125: tenant/0125_entity_tool_mapping_entity_id.sql.
	//
	// 126: tenant/0126_chat_folders_and_selected_conversations.sql, which gives
	// the ledgered corpus the conversation-folder objects that until now existed
	// only in the dev bootstrap — chat_conversation_folders,
	// chat_conversations.folder_id and attachment_participant_id, and
	// chat_selected_conversations. 0123 declared a chat_conversations that was a
	// strict SUBSET of the deployed one because it mirrored the sqlc COMPILER
	// projection, which is a projection of the queries and not of the schema.
	// The corpus could therefore not rebuild the shape every deployment runs,
	// and the repository test template — built from the corpus alone — could not
	// execute a single line of folder SQL.
	//
	// 127: tenant/0127_chat_message_attachment_items.sql, which takes ownership
	// of chat_messages_attachment (#606). Its absence was why an uploaded chat
	// attachment was conversation-scoped only, with no association to the
	// message it was sent with: it never rendered inline in the transcript, and
	// pylon's per-message attachment cleanup had nothing to iterate.
	//
	// 128: tenant/0128_owner_id_column_meanings.sql, which writes what
	// `owner_id` and `author_id` mean onto the columns themselves (#533). The
	// one name holds a PROJECT in `elitea_tools` and a USER in
	// `chat_conversation_folders`, and no foreign key catches either, so a join
	// written on the wrong assumption returns rows that look valid. The file
	// adds no constraint: a project column would reference `centry.project`,
	// which shared/0071, 0073 and 0098 each refuse for the same reason, and a
	// user column would reference a pylon table that a corpus-only database
	// does not have. It records the disagreement over `applications.owner_id`
	// rather than hiding it.
	//
	// 129: tenant/0129_chat_canvas_tables.sql, which takes ownership of
	// chat_messages_canvas, chat_canvas_versions and
	// chat_canvas_version_authors. ConversationsRepo referenced the first two
	// and nothing created either, so POST
	// /elitea_core/canvases/prompt_lib/{projectID} — a registered route behind
	// a permission shared/0068 seeds — answered 42P01 on every deployment pylon
	// never touched, and GetMessageByUUID's unconditional LEFT JOIN would have
	// done the same for every message read once its GET was bound, which the
	// same session did.
	//
	// 130: tenant/0130_eval_dimensions.sql, the evaluation dimension library —
	// the FIRST and, for now, the ONLY Agent Evaluation table. The baseline UI
	// spans 19 `eval_*` path families; the library is the one with no
	// orchestrator, judge model or code sandbox behind it, so it is the one
	// that can ship correct while the run engine is unbuilt. `eval_suites`,
	// `eval_bindings`, `eval_datasets`, `eval_dataset_cases`, `eval_runs`,
	// `eval_results` and `eval_human_scores` are deliberately absent and must
	// arrive with the code that reads them.
	require.EqualValues(t, 130, Head(tenant))

	// The agentstate scope is this branch's, and it is counted separately: the
	// native runtime's ADK sessions and graph checkpoints live in their own
	// database, so its ledger advances independently of the tenant one.
	agentState, err := LoadManifest(platformmigrations.Files, ScopeAgentState)
	require.NoError(t, err)
	require.EqualValues(t, 2, Head(agentState))
}
