package repos

// The one-time record of inbound delegation assertions (shared 0210)
// against a real migrated database: a pair is bound to its first
// presenter, the same presenter may present it again, another one is a
// replay, and concurrent first presentations bind exactly one presenter.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The run read plans against the migrated schema; an unknown run is not an
// agent run (the mint then refuses).
func TestRemoteAgentRunsReadsTheMigratedSchema(t *testing.T) {
	runs, err := NewRemoteAgentRuns(newMigratedPostgresIntegrationPool(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = runs.ReadRun(context.Background(), "exec-missing", 1)
	if err == nil || !strings.Contains(err.Error(), "not an agent run") {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoteAgentReplaysBindTheFirstPresenter(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	replays, err := NewRemoteAgentReplays(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	expires := time.Now().Add(5 * time.Minute)
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, step := range []struct {
		presenter string
		want      bool
	}{{first, true}, {first, true}, {second, false}, {first, true}} {
		got, err := replays.Claim(ctx, "https://a.example/agent-issuer", "jti-1", step.presenter, expires)
		if err != nil || got != step.want {
			t.Fatalf("claim by %s: %v, %v; want %v", step.presenter[:1], got, err, step.want)
		}
	}
	// Another issuer's jti of the same value is another assertion.
	if got, err := replays.Claim(ctx, "https://b.example/agent-issuer", "jti-1", second, expires); err != nil || !got {
		t.Fatalf("other issuer: %v, %v", got, err)
	}
	// A presenter that is not a digest is refused by the table.
	if _, err := replays.Claim(ctx, "https://a.example/agent-issuer", "jti-2", "token:1", expires); err == nil {
		t.Fatal("a raw presenter was stored")
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			presenter := strings.Repeat(string(rune('0'+i)), 64)
			got, err := replays.Claim(ctx, "https://a.example/agent-issuer", "jti-race", presenter, expires)
			if err != nil {
				t.Errorf("race claim: %v", err)
				return
			}
			if got {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d presenters won one assertion", wins)
	}
}
