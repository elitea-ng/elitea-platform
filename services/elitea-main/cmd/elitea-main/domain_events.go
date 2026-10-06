package main

import "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/events"

// newDomainEventsPublisher builds the one domain-events Publisher the
// composition root shares between every producer (#876).
//
// It deliberately takes NO events.Bus: its Bus is always events.NoopBus{}, so
// domain events (conversation.created, artifact.uploaded, pipeline.run.*,
// schedule.fired, agent.version.*, moderation.request.decided) reach their
// webhook sinks and nothing else. They must not reach the project SSE stream:
// that stream is gated on models.project_context.view, which every project
// viewer holds, while a conversation.created payload carries a PRIVATE
// conversation's name and creator, and no web client consumes these types
// anyway. Taking the bus out of the signature is what keeps a later
// "just pass the live bus" edit from reopening that leak silently;
// TestDomainEventsStayOffTheLiveBus pins main.go to this constructor.
func newDomainEventsPublisher(sinks ...events.Sink) *events.Publisher {
	return events.NewPublisher(events.NoopBus{}, sinks...)
}
