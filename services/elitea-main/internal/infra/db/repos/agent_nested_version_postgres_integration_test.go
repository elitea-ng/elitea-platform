package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
)

// TestPostgresNestedApplicationVersionProjectsTheSameDocumentAsTheTurn is the
// only test that can hold the two projections together.
//
// ResolveCurrentApplicationVersionDetails is a COPY of
// ResolveCurrentApplicationTurn's version block (agent_chat.sql), and a copy
// drifts. The native runtime decodes the parent's document and every nested
// child's with one decoder, so a divergence would not fail here or in any unit
// test — it would fail at assembly time, in a worker span, on whichever agent
// happened to use the field that moved. Comparing the two strings byte for byte
// on one seeded version is what keeps the copy honest.
func TestPostgresNestedApplicationVersionProjectsTheSameDocumentAsTheTurn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	// The seed attaches a skill to version 41 but no toolkit. Both subqueries
	// have to be non-empty for this comparison to mean anything: an empty
	// `tools` array would match between the two projections no matter how far
	// their tool clauses had drifted.
	if _, err := tx.Exec(t.Context(), `
INSERT INTO applications (id, name, description, owner_id) VALUES
    (32, 'Full Name Resolver', 'Resolve a full name', 11);
INSERT INTO application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings
) VALUES (
    42, 32, 'without_nesting', 'draft', 11,
    '80000000-0000-4000-8000-000000000032', '{}'::jsonb, 'Resolve names',
    '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb
);
INSERT INTO elitea_tools (
    id, type, name, description, settings, author_id, meta
) VALUES (
    52, 'application', 'without_nesting', NULL,
    '{"application_id":32,"application_version_id":42}'::jsonb, 11, '{}'::jsonb
);
INSERT INTO entity_tool_mapping (
    id, tool_id, entity_id, entity_version_id, entity_type, selected_tools
) VALUES
    (91, 51, 31, 41, 'agent', '["list_products"]'::jsonb),
    (92, 52, 31, 41, 'agent', NULL);`); err != nil {
		t.Fatal(err)
	}

	queries := sqlcgen.New(tx)
	turn, err := queries.ResolveCurrentApplicationTurn(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21,
			QuestionID:       mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000031"),
			ConversationUuid: mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
			ProjectID:        1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	nested, err := queries.ResolveCurrentApplicationVersionDetails(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
			ApplicationVersionID: 41,
			ApplicationID:        31,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if nested.ApplicationVersionID != 41 || nested.ApplicationID != 31 {
		t.Fatalf(
			"nested projection returned identity %d/%d, want 41/31",
			nested.ApplicationVersionID,
			nested.ApplicationID,
		)
	}
	// Guard against a vacuous comparison: two empty documents would also be
	// equal. The seeded toolkit, its narrowed selection and the seeded skill all
	// have to be present for the equality below to be evidence of anything.
	for _, marker := range []string{
		`"toolkit_name": "product"`,
		`"toolkit_name": "Full Name Resolver"`,
		`"name": "Full Name Resolver"`,
		`"selected_tools": ["list_products"]`,
		`"available_tools": ["list_products"]`,
		`"name": "release-proof"`,
	} {
		if !strings.Contains(nested.ApplicationVersionDetailsJson, marker) {
			t.Fatalf(
				"nested projection omits %s: %s",
				marker,
				nested.ApplicationVersionDetailsJson,
			)
		}
	}
	if strings.Contains(nested.ApplicationVersionDetailsJson, `"toolkit_name": "without_nesting"`) {
		t.Fatalf(
			"nested projection exposed the stored version label as the application alias: %s",
			nested.ApplicationVersionDetailsJson,
		)
	}
	if nested.ApplicationVersionDetailsJson != turn.ApplicationVersionDetailsJson {
		t.Fatalf(
			"nested version projection drifted from the turn projection\nnested: %s\nturn:   %s",
			nested.ApplicationVersionDetailsJson,
			turn.ApplicationVersionDetailsJson,
		)
	}

	// The application id is a filter, not a label: a version that belongs to a
	// different application must not resolve for the pair the worker asked for.
	if _, err := queries.ResolveCurrentApplicationVersionDetails(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
			ApplicationVersionID: 41,
			ApplicationID:        32,
		},
	); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched application identity resolved a version: %v", err)
	}
	if _, err := queries.ResolveCurrentApplicationVersionDetails(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
			ApplicationVersionID: 999,
			ApplicationID:        31,
		},
	); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("absent version resolved a document: %v", err)
	}
}
