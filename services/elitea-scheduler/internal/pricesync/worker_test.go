package pricesync

import (
	"context"
	"testing"
	"time"
)

func TestNewWorkerDefaults(t *testing.T) {
	w := NewWorker(nil, 0, nil)
	if w.interval != 24*time.Hour {
		t.Errorf("non-positive interval must default to 24h, got %v", w.interval)
	}
	if w.logger == nil {
		t.Error("nil logger must fall back to default")
	}
	if w2 := NewWorker(nil, -time.Second, nil); w2.interval != 24*time.Hour {
		t.Errorf("negative interval must default to 24h, got %v", w2.interval)
	}
}

func TestNewWorkerKeepsInterval(t *testing.T) {
	w := NewWorker(nil, 6*time.Hour, quietLogger())
	if w.interval != 6*time.Hour {
		t.Errorf("interval = %v, want 6h", w.interval)
	}
}

// TestWorkerRunOnceUpserts confirms a pass actually drives an upsert.
func TestWorkerRunOnceUpserts(t *testing.T) {
	src := &fakeSource{name: "seed", denom: Per1M, raws: []RawModelPrice{
		{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(2.5)},
	}}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{src}, quietLogger())
	w := NewWorker(s, time.Hour, quietLogger())

	if outcome, err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: outcome=%q err=%v", outcome, err)
	}
	if len(tx.execArgs) != 1 {
		t.Errorf("Pass must drive one upsert, got %d", len(tx.execArgs))
	}
}
