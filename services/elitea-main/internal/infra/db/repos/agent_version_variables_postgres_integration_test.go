package repos

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
)

// TestPostgresApplicationVersionDetailsCarryTheAuthoredVariables pins the one
// thing that makes agent variables reach the SDK worker at all.
//
// `application_version_details_json` used to build `'variables', '[]'::jsonb`
// unconditionally, and that key is the SDK worker's PRIMARY variable source:
// `assistant.py:557-576` (elitea-sdk 0.9.8 @ b5113a12, the revision
// `services/elitea-worker-python/elitea-sdk.lock.json` pins) builds
// `prompt_variables` out of `data['variables']`, a LIST of `{name, value}`
// rows, and renders `instructions` through Jinja2 with that map. Its only
// other source is guarded by `isinstance(meta['variables'], dict)` while this
// platform stores an ARRAY, so with the key hardcoded empty an authored
// `{{topic}}` reached the model unsubstituted no matter what the user saved.
//
// WHICH STORE the key reads is the second thing pinned here, and it is not
// `meta`. A version's variables live in `p_<id>.application_variables`, the
// only store the legacy runtime has and the one every other reader in this
// repository already reads — the version-detail GET the editor reloads through,
// the runtime-facing `GetVersionExpanded`, and the export. `meta ->
// 'variables'` is a mirror the interactive write folds in for its own 201 echo,
// and a FORK does not carry it: the fork copies `meta` verbatim out of an
// export, and an export of a legacy or imported agent has no such key. Reading
// the mirror alone therefore served every forked agent an empty list while its
// rows sat in the table. The projection reads the rows and falls back to the
// mirror, so this test seeds each store separately and pins which one wins.
//
// Both queries are asserted, not just the turn: the two copies of the block
// are byte-identical by construction
// (TestSharedApplicationVersionDetailsProjectionsAreIdentical), but that test
// compares SQL TEXT — only a real database says whether the expression the two
// copies share actually resolves to the stored rows.
func TestPostgresApplicationVersionDetailsCarryTheAuthoredVariables(t *testing.T) {
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

	// The shape the update path writes: the authored rows alongside the
	// `step_limit` the runtime also reads out of the same object.
	if _, err := tx.Exec(t.Context(), `
UPDATE application_versions
   SET meta = '{"step_limit": 25, "variables": [{"name": "topic", "value": "release notes"}, {"name": "blank", "value": ""}]}'::jsonb
 WHERE id = 41;`); err != nil {
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

	// The blank row is projected as authored rather than filtered here: the
	// SDK's own capture skips an empty value ("empty values are runtime
	// placeholders", assistant.py:564-569) and so does the native runtime's
	// `variables::capture_variables`. Dropping it in SQL would hide from both
	// decoders a row the edit page shows, and would make this projection
	// disagree with `versionDetailsResponse`, which returns `meta.variables`
	// verbatim.
	want := []map[string]any{
		{"name": "topic", "value": "release notes"},
		{"name": "blank", "value": ""},
	}
	for name, document := range map[string]string{
		"ResolveCurrentApplicationTurn":           turn.ApplicationVersionDetailsJson,
		"ResolveCurrentApplicationVersionDetails": nested.ApplicationVersionDetailsJson,
	} {
		got := decodeCurrentVersionVariables(t, name, document)
		if len(got) != len(want) {
			t.Fatalf("%s projected %d variables, want %d: %s", name, len(got), len(want), document)
		}
		for index, row := range want {
			for key, value := range row {
				if got[index][key] != value {
					t.Errorf(
						"%s variables[%d][%q] = %v, want %v",
						name, index, key, got[index][key], value,
					)
				}
			}
		}
	}

	// A meta that declares nothing still projects the empty list the decoders
	// expect — the key is never absent and never null.
	for _, meta := range []string{
		`{"step_limit": 25}`,
		`{}`,
		`null`,
		// An OBJECT spelling is deliberately NOT unwrapped into this key: it is
		// not the `{name, value}` list shape either decoder reads here, and both
		// reach that spelling through the `meta` key this projection already
		// carries (the SDK's `isinstance(..., dict)` branch, and the native
		// runtime's object arm of `capture_variables`).
		`{"variables": {"topic": "release notes"}}`,
	} {
		if _, err := tx.Exec(
			t.Context(),
			`UPDATE application_versions SET meta = $1::jsonb WHERE id = 41;`,
			meta,
		); err != nil {
			t.Fatal(err)
		}
		empty, err := queries.ResolveCurrentApplicationVersionDetails(
			t.Context(),
			sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
				ApplicationVersionID: 41,
				ApplicationID:        31,
			},
		)
		if err != nil {
			t.Fatalf("meta %s: %v", meta, err)
		}
		if got := decodeCurrentVersionVariables(t, "meta "+meta, empty.ApplicationVersionDetailsJson); len(got) != 0 {
			t.Errorf("meta %s projected %v, want an empty list", meta, got)
		}
	}

	// THE ROWS. With `meta` left carrying an object spelling this projection
	// deliberately ignores — the state the loop above left behind — the stored
	// rows alone must fill the key. This is the shape a FORK produces and the
	// shape every legacy agent has, and it used to project nothing at all.
	if _, err := tx.Exec(t.Context(), `
INSERT INTO application_variables (application_version_id, name, value, created_at) VALUES
    (41, 'topic', 'release notes', now() AT TIME ZONE 'UTC'),
    (41, 'blank', NULL, now() AT TIME ZONE 'UTC');`); err != nil {
		t.Fatal(err)
	}
	fromRows, err := queries.ResolveCurrentApplicationVersionDetails(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
			ApplicationVersionID: 41,
			ApplicationID:        31,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// `id` is NOT projected, and `value` is '' and not null: one key set for
	// both COALESCE branches, and the string type both decoders declare. The
	// blank row is projected rather than filtered for the reason the first case
	// states — that judgement belongs to the two decoders, not to SQL.
	assertCurrentVersionVariables(t, "rows", fromRows.ApplicationVersionDetailsJson, []map[string]any{
		{"name": "topic", "value": "release notes"},
		{"name": "blank", "value": ""},
	})

	// PRECEDENCE. With rows present, a `meta` mirror that disagrees does not
	// win. The rows are the authored store; the mirror is a fallback for the
	// agents that have no rows, and a fallback that could override the store
	// would let a stale copy of a list decide what the model is told.
	if _, err := tx.Exec(t.Context(), `
UPDATE application_versions
   SET meta = '{"variables": [{"name": "topic", "value": "a stale mirror"}]}'::jsonb
 WHERE id = 41;`); err != nil {
		t.Fatal(err)
	}
	contested, err := queries.ResolveCurrentApplicationVersionDetails(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
			ApplicationVersionID: 41,
			ApplicationID:        31,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentVersionVariables(t, "rows over a disagreeing meta", contested.ApplicationVersionDetailsJson, []map[string]any{
		{"name": "topic", "value": "release notes"},
		{"name": "blank", "value": ""},
	})
}

func assertCurrentVersionVariables(t *testing.T, name, document string, want []map[string]any) {
	t.Helper()
	got := decodeCurrentVersionVariables(t, name, document)
	if len(got) != len(want) {
		t.Fatalf("%s projected %d variables, want %d: %s", name, len(got), len(want), document)
	}
	for index, row := range want {
		if len(got[index]) != len(row) {
			t.Errorf("%s variables[%d] = %v, want exactly the keys %v", name, index, got[index], row)
		}
		for key, value := range row {
			if got[index][key] != value {
				t.Errorf("%s variables[%d][%q] = %v, want %v", name, index, key, got[index][key], value)
			}
		}
	}
}

func decodeCurrentVersionVariables(t *testing.T, name string, document string) []map[string]any {
	t.Helper()
	var decoded struct {
		Variables *[]map[string]any `json:"variables"`
	}
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatalf("%s: decode %s: %v", name, document, err)
	}
	if decoded.Variables == nil {
		t.Fatalf("%s: the projection carries no `variables` array: %s", name, document)
	}
	return *decoded.Variables
}
