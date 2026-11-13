package repos

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// The diagnosis takes its internal-tools allowlist from
// currentAuthorableInternalTools, the Go copy of the list the resolver SQL
// carries (agent_chat.sql). This keeps that Go copy equal to every list in the
// SQL source, so the diagnosis cannot drift from the resolver.
func TestAuthorableInternalToolsEqualEveryResolverSQLList(t *testing.T) {
	source, err := os.ReadFile("../../../db/queries/agent_chat.sql")
	if err != nil {
		t.Fatal(err)
	}
	lists := regexp.MustCompile(`(?m)^\s*'ask_user'[^)]*\)`).FindAllString(string(source), -1)
	if len(lists) == 0 {
		t.Fatal("no internal-tools list found in agent_chat.sql")
	}
	want := authorableInternalToolNames()
	for _, list := range lists {
		var got []string
		for _, match := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(list, -1) {
			got = append(got, match[1])
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("resolver SQL list %v differs from currentAuthorableInternalTools %v", got, want)
		}
	}
}
