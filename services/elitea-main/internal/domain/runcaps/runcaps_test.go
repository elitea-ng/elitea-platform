package runcaps

import "testing"

func TestEveryCapHasItsOwnMessage(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, cap := range All {
		if !Valid(cap) {
			t.Fatalf("%s is not valid", cap)
		}
		message := Message(cap)
		if message == "" || seen[message] || len(message) > 256 {
			t.Fatalf("%s: message %q", cap, message)
		}
		seen[message] = true
		worker, ok := WorkerCap(message)
		if want := cap != MaxWallSeconds && cap != MaxSpend; ok != want || (ok && worker != cap) {
			t.Fatalf("%s: WorkerCap = %q, %t", cap, worker, ok)
		}
	}
	if Valid("max_everything") || Message("max_everything") != "" {
		t.Fatal("an unknown cap was accepted")
	}
}
