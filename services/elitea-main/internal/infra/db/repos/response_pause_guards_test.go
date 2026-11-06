package repos

import (
	"os"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/responsepause"
)

// FU-284: a query that continues or resumes a response only when it does not
// wait for a person refuses every waiting key, a parked fan-out included, so
// an output-limit continuation or a static resume cannot start beside open
// cards.
func TestResumeGuardsRefuseEveryWaitingKey(t *testing.T) {
	for file, names := range map[string][]string{
		"../../../db/queries/agent_chat.sql":         {"ResolveCurrentOutputLimitContinuation", "ResumeCurrentAgentOutputLimit"},
		"../../../db/queries/agent_static_pause.sql": {"ResolveCurrentStaticContinuation", "ResumeCurrentAgentStatic"},
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			query := namedQuery(string(raw), name)
			if query == "" {
				t.Fatalf("%s: no query %s", file, name)
			}
			for _, key := range responsepause.WaitingKeys() {
				if !strings.Contains(query, "NOT (response.meta ? '"+key+"')") {
					t.Errorf("%s does not refuse %s", name, key)
				}
			}
		}
	}
}

// namedQuery is the text of one sqlc query, from its "-- name:" line to the
// next one.
func namedQuery(file, name string) string {
	start := strings.Index(file, "-- name: "+name+" ")
	if start < 0 {
		return ""
	}
	rest := file[start+len("-- name: "):]
	if end := strings.Index(rest, "-- name: "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}
