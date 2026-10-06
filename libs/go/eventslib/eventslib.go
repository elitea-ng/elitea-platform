// Package eventslib provides shared event types and publishing interfaces for
// the Elitea platform event bus.
package eventslib

import "time"

// Event is the envelope for all platform domain events.
type Event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Source    string    `json:"source"`
	TenantID  string    `json:"tenant_id"`
	Timestamp time.Time `json:"timestamp"`
	Payload   any       `json:"payload"`
}

// Publisher is the interface for publishing events.
// TODO: implement with a NATS (JetStream) backend.
type Publisher interface {
	Publish(event Event) error
}

// NoopPublisher discards all events; useful in tests.
type NoopPublisher struct{}

func (NoopPublisher) Publish(_ Event) error { return nil }
