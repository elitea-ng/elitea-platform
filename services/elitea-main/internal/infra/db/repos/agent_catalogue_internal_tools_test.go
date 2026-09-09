package repos

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// The internal-tool catalogue is written in TWO places and has to say one thing.
//
// Both turn statements apply it in SQL, against
// `application_version.meta -> 'internal_tools'`
// (internal/db/queries/agent_chat.sql). Neither can apply it to a version in the
// CATALOGUE project: that row is in another schema, their join misses it, and
// the clause is then vacuously true — the "a negative result was believed
// without checking the checker" shape. `currentCatalogueVersionAdmissible`
// applies the same list in Go, on the document the second read actually
// returned, and this test is what keeps the two lists equal.
//
// It reads the list out of the .sql source rather than restating it a third
// time. A test that carried its own copy would agree with itself while both
// production copies drifted.
func TestCatalogueInternalToolListMatchesTheSQLGate(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile(filepath.Join("..", "..", "..", "db", "queries", "agent_chat.sql"))
	if err != nil {
		t.Fatalf("read agent_chat.sql: %v", err)
	}

	fromSQL := internalToolNamesFromSQL(t, string(source))
	fromGo := make([]string, 0, len(currentAuthorableInternalTools))
	for name := range currentAuthorableInternalTools {
		fromGo = append(fromGo, name)
	}
	sort.Strings(fromGo)

	if strings.Join(fromSQL, ",") != strings.Join(fromGo, ",") {
		t.Fatalf(
			"the internal-tool catalogue disagrees between the SQL gate and the catalogue-version gate.\n"+
				"agent_chat.sql: %v\ncurrentAuthorableInternalTools: %v\n"+
				"A name only in SQL refuses a published agent this platform can run; a name only in Go admits one it cannot.",
			fromSQL, fromGo,
		)
	}
}

// internalToolNamesFromSQL pulls every quoted name out of the `NOT IN ( … )`
// lists that gate `internal_tools` in agent_chat.sql, and proves they all agree
// with each other before answering.
//
// The file carries the list four times — the conversation's list and the
// version's list, in the resolve and in the INSERT — and a caller that took the
// first one would not notice a fifth copy going stale.
//
// It scans lines rather than matching one regular expression across the whole
// list, because each list is introduced by a prose comment that itself contains
// a bracket ("(the agent form's own list plus ask_user)"): a non-greedy match to
// the first `)` captures the comment and none of the names, and then reports an
// empty list as agreement.
func internalToolNamesFromSQL(t *testing.T, source string) []string {
	t.Helper()

	listOpener := regexp.MustCompile(`\.value #>> '\{\}' NOT IN \($`)
	namePattern := regexp.MustCompile(`'([a-z_]+)'`)

	var lists [][]string
	lines := strings.Split(source, "\n")
	for index := 0; index < len(lines); index++ {
		if !listOpener.MatchString(strings.TrimRight(lines[index], " \t")) {
			continue
		}
		names := make([]string, 0, 9)
		closed := false
		for cursor := index + 1; cursor < len(lines); cursor++ {
			body := strings.TrimSpace(lines[cursor])
			if body == ")" {
				closed = true
				break
			}
			if strings.HasPrefix(body, "--") {
				continue
			}
			for _, name := range namePattern.FindAllStringSubmatch(body, -1) {
				names = append(names, name[1])
			}
		}
		if !closed {
			t.Fatalf("an internal-tool admission list starting at agent_chat.sql:%d is never closed", index+1)
		}
		if len(names) == 0 {
			t.Fatalf("the internal-tool admission list at agent_chat.sql:%d is empty", index+1)
		}
		sort.Strings(names)
		lists = append(lists, names)
	}

	if len(lists) < 4 {
		t.Fatalf(
			"found %d internal-tool admission lists in agent_chat.sql; the resolve and the INSERT each carry one for the conversation and one for the version, so fewer than four means this test stopped reading the gate it guards",
			len(lists),
		)
	}
	for _, names := range lists[1:] {
		if strings.Join(lists[0], ",") != strings.Join(names, ",") {
			t.Fatalf(
				"agent_chat.sql's internal-tool admission lists disagree with each other: %v vs %v",
				lists[0], names,
			)
		}
	}
	return lists[0]
}

// The catalogue-version gate itself: what it admits and what it refuses.
//
// These need no database — the function reads one JSON document — and they are
// the cheapest place to hold each refusal, so a change to the rule fails here
// before it reaches an integration run.
func TestCatalogueVersionAdmissible(t *testing.T) {
	t.Parallel()

	document := func(overrides map[string]any) json.RawMessage {
		version := map[string]any{
			"status": "published",
			"tools":  []any{},
			"meta":   map[string]any{},
		}
		for key, value := range overrides {
			version[key] = value
		}
		encoded, err := json.Marshal(version)
		if err != nil {
			t.Fatalf("encode fixture: %v", err)
		}
		return encoded
	}

	cases := []struct {
		name     string
		details  json.RawMessage
		admitted bool
		mentions string
	}{
		{
			name:     "a published version with nothing borrowed",
			details:  document(nil),
			admitted: true,
		},
		{
			name:     "a published version whose internal tools are all authorable",
			details:  document(map[string]any{"meta": map[string]any{"internal_tools": []any{"planner", "ask_user"}}}),
			admitted: true,
		},
		{
			name:     "a draft",
			details:  document(map[string]any{"status": "draft"}),
			mentions: "not published",
		},
		{
			name:     "an embedded sub-agent clone",
			details:  document(map[string]any{"status": "embedded"}),
			mentions: "not published",
		},
		{
			name:     "a version that carries a toolkit reference",
			details:  document(map[string]any{"tools": []any{map[string]any{"id": 1, "type": "aha"}}}),
			mentions: "belong to",
		},
		{
			name:     "a version that carries a sub-agent reference",
			details:  document(map[string]any{"tools": []any{map[string]any{"id": 2, "type": "application"}}}),
			mentions: "belong to",
		},
		{
			name:     "a version naming an internal tool this platform does not serve",
			details:  document(map[string]any{"meta": map[string]any{"internal_tools": []any{"planner", "teleport"}}}),
			mentions: "internal tool",
		},
		{
			name:     "a version whose internal-tool list holds something that is not a name",
			details:  document(map[string]any{"meta": map[string]any{"internal_tools": []any{7}}}),
			mentions: "internal tool",
		},
		{
			name:     "a document that is not a version at all",
			details:  json.RawMessage(`[]`),
			mentions: "readable version document",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := currentCatalogueVersionAdmissible(testCase.details)
			if testCase.admitted {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("admitted a version that must not cross a project line")
			}
			if !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
				t.Fatalf("the refusal is not the start classification a route can answer 422 for: %v", err)
			}
			if !strings.Contains(err.Error(), testCase.mentions) {
				t.Fatalf("the refusal does not say why (%q): %v", testCase.mentions, err)
			}
		})
	}
}
