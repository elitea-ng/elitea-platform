package responsepause

import "testing"

func TestWaitingKeysAreTheFourPauseKindsAndACopy(t *testing.T) {
	keys := WaitingKeys()
	want := []string{"hitl_interrupt", "hitl_interrupts", "authorization_requests", "fanout_parked_v1"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v", keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
	}
	keys[0] = "changed"
	if WaitingKeys()[0] != "hitl_interrupt" {
		t.Fatal("a caller changed the shared set")
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
