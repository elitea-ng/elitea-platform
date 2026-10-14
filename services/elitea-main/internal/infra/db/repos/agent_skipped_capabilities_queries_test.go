package repos

import (
	"os"
	"strings"
	"testing"
)

// queryTextsThatFinalizeATurn returns the SQL of the four finalize statements,
// read from the sqlc source so the guard follows the queries, not a copy.
func queryTextsThatFinalizeATurn() []string {
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		return string(raw)
	}
	chat := read("../../../db/queries/agent_chat.sql")
	mixed := read("../../../db/queries/agent_mixed_pause.sql")
	var out []string
	for _, name := range []string{"FinalizeCurrentAgentFullMessage", "FinalizeCurrentAgentHITLPause", "FinalizeCurrentAgentAuthorizationPause"} {
		start := strings.Index(chat, "-- name: "+name+" ")
		end := strings.Index(chat[start+1:], "-- name: ")
		out = append(out, chat[start:start+1+end])
	}
	return append(out, mixed)
}

func TestFinalizeQueriesAreFound(t *testing.T) {
	if got := queryTextsThatFinalizeATurn(); len(got) != 4 {
		t.Fatalf("found %d finalize queries", len(got))
	}
}
