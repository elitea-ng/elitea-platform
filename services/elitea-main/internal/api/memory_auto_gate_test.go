package api

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryAutoGateWithoutPoolIsOff(t *testing.T) {
	if gate := newMemoryAutoGate(nil); gate != nil {
		t.Fatalf("no pool: gate %v, want nil", gate)
	}
}

// A gate that could not be built refuses (fail closed): its reads error, which
// the save answers as a 500 for a write auto, never as acceptance.
func TestMemoryAutoGateThatCouldNotBeBuiltFailsClosed(t *testing.T) {
	gate := &memoryAutoGate{err: errors.New("no tier reader")}
	if _, err := gate.MaxTier(context.Background()); err == nil {
		t.Fatal("MaxTier answered without an error")
	}
	if _, err := gate.AgentTier(context.Background(), 1, 2); err == nil {
		t.Fatal("AgentTier answered without an error")
	}
}
