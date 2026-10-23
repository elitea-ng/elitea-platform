package account

// overhead_mark_test.go — issue #17. GetKeysForProvider is the credential
// resolution the BFF.9d overhead metric used to miss. These tests pin the mark
// that carries it back to the /llm handler.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/overhead"
)

// meterCtx builds the context bifrost/core hands to GetKeysForProvider: a
// BifrostContext that carries the caller's project and an overhead Meter.
func meterCtx(projectID string) (context.Context, *overhead.Meter) {
	bc := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	bc.SetValue(schemas.BifrostContextKeyVirtualKey, projectID)
	return bc, overhead.Attach(bc, time.Now())
}

// TestGetKeysForProvider_MarksOverheadMeter asserts a successful credential
// resolution marks the Meter. Without the mark X-Elapsed-Ms reports the
// pre-dispatch time alone, and the gate understates the gateway overhead.
func TestGetKeysForProvider_MarksOverheadMeter(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-1", "OpenAI prod", []byte(`{"api_base":"https://api.openai.com/v1","api_key":"sk-literal"}`)},
	}}
	a := newTestAccount(t, db, &fakeVault{})

	ctx, meter := meterCtx("42")
	if got := meter.Overhead(0); got != 0 {
		t.Fatalf("meter carried a mark before the call: %v", got)
	}

	keys, err := a.GetKeysForProvider(ctx, schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(keys))
	}
	if got := meter.Overhead(0); got <= 0 {
		t.Fatal("GetKeysForProvider left the overhead meter unmarked: X-Elapsed-Ms then excludes credential resolution (issue #17)")
	}
}

// TestGetKeysForProvider_MarksOverheadMeterOnRefusal asserts a refused
// resolution marks the Meter too. A vault failure costs the caller the same
// gateway time as a success, so the metric must count it.
func TestGetKeysForProvider_MarksOverheadMeterOnRefusal(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-2", "OpenAI", []byte(`{"api_base":"https://api.openai.com/v1","api_key":"{{secret.OPENAI_KEY}}"}`)},
	}}
	a := newTestAccount(t, db, &fakeVault{err: errors.New("vault unavailable")})

	ctx, meter := meterCtx("42")
	if _, err := a.GetKeysForProvider(ctx, schemas.OpenAI); err == nil {
		t.Fatal("expected a vault error")
	}
	if got := meter.Overhead(0); got <= 0 {
		t.Fatal("a refused credential resolution left the overhead meter unmarked (issue #17)")
	}
}

// TestGetKeysForProvider_NoMeterOnContextIsSafe asserts the mark is a no-op
// when no handler attached a Meter — the shape every non-/llm caller has.
func TestGetKeysForProvider_NoMeterOnContextIsSafe(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-3", "OpenAI", []byte(`{"api_base":"https://api.openai.com/v1","api_key":"sk-literal"}`)},
	}}
	a := newTestAccount(t, db, &fakeVault{})

	if _, err := a.GetKeysForProvider(ctxWithProject("42"), schemas.OpenAI); err != nil {
		t.Fatalf("GetKeysForProvider without a meter: %v", err)
	}
}
