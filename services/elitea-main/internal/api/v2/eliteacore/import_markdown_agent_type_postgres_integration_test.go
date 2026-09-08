package eliteacore_test

// Issue 845: the platform's own markdown export was not importable by the
// platform's own API.
//
// `?format=md` writes `agent_type: agent` for a stored `openai` agent, which is
// pylon's spelling and which this port kept on purpose (export_markdown.go,
// markdownAgentType). The import wizard's allow-list never had an `agent`
// member, so posting that frontmatter straight back answered
// `Import function has been failed: invalid agent_type` and wrote nothing. The
// loop closed only inside a browser: both web clients rename `agent` to
// `openai` in their own file parser, and no part of the API contract said a
// caller had to.
//
// The case below is the round trip a NON-BROWSER client makes — export, parse
// the frontmatter, post it back verbatim — and it reads the stored row rather
// than the status code, because an import that answered 201 and stored the
// literal `agent` would route the agent to no executor at all.
//
// The pool, the migration corpus and the seed come from
// export_import_roundtrip_postgres_integration_test.go — see its header.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

// markdownFrontmatter splits an exported `.agent.md` file into its parsed YAML
// frontmatter and its body, the way any client's file parser does.
func markdownFrontmatter(t *testing.T, document string) (map[string]any, string) {
	t.Helper()
	if !strings.HasPrefix(document, "---\n") {
		t.Fatalf("the export does not open with YAML frontmatter:\n%s", document)
	}
	parts := strings.SplitN(document, "---", 3)
	if len(parts) < 3 {
		t.Fatalf("the export has no closing --- delimiter:\n%s", document)
	}
	var front map[string]any
	if err := yaml.Unmarshal([]byte(parts[1]), &front); err != nil {
		t.Fatalf("the exported frontmatter is not YAML: %v\n%s", err, parts[1])
	}
	return front, strings.TrimSpace(parts[2])
}

// importEntryFromFrontmatter builds the wizard entry a client makes out of an
// exported file WITHOUT translating anything: every value goes back exactly as
// the export wrote it. That "without" is the whole point of the case.
func importEntryFromFrontmatter(front map[string]any, body string) []any {
	name, _ := front["name"].(string)
	description, _ := front["description"].(string)
	return []any{map[string]any{
		"entity":            "agents",
		"name":              name,
		"description":       description,
		"original_exported": true,
		"import_uuid":       "md-round-trip",
		"versions": []any{map[string]any{
			"name":                "base",
			"import_version_uuid": "md-round-trip-base",
			"instructions":        body,
			"agent_type":          front["agent_type"],
			"llm_settings": map[string]any{
				"model_name":  front["model"],
				"temperature": front["temperature"],
				"max_tokens":  front["max_tokens"],
			},
			"meta":                  map[string]any{"step_limit": front["step_limit"]},
			"tools":                 []any{},
			"variables":             []any{},
			"conversation_starters": []any{},
			"welcome_message":       front["welcome_message"],
			"tags":                  []any{},
		}},
	}}
}

// TestMarkdownExportImportsWithoutAClientSideRename is the acceptance test for
// issue 845.
func TestMarkdownExportImportsWithoutAClientSideRename(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedRoundTripAgent(t, pool)

	recorder := exportWithQuery(t, handler, seeded.applicationID, "?format=md")
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200, body = %s", recorder.Code, recorder.Body.String())
	}
	front, body := markdownFrontmatter(t, recorder.Body.String())

	// The exporter is UNCHANGED: a stored `openai` agent is still written as
	// `agent`, so every file already in the wild keeps its meaning. If this
	// assertion ever fails the fix moved to the other side of the round trip
	// and this test is measuring nothing.
	if front["agent_type"] != "agent" {
		t.Fatalf("the markdown export writes agent_type = %v, want \"agent\" — issue 845 is about that value",
			front["agent_type"])
	}
	if body != "do the thing" {
		t.Fatalf("the export body = %q, want the seeded instructions", body)
	}

	answer := decodeImportLink(t, importLinkDo(t, importLinkRouter(handler),
		importEntryFromFrontmatter(front, body)))
	if len(answer.Errors.Agents) != 0 {
		t.Fatalf("the import of the platform's own export was refused: %+v (issue 845)", answer.Errors.Agents)
	}
	if len(answer.Result.Agents) != 1 {
		t.Fatalf("the import created %d agents, want 1", len(answer.Result.Agents))
	}
	importedID := importLinkAtoi(t, answer.Result.Agents[0].ID)

	// The stored row, not the echo. An import that accepted `agent` and wrote
	// it verbatim would satisfy every assertion above and leave an agent whose
	// type routes it to no executor.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var storedType, storedInstructions, storedLLM string
	if err := pool.QueryRow(ctx, `
SELECT agent_type, COALESCE(instructions, ''), COALESCE(llm_settings::text, '{}')
FROM p_1.application_versions
WHERE application_id = $1`, importedID).Scan(&storedType, &storedInstructions, &storedLLM); err != nil {
		t.Fatalf("the import wrote no version for application %d: %v", importedID, err)
	}
	if storedType != "openai" {
		t.Errorf("the imported version stores agent_type = %q, want %q", storedType, "openai")
	}
	if storedInstructions != "do the thing" {
		t.Errorf("the imported version stores instructions = %q, want the source's", storedInstructions)
	}

	// The rest of the version is the SOURCE's. The rename must not be a
	// shortcut that drops what it did not translate.
	var llm map[string]any
	if err := json.Unmarshal([]byte(storedLLM), &llm); err != nil {
		t.Fatalf("the imported llm_settings is not JSON: %v", err)
	}
	if llm["model_name"] != "gpt-4o" {
		t.Errorf("the imported version stores model_name = %v, want the source's gpt-4o", llm["model_name"])
	}

	var sourceType, sourceInstructions string
	if err := pool.QueryRow(ctx, `
SELECT agent_type, COALESCE(instructions, '') FROM p_1.application_versions WHERE id = $1`,
		seeded.versionID).Scan(&sourceType, &sourceInstructions); err != nil {
		t.Fatalf("read the source version back: %v", err)
	}
	if storedType != sourceType || storedInstructions != sourceInstructions {
		t.Errorf("the imported version (%q, %q) is not the source version (%q, %q)",
			storedType, storedInstructions, sourceType, sourceInstructions)
	}
}

// The gate is still a gate. Accepting one more spelling must not accept every
// spelling: a type nothing can run is still refused, and refused BEFORE the
// application row is written.
func TestImportStillRefusesAnUnknownAgentType(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)

	entry := []any{map[string]any{
		"entity":      "agents",
		"name":        "unknown type",
		"description": "seeded",
		"import_uuid": "unknown-type",
		"versions": []any{map[string]any{
			"name":       "base",
			"agent_type": "anthropic",
		}},
	}}
	answer := decodeImportLink(t, importLinkDo(t, importLinkRouter(handler), entry))
	if len(answer.Errors.Agents) != 1 {
		t.Fatalf("an unknown agent_type produced %d errors, want 1: %+v", len(answer.Errors.Agents), answer)
	}
	if !strings.Contains(answer.Errors.Agents[0].Msg, "invalid agent_type") {
		t.Errorf("the refusal reads %q, want it to name the invalid agent_type", answer.Errors.Agents[0].Msg)
	}
	if len(answer.Result.Agents) != 0 {
		t.Errorf("a refused entry still produced %d agents", len(answer.Result.Agents))
	}
	if got := importLinkCount(t, pool, `SELECT count(*) FROM p_1.applications`); got != 0 {
		t.Errorf("a refused entry wrote %d application rows", got)
	}
}

// The two spellings that mean the same stored type must produce the same row.
// `""` took the column default through a branch of its own before; it now goes
// through the same mapping as `agent`, and this pins that they still agree.
func TestImportStoresOpenaiForBothSpellings(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)

	for _, spelling := range []any{"agent", "", nil} {
		entry := []any{map[string]any{
			"entity":      "agents",
			"name":        "spelling probe",
			"description": "seeded",
			"import_uuid": "spelling-probe",
			"versions": []any{map[string]any{
				"name":       "base",
				"agent_type": spelling,
			}},
		}}
		answer := decodeImportLink(t, importLinkDo(t, importLinkRouter(handler), entry))
		if len(answer.Errors.Agents) != 0 {
			t.Fatalf("agent_type %#v was refused: %+v", spelling, answer.Errors.Agents)
		}
		if len(answer.Result.Agents) != 1 {
			t.Fatalf("agent_type %#v created %d agents, want 1", spelling, len(answer.Result.Agents))
		}
		importedID := importLinkAtoi(t, answer.Result.Agents[0].ID)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var stored string
		err := pool.QueryRow(ctx, `
SELECT agent_type FROM p_1.application_versions WHERE application_id = $1`, importedID).Scan(&stored)
		cancel()
		if err != nil {
			t.Fatalf("agent_type %#v: the import wrote no version: %v", spelling, err)
		}
		if stored != "openai" {
			t.Errorf("agent_type %#v stored %q, want %q", spelling, stored, "openai")
		}
	}
}
