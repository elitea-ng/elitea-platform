package idlecompaction

import (
	"testing"
	"time"
)

var started = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func TestArmSchedulesFromTheLastRequestStart(t *testing.T) {
	due, skip := Arm(Turn{Succeeded: true, PromptCache: true, MessageTokens: MinMessageTokens, RequestStarted: started})
	if skip != SkipNone || !due.Equal(started.Add(4*time.Minute)) {
		t.Fatalf("due=%v skip=%q", due, skip)
	}
	for name, c := range map[string]struct {
		turn Turn
		want SkipCode
	}{
		"failed":       {Turn{PromptCache: true, MessageTokens: 90_000, RequestStarted: started}, SkipNotSucceeded},
		"no cache":     {Turn{Succeeded: true, MessageTokens: 90_000, RequestStarted: started}, SkipNoPromptCache},
		"small":        {Turn{Succeeded: true, PromptCache: true, MessageTokens: MinMessageTokens - 1, RequestStarted: started}, SkipTooSmall},
		"not measured": {Turn{Succeeded: true, PromptCache: true, MessageTokens: 90_000}, SkipNotMeasured},
	} {
		if _, got := Arm(c.turn); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestDecideCancelsOnlyOnTheSameAgentsTurn(t *testing.T) {
	check := Check{RequestStarted: started, SourceExecution: "a", LatestExecution: "a"}
	if got := Decide(check, started.Add(Delay)); got != SkipNone {
		t.Fatalf("due check: %q", got)
	}
	// A peer's turn or a person's message is not an input at all; the same
	// agent's newer execution supersedes, its running turn defers.
	superseded := check
	superseded.LatestExecution = "b"
	if got := Decide(superseded, started.Add(Delay)); got != SkipSuperseded {
		t.Fatalf("same agent's new turn: %q", got)
	}
	busy := check
	busy.AgentBusy = true
	if got := Decide(busy, started.Add(Delay)); got != SkipAgentBusy {
		t.Fatalf("busy: %q", got)
	}
}

func TestDecideSkipsAnExpiredCache(t *testing.T) {
	check := Check{RequestStarted: started, SourceExecution: "a", LatestExecution: "a"}
	if got := Decide(check, started.Add(CacheTTL)); got != SkipCacheExpired {
		t.Fatalf("at expiry: %q", got)
	}
	if got := Decide(check, started.Add(-time.Second)); got != SkipCacheExpired {
		t.Fatalf("clock set back: %q", got)
	}
	if got := Decide(check, started.Add(CacheTTL-time.Second)); got != SkipNone {
		t.Fatalf("just inside: %q", got)
	}
}
