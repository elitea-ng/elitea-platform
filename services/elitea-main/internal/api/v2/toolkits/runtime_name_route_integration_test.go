package toolkits

// The toolkit DETAILS route, against a real database, for the one field the
// runtime reads back out of it.
//
// `toolkit_name` is not decoration: it is the identifier an agent's
// instructions and the SDK's tool registry address this toolkit's tools by.
// The route computed it with a rule of its own that kept alphanumerics ONLY,
// while index admission, the tool-run path, the built-in name deriver, the
// Python worker's SDK adapter and all three web helpers keep `_`, `.` and `-`
// and fold `.` into `_`. So the details page told a user to write
// `autotestgithubtoolkitv1` for a toolkit the runtime knows as
// `autotestgithubtoolkit_v1`, and the instruction that followed named a tool
// that does not exist.
//
// A unit test on the shared rule cannot state this: the defect was that the
// ROUTE did not use the rule. So this goes through the repository read the
// route serves, against a real row whose stored name carries all three
// characters plus a space.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL), which ci-go.yml
// provides.

import (
	"context"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

func TestToolkitDetailsReportsTheRuntimeIdentifierAgainstPostgres(t *testing.T) {
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	repo := &pgRepo{pool: pool}

	for _, testCase := range []struct {
		name        string
		storedName  string
		toolkitType string
		want        string
	}{
		{
			// The reported case: a readable name with a space, a hyphen and a
			// dot in it. Under the old rule this answered
			// "autotestgithubtoolkitv1".
			name:        "a readable name keeps its underscore, hyphen and folded dot",
			storedName:  "autotest github-toolkit.v1",
			toolkitType: "custom",
			want:        "autotestgithub-toolkit_v1",
		},
		{
			name:        "an underscore is not punctuation to the runtime",
			storedName:  "autotest_artifact_toolkit",
			toolkitType: "custom",
			want:        "autotest_artifact_toolkit",
		},
		{
			// A row with no name at all is addressed by its type, which the
			// old rule rendered as the empty string.
			name:        "a nameless row is addressed by its type",
			storedName:  "",
			toolkitType: "custom",
			want:        "custom",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			created, err := repo.CreateToolkit(ctx, "1", map[string]any{
				"name":       testCase.storedName,
				"type":       testCase.toolkitType,
				"settings":   map[string]any{"selected_tools": []any{}},
				"meta":       map[string]any{},
				"_author_id": "7",
			})
			if err != nil {
				t.Fatalf("CreateToolkit: %v", err)
			}
			toolkitID, _ := created["id"].(string)
			if toolkitID == "" {
				t.Fatalf("CreateToolkit returned no id: %#v", created)
			}
			t.Cleanup(func() {
				if err := repo.DeleteToolkit(context.Background(), "1", toolkitID); err != nil {
					t.Errorf("DeleteToolkit: %v", err)
				}
			})

			row, err := repo.GetToolkit(ctx, "1", toolkitID)
			if err != nil {
				t.Fatalf("GetToolkit: %v", err)
			}
			// The stored name is returned UNTOUCHED alongside it — the two
			// fields answer different questions and a fix that sanitized the
			// display name as well would be a different defect.
			if name, _ := row["name"].(string); name != testCase.storedName {
				t.Errorf("name=%q, want the stored name %q", name, testCase.storedName)
			}
			runtimeName, ok := row["toolkit_name"].(string)
			if !ok {
				t.Fatalf("the details row carries no toolkit_name: %#v", row)
			}
			if runtimeName != testCase.want {
				t.Errorf("toolkit_name=%q, want %q — the details route must report the "+
					"identifier the runtime addresses the toolkit by", runtimeName, testCase.want)
			}
		})
	}
}
