package events

import (
	"context"
	"fmt"
	"log/slog"
)

const (
	EventApplicationCreated = "application.created"
	EventApplicationUpdated = "application.updated"
	EventApplicationDeleted = "application.deleted"

	EventSkillCreated = "skill.created"
	EventSkillUpdated = "skill.updated"
	EventSkillDeleted = "skill.deleted"

	EventFolderCreated = "folder.created"
	EventFolderUpdated = "folder.updated"
	EventFolderDeleted = "folder.deleted"

	EventConversationCreated = "conversation.created"
	EventConversationUpdated = "conversation.updated"
	EventConversationDeleted = "conversation.deleted"

	EventMessageCreated = "message.created"

	// EventBudgetSoftAlert is the LLM-gateway 80%-threshold soft-alert event
	// (design §8.3). It flows on the gateway.events.* subject when the NATS
	// EventBus (internal/infra/natsbus) is wired; the gateway emits it and
	// elitea-main subscribers (and the project SSE stream) receive it.
	EventBudgetSoftAlert = "budget.soft_alert"

	// The five events below were added for #876's second half: wiring the
	// outbound webhook Dispatcher (internal/api/webhook) to real producers
	// instead of leaving it composed against nothing. Each constant is
	// wired at exactly one call site — see that site's own comment for the
	// producer and for why its sibling ("pipeline run FINISHED") is a
	// declared, not a wired, event (below).

	// EventPipelineRunStarted fires the moment an unattended pipeline run is
	// admitted — internal/api/v2/pipelinetriggers/run.go's admit(), the one
	// path both the inbound trigger and the schedule tick use. "Admitted"
	// and "finished" are different facts here: admit() returns as soon as
	// StartCurrentApplication accepts the turn, well before the pipeline's
	// graph has run. See EventPipelineRunSucceeded's comment for why this
	// package does not also emit that fact yet.
	EventPipelineRunStarted = "pipeline.run.started"
	// EventPipelineRunSucceeded and EventPipelineRunFailed are DECLARED, not
	// wired. A pipeline is an ordinary agent-execution turn (run.go's own
	// package doc: "both halves end in the SAME call the chat composer
	// makes"), and this platform's turn completion is settled deep inside
	// the claim-fence/settlement machinery — internal/application/output
	// .AgentExecutionService.IngestAgent, fed by a gRPC frame from a worker
	// process minutes after admission, behind the fence-verification
	// contract internal/application/execution/settlement.go owns — or, on
	// the native runtime, the equivalent Rust-engine claim/settle path.
	// Neither exposes a single safe, already-isolated hook that reports
	// "this execution just finished, succeeded or failed" without editing
	// that fencing logic itself, which chat's own history in this repository
	// (turn-settlement races, claim-fence staleness, #133/#143) says is not
	// a change to make as a side effect of an unrelated feature. These two
	// constants exist so a future change that DOES find that safe hook is a
	// one-line Emit call and a vocabulary the UI, the docs and the delivery
	// log already agree on.
	EventPipelineRunSucceeded = "pipeline.run.succeeded"
	EventPipelineRunFailed    = "pipeline.run.failed"
	// EventScheduleFired fires from the SAME admit() call as
	// EventPipelineRunStarted, additionally, when the run's origin is a
	// pipeline_schedules row rather than an inbound trigger. It is elitea-
	// main's OWN scheduling job framework (internal/application/scheduling,
	// registered as pipelinetriggers.ScheduleJob) firing the tick — not a
	// callback FROM the separate elitea-scheduler service INTO elitea-main.
	// elitea-scheduler has no HTTP hook into elitea-main for pipeline
	// schedules to answer through, so that direction is not wired.
	EventScheduleFired = "schedule.fired"
	// EventAgentVersionPublished and EventAgentVersionUnpublished fire from
	// internal/api/v2/eliteacore/handler.go's Publish and Unpublish, once the
	// publish/unpublish transaction has committed.
	EventAgentVersionPublished   = "agent.version.published"
	EventAgentVersionUnpublished = "agent.version.unpublished"
	// EventArtifactUploaded fires from internal/api/v2/artifacts/objects.go's
	// UploadObject, once the object is durably stored.
	EventArtifactUploaded = "artifact.uploaded"
	// EventModerationRequestDecided fires from internal/api/v2/moderation
	// /requests.go's AdministrationRequestUpdate, for every issue_type this
	// resource carries — an ordinary app request AND a Project Request
	// (issue #871) alike, since both flow through the same decision path and
	// a webhook watching this event has no reason to care which table-shape
	// underlies the row.
	EventModerationRequestDecided = "moderation.request.decided"
)

// EventCatalogueEntry names one event type this platform can emit and says
// whether a real producer is wired to it yet. It is the source
// webhook.EventCatalogue (internal/api/webhook) re-exports for the create/
// update handlers' documentation and for tests that assert the picker the
// web client renders (apps/elitea-web/src/features/settings/lib/webhooks
// /webhookEventCatalogue.ts) has not drifted from this list.
type EventCatalogueEntry struct {
	Type        string
	Description string
	// Wired is false for an event this package has named but no producer in
	// this repository emits yet (EventPipelineRunSucceeded and
	// EventPipelineRunFailed, today). A webhook may still list a not-yet-
	// wired event in its `events` array — the API enforces no fixed
	// vocabulary — it simply never fires until a producer is.
	Wired bool
}

// Catalogue is every event type this package declares, in the stable order
// the picker renders them. It is deliberately NOT every string a webhook's
// `events` array may legally hold — the API is free text — but every value
// here is one this repository's own producers can (or, for the two
// EventPipelineRun* entries, will) actually emit.
var Catalogue = []EventCatalogueEntry{
	{Type: EventPipelineRunStarted, Description: "An unattended pipeline run was admitted (inbound trigger or schedule).", Wired: true},
	{Type: EventPipelineRunSucceeded, Description: "A pipeline run finished successfully.", Wired: false},
	{Type: EventPipelineRunFailed, Description: "A pipeline run finished with an error.", Wired: false},
	{Type: EventScheduleFired, Description: "A pipeline's cron schedule fired and admitted a run.", Wired: true},
	{Type: EventAgentVersionPublished, Description: "An agent (or pipeline) version was published to the catalog.", Wired: true},
	{Type: EventAgentVersionUnpublished, Description: "A published agent version was withdrawn.", Wired: true},
	{Type: EventConversationCreated, Description: "A new chat conversation was created.", Wired: true},
	{Type: EventArtifactUploaded, Description: "A file was uploaded to a project artifact bucket.", Wired: true},
	{Type: EventModerationRequestDecided, Description: "A moderation or project-creation request was approved or rejected.", Wired: true},
}

// AllEventTypes is Catalogue's Type column, for a caller that only needs the
// vocabulary (e.g. a picker's option list).
func AllEventTypes() []string {
	types := make([]string, len(Catalogue))
	for i, entry := range Catalogue {
		types[i] = entry.Type
	}
	return types
}

type Bus interface {
	Publish(ctx context.Context, channel string, eventType string, payload interface{}) error
}

// NoopBus discards every publish. It is the composition root's fallback Bus
// when no Redis (and no NATS) EventBus is configured, so a deployment with
// neither still gets a working domain-events Publisher: the project SSE
// stream simply has nothing to read (it is nil-gated separately in
// router.go), but webhook delivery — which reaches this Publisher's Sinks,
// not its Bus — still works, because Emit calls both regardless of whether
// the Bus publish succeeds.
type NoopBus struct{}

func (NoopBus) Publish(context.Context, string, string, interface{}) error { return nil }

// Sink receives every event a Publisher Emits, in addition to the Bus
// publish. webhook.Dispatcher implements this (structurally — this package
// does not import internal/api/webhook, which would be a cycle) to turn a
// domain event into signed HTTP deliveries. A Sink's own work must not block
// the producer that called Emit: Publisher dispatches to each Sink on its own
// goroutine, so a slow or panicking Sink cannot stall a conversation create,
// an artifact upload, or any other request-path Emit call.
type Sink interface {
	HandleDomainEvent(ctx context.Context, projectID, eventType string, payload any)
}

type Publisher struct {
	bus   Bus
	sinks []Sink
}

// NewPublisher builds a Publisher over bus, additionally fanning out every
// Emit to sinks (typically the webhook Dispatcher). sinks is variadic and may
// be empty — every existing caller that built a presence-only Publisher over
// its own EventBus keeps working unchanged.
func NewPublisher(bus Bus, sinks ...Sink) *Publisher {
	return &Publisher{bus: bus, sinks: sinks}
}

func (p *Publisher) Emit(ctx context.Context, projectID, eventType string, payload any) {
	channel := ProjectChannel(projectID)
	if err := p.bus.Publish(ctx, channel, eventType, payload); err != nil {
		slog.Error("events: publish failed", "type", eventType, "project", projectID, "err", err)
	}
	for _, sink := range p.sinks {
		if sink == nil {
			continue
		}
		// Detached from ctx's cancellation: an HTTP handler's request
		// context is cancelled the moment the handler returns, and a
		// webhook delivery that has only just been admitted to a queue must
		// outlive that. WithoutCancel keeps ctx's values (trace ids, if any
		// are ever attached) while dropping its deadline.
		go sink.HandleDomainEvent(context.WithoutCancel(ctx), projectID, eventType, payload)
	}
}

func ProjectChannel(projectID string) string {
	return fmt.Sprintf("project:%s:events", projectID)
}

type DomainEvent struct {
	ProjectID  string `json:"project_id"`
	EntityID   string `json:"entity_id"`
	EntityType string `json:"entity_type"`
	Action     string `json:"action"`
}

// SoftAlertPayload is the soft-alert event body published by elitea-llm-gateway
// on gateway.events.project.<id>.events (design §8.3). The normative contract
// is implemented in services/elitea-llm-gateway/internal/llmproxy/budget_gate.go
// (softAlertPayload struct). Subscribers receive it within a softAlertEnvelope
// (type, source, payload, timestamp). All cost values are int64 nano-USD to
// maintain parity with the NATS governance counters and avoid floating-point
// precision loss.
type SoftAlertPayload struct {
	ProjectID          string `json:"project_id"`
	Scope              string `json:"scope"`
	PeriodStartUnix    int64  `json:"period_start_unix"`
	CostJustBilledNano int64  `json:"cost_just_billed_nano"`

	// Deprecated/historical fields from an earlier design (issue #16):
	// These fields were in the original spec but never implemented in the gateway.
	// Kept as reference for context. DO NOT use these in new code.
	// EventType       string  `json:"event_type"`         // now in envelope.Type
	// OrgID           string  `json:"org_id"`              // never emitted by gateway
	// ThresholdPct    int     `json:"threshold_pct"`       // implicit in business logic
	// AccumulatedCost float64 `json:"accumulated_cost"`   // changed to CostJustBilledNano (int64)
	// Limit           float64 `json:"limit"`               // never emitted by gateway
	// Timestamp       string  `json:"timestamp"`           // now in envelope.Timestamp (time.Time)
}
