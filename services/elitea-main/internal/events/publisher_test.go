package events

import (
	"context"
	"testing"
)

type mockBus struct {
	published []struct {
		channel   string
		eventType string
		payload   interface{}
	}
}

func (m *mockBus) Publish(_ context.Context, channel string, eventType string, payload interface{}) error {
	m.published = append(m.published, struct {
		channel   string
		eventType string
		payload   interface{}
	}{channel, eventType, payload})
	return nil
}

func TestPublisher_Emit(t *testing.T) {
	bus := &mockBus{}
	pub := NewPublisher(bus)

	pub.Emit(context.Background(), "proj-123", EventApplicationCreated, DomainEvent{
		ProjectID:  "proj-123",
		EntityID:   "app-1",
		EntityType: "application",
		Action:     "created",
	})

	if len(bus.published) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(bus.published))
	}
	if bus.published[0].channel != "project:proj-123:events" {
		t.Errorf("unexpected channel: %s", bus.published[0].channel)
	}
	if bus.published[0].eventType != EventApplicationCreated {
		t.Errorf("unexpected event type: %s", bus.published[0].eventType)
	}
}

// Canvas presence publishes on its own family, never the gateway's subject,
// and to no webhook sink (#1076).
func TestPresencePublisherUsesThePresenceChannel(t *testing.T) {
	bus := &mockBus{}
	NewPresencePublisher(bus).Emit(context.Background(), "7", "canvas.editors", map[string]any{})
	if len(bus.published) != 1 || bus.published[0].channel != "project:7:presence" {
		t.Fatalf("published = %+v, want one event on project:7:presence", bus.published)
	}
	if PresenceChannel("7") == ProjectChannel("7") {
		t.Fatal("the presence and gateway families share a channel")
	}
}

func TestProjectChannel(t *testing.T) {
	ch := ProjectChannel("abc-123")
	if ch != "project:abc-123:events" {
		t.Errorf("unexpected channel format: %s", ch)
	}
}
