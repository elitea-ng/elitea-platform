package conversations

// Issue 1234: the fork's mode, branch summary and fork_id. The Postgres
// behaviour is in repos/conversation_fork_postgres_integration_test.go.

import (
	"encoding/json"
	"strings"
	"testing"
)

func validSummaryValue() map[string]any {
	return map[string]any{"version": float64(1), "objective": "Add a cache.", "constraints": []any{},
		"decisions": []any{"Use an LRU."}, "key_facts": []any{}, "completed_work": []any{map[string]any{"result": "Wrote it.", "evidence_refs": []any{}}},
		"open_work": []any{}, "next_steps": []any{}, "unresolved_issues": []any{}, "references": []any{}}
}

func TestForkOptions(t *testing.T) {
	for name, tc := range map[string]struct {
		body      map[string]any
		messageID string
		wantMode  string
		wantError bool
	}{
		"no options is a fork":             {body: map[string]any{}, wantMode: ForkModeFork},
		"a rewind at an answer":            {body: map[string]any{"mode": "rewind"}, messageID: "m", wantMode: ForkModeRewind},
		"a rewind with a summary":          {body: map[string]any{"mode": "rewind", "branch_summary": validSummaryValue()}, messageID: "m", wantMode: ForkModeRewind},
		"a rewind needs its fork point":    {body: map[string]any{"mode": "rewind"}, wantError: true},
		"another mode":                     {body: map[string]any{"mode": "branch"}, wantError: true},
		"a summary is a rewind's only":     {body: map[string]any{"branch_summary": validSummaryValue()}, messageID: "m", wantError: true},
		"a fork_id that is not a UUID":     {body: map[string]any{"fork_id": "abc"}, wantError: true},
		"a summary of another version":     {body: map[string]any{"mode": "rewind", "branch_summary": map[string]any{"version": float64(2)}}, messageID: "m", wantError: true},
		"a summary with an unknown key":    {body: map[string]any{"mode": "rewind", "branch_summary": withKey(validSummaryValue(), "note", "x")}, messageID: "m", wantError: true},
		"a summary with too long a string": {body: map[string]any{"mode": "rewind", "branch_summary": withKey(validSummaryValue(), "objective", strings.Repeat("x", 2049))}, messageID: "m", wantError: true},
	} {
		t.Run(name, func(t *testing.T) {
			request := ForkRequest{MessageID: tc.messageID}
			err := forkOptions(tc.body, &request)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v", err, tc.wantError)
			}
			if err == nil && request.Mode != tc.wantMode {
				t.Fatalf("mode = %q, want %q", request.Mode, tc.wantMode)
			}
		})
	}
	request := ForkRequest{}
	if err := forkOptions(map[string]any{"fork_id": "6F9619FF-8B86-D011-B42D-00C04FC964FF"}, &request); err != nil || request.ForkID != "6f9619ff-8b86-d011-b42d-00c04fc964ff" {
		t.Fatalf("fork_id = %q (%v), want it in canonical form", request.ForkID, err)
	}
}

func withKey(summary map[string]any, key string, value any) map[string]any {
	summary[key] = value
	return summary
}

func TestBuildBranchSummaryIsABoundedV1Record(t *testing.T) {
	long := strings.Repeat("word ", 3000)
	turns := []AbandonedTurn{{Question: true, Text: "Rewrite  the\nparser"}, {Text: "I rewrote it."}}
	for range 40 {
		turns = append(turns, AbandonedTurn{Question: true, Text: long}, AbandonedTurn{Text: long})
	}
	summary := BuildBranchSummary(turns)
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxBranchSummaryBytes {
		t.Fatalf("the summary is %d bytes", len(raw))
	}
	// It is a record the fork itself would accept from a client.
	if _, err := validBranchSummary(summary); err != nil {
		t.Fatalf("the built summary does not validate: %v", err)
	}
	facts, _ := summary["key_facts"].([]any)
	if len(facts) == 0 || facts[0] != "Asked: Rewrite the parser" || facts[1] != "Answered: I rewrote it." {
		t.Fatalf("key_facts = %v", facts)
	}
	if summary["objective"] != "The abandoned attempt was asked: Rewrite the parser" {
		t.Fatalf("objective = %v", summary["objective"])
	}
	if work, _ := summary["completed_work"].([]any); len(work) != 0 {
		t.Fatalf("an abandoned turn is never completed work: %v", work)
	}
}

// Issue 1420: `<`, `>` and `&` count one byte each, as serde_json writes
// them: a summary full of them that the desktop built at the bound is
// accepted, and the server's own (stored, then read back) still validates.
func TestBranchSummaryBoundIgnoresHTMLEscaping(t *testing.T) {
	angles := strings.Repeat("<", 2000)
	client := validSummaryValue()
	facts := make([]any, 0, 15)
	for range 15 {
		facts = append(facts, angles)
	}
	client["key_facts"] = facts
	if raw, _ := json.Marshal(client); len(raw) <= maxBranchSummaryBytes {
		t.Fatalf("the fixture must exceed the bound under HTML escaping (%d bytes)", len(raw))
	}
	if _, err := validBranchSummary(client); err != nil {
		t.Fatalf("a 30 KiB summary of '<' is refused: %v", err)
	}
	client["key_facts"] = append(facts, angles, angles)
	if _, err := validBranchSummary(client); err == nil {
		t.Fatal("a summary over 32 KiB is accepted")
	}

	turns := []AbandonedTurn{}
	for range 40 {
		turns = append(turns, AbandonedTurn{Question: true, Text: angles}, AbandonedTurn{Text: "&" + angles})
	}
	built := BuildBranchSummary(turns)
	stored, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	var read map[string]any
	if err := json.Unmarshal(stored, &read); err != nil {
		t.Fatal(err)
	}
	if _, err := validBranchSummary(read); err != nil {
		t.Fatalf("the built summary of '<' does not validate when read back: %v", err)
	}
	if facts, _ := built["key_facts"].([]any); len(facts) == 0 {
		t.Fatal("no facts kept")
	}
	// Text that escapes to six bytes a character still fits: facts go.
	controls := strings.Repeat("\x01", 1500)
	turns = turns[:0]
	for range 40 {
		turns = append(turns, AbandonedTurn{Question: true, Text: controls})
	}
	if _, err := validBranchSummary(BuildBranchSummary(turns)); err != nil {
		t.Fatalf("a summary of control characters does not validate: %v", err)
	}
}

func TestForkedFromNamesTheModeAndTheCut(t *testing.T) {
	cut := "6f9619ff-8b86-d011-b42d-00c04fc964ff"
	meta := map[string]any{ForkedFromMetaKey: map[string]any{"conversation_id": float64(5), "mode": "rewind",
		"cut_message_uuid": cut, "branch_summary": validSummaryValue(), "fork_id": cut}}
	ff := forkedFrom(meta)
	if ff["mode"] != ForkModeRewind || ff["cut_message_uuid"] != cut || ff["fork_id"] != nil || ff["branch_summary"] != nil {
		t.Fatalf("forked_from = %v", ff)
	}
	annotateBranchSummary(meta, ff)
	if summary, _ := ff["branch_summary"].(map[string]any); summary["objective"] != "Add a cache." {
		t.Fatalf("the detail's branch_summary = %v", ff["branch_summary"])
	}
	// A fork made before rewinds is a fork, and carries no summary.
	older := map[string]any{ForkedFromMetaKey: map[string]any{"conversation_id": float64(5), "branch_summary": validSummaryValue()}}
	ff = forkedFrom(older)
	annotateBranchSummary(older, ff)
	if ff["mode"] != ForkModeFork || ff["branch_summary"] != nil {
		t.Fatalf("an older fork's forked_from = %v", ff)
	}
}
