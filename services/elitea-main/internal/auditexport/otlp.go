package auditexport

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/elitea-ng/elitea-platform/libs/go/observability"
)

// LogsExporter is the slice of observability.LogsExporter the sink uses.
type LogsExporter interface {
	Export(ctx context.Context, entries []observability.LogEntry) error
}

// EventName is the OpenTelemetry event name of every exported record.
const EventName = "elitea.audit.event"

// OTLPSink sends events as OTLP log records: the whole envelope as the JSON
// body, and the fields a SIEM filters on as flat attributes.
type OTLPSink struct{ exporter LogsExporter }

// NewOTLPSink wraps an OTLP logs exporter.
func NewOTLPSink(exporter LogsExporter) (*OTLPSink, error) {
	if exporter == nil {
		return nil, fmt.Errorf("otlp sink needs an exporter")
	}
	return &OTLPSink{exporter: exporter}, nil
}

// Name is the sink's configuration name.
func (*OTLPSink) Name() string { return "otlp" }

// Send exports the events as one OTLP request. Every failure is transient: an
// OTLP receiver that answers 400 is a configuration problem, not a bad event.
func (s *OTLPSink) Send(ctx context.Context, events []Event) error {
	entries := make([]observability.LogEntry, 0, len(events))
	for _, e := range events {
		body, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode event %s: %w", e.EventID, err)
		}
		at, err := time.Parse(time.RFC3339Nano, e.Time)
		if err != nil {
			at = time.Now()
		}
		severity := otellog.SeverityInfo
		if e.Outcome == OutcomeDeny || e.Outcome == OutcomeFailure {
			severity = otellog.SeverityWarn
		}
		attrs := map[string]string{
			"elitea.audit.schema":     e.Schema,
			"elitea.audit.event_id":   e.EventID,
			"elitea.audit.source":     string(e.Source),
			"elitea.audit.event_type": e.EventType,
			"elitea.audit.action":     e.Action,
			"elitea.audit.outcome":    e.Outcome,
			"elitea.audit.actor.type": e.Actor.Type,
			"elitea.audit.actor.id":   e.Actor.ID,
		}
		if e.Tenant.ProjectID != nil {
			attrs["elitea.audit.tenant.project_id"] = strconv.FormatInt(*e.Tenant.ProjectID, 10)
		}
		if e.OCSF != nil {
			attrs["elitea.audit.ocsf.class_uid"] = strconv.Itoa(e.OCSF.ClassUID)
		}
		entries = append(entries, observability.LogEntry{
			EventName: EventName, Time: at, Severity: severity, Body: string(body), Attributes: attrs,
		})
	}
	return s.exporter.Export(ctx, entries)
}
