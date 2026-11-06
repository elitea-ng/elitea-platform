package responsepause

import "testing"

func TestEachWaitingKeyIsAWaitAndOthersAreNot(t *testing.T) {
	for _, key := range []string{"hitl_interrupt", "hitl_interrupts", "authorization_requests", "fanout_parked_v1"} {
		if !Waiting(map[string]any{key: map[string]any{}}) {
			t.Errorf("%s does not wait", key)
		}
	}
	for _, meta := range []map[string]any{
		{},
		{"output_limit_reached": true},
		{"is_error": true},
		{"pipeline_static_v1": map[string]any{}},
		{"thread_id": "t"},
	} {
		if Waiting(meta) {
			t.Errorf("%v waits", meta)
		}
	}
}

func TestSQLNamesEveryKeyUnderTheAlias(t *testing.T) {
	const keys = "ARRAY['hitl_interrupt', 'hitl_interrupts', 'authorization_requests', 'fanout_parked_v1']::text[]"
	for alias, want := range map[Alias]string{
		Unqualified: "(meta ?| " + keys + ")",
		Response:    "(response.meta ?| " + keys + ")",
		Runs:        "(runs.meta ?| " + keys + ")",
		Group:       "(g.meta ?| " + keys + ")",
	} {
		if got := SQL(alias); got != want {
			t.Errorf("SQL(%q) = %s, want %s", alias.name, got, want)
		}
	}
}

func TestSQLRefusesAnAliasOutsideTheBackstop(t *testing.T) {
	for _, name := range []string{"g; DROP", "G", "a.b", "x\nOR TRUE", "abcdefghijklmnopqrstuvwxyzabcdefg"} {
		if got := SQL(Alias{name: name}); got != "FALSE" {
			t.Errorf("SQL(%q) = %s, want FALSE", name, got)
		}
	}
}
