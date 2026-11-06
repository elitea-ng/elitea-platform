package eliteacore_test

// ADR-0050 decision 1a (issue 1604): a version's soul survives the platform's
// own markdown export and a non-browser client's import of that file. The
// export writes the soul as one nested front-matter mapping; the import reads
// it back as the version entry's `soul` and stores it unchanged.

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

func TestTheSoulSurvivesTheMarkdownExportAndImport(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedRoundTripAgent(t, pool)
	soul := map[string]any{
		"schema":        "elitea.soul/v1",
		"display_name":  "Mira",
		"voice":         []any{"concise"},
		"relationships": []any{map[string]any{"agent": "ci-doctor", "stance": "delegate: failing builds"}},
		"body":          "Short sentences.\n\nNo exclamation marks.",
	}
	encoded, err := json.Marshal(soul)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE p_1.application_versions SET soul = $2::jsonb WHERE id = $1`,
		seeded.versionID, string(encoded)); err != nil {
		t.Fatal(err)
	}

	recorder := exportWithQuery(t, handler, seeded.applicationID, "?format=md")
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	front, body := markdownFrontmatter(t, recorder.Body.String())
	if !reflect.DeepEqual(front["soul"], soul) {
		t.Fatalf("the export's soul = %#v, want %#v", front["soul"], soul)
	}

	entries := importEntryFromFrontmatter(front, body)
	version := entries[0].(map[string]any)["versions"].([]any)[0].(map[string]any)
	version["soul"] = front["soul"]
	answer := decodeImportLink(t, importLinkDo(t, importLinkRouter(handler), entries))
	if len(answer.Errors.Agents) != 0 || len(answer.Result.Agents) != 1 {
		t.Fatalf("the import of the soul was refused: %+v", answer.Errors.Agents)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT soul::text FROM p_1.application_versions WHERE application_id = $1`,
		importLinkAtoi(t, answer.Result.Agents[0].ID)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(stored), &back); err != nil || !reflect.DeepEqual(back, soul) {
		t.Fatalf("the imported soul = %s, want %s (%v)", stored, encoded, err)
	}

	// A file whose soul names a capability is refused, and the row says why.
	version["soul"] = map[string]any{"schema": "elitea.soul/v1", "tools": []any{"shell"}}
	refused := decodeImportLink(t, importLinkDo(t, importLinkRouter(handler), entries))
	if len(refused.Errors.Agents) != 1 {
		t.Fatalf("a soul naming a capability was imported: %+v", refused.Result.Agents)
	}
	if message := refused.Errors.Agents[0].Msg; !strings.Contains(message, "soul_capability_key") {
		t.Fatalf("the refusal does not name the rule: %q", message)
	}
}
