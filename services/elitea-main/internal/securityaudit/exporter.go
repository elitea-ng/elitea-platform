package securityaudit

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
)

// Exporter sends stored events to an external sink (a SIEM collector).
//
// The durable store elitea_identity.security_audit_events is always written
// first, so an exporter is an additional copy. Export receives records in
// seq order. It returns nil only when the sink accepted every record; any
// error keeps the whole batch unexported, and the relay offers it again.
// Delivery is at least once, so a sink must deduplicate on EventID.
//
// libs/go/observability has no OTLP logs exporter today (traces only), so
// this package ships LogExporter. An OTLP logs or webhook exporter
// implements this interface and is selected by operator configuration.
type Exporter interface {
	Name() string
	Export(ctx context.Context, records []Record) error
}

// LogExporter writes one structured log line per event. The event holds
// references only, so the line holds no credential. It is a sink for a log
// pipeline that already ships the service's stdout to a SIEM.
type LogExporter struct {
	Logger *slog.Logger
}

// Name is the exporter's configuration name.
func (LogExporter) Name() string { return "log" }

// Export logs each record.
func (l LogExporter) Export(ctx context.Context, records []Record) error {
	logger := l.Logger
	if logger == nil {
		return errors.New("log exporter needs a logger")
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "security_audit_event",
			slog.Int64("seq", record.Seq),
			slog.String("partition_key", record.PartitionKey),
			slog.Int64("partition_seq", record.PartitionSeq),
			slog.String("event_id", record.EventID),
			slog.String("event_type", string(record.EventType)),
			slog.String("hash", hex.EncodeToString(record.Hash[:])),
			slog.String("cloudevent", string(record.Event)),
		)
	}
	return nil
}
