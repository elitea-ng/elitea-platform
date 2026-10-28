package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auditexport"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// auditExportAtBoot builds the SIEM audit export (ADR-0042 decision 3, #1494)
// and starts its audit_events relay, backlog monitor and metrics listener.
// With neither ELITEA_AUDIT_EXPORT_OTLP_ENDPOINT nor
// ELITEA_AUDIT_EXPORT_WEBHOOK_URL set it returns nil and starts nothing, so
// an install that does not export behaves exactly as before. A bad value
// refuses the boot. The returned service's SecurityExporter is given to the
// security audit relay, which keeps each outbox row pending until the sinks
// accepted it. closeFn shuts the OTLP exporter down.
func auditExportAtBoot(
	ctx context.Context,
	lookup func(string) (string, bool),
	admission profile.Admission,
	pool *pgxpool.Pool,
	logger *slog.Logger,
) (service *auditexport.Service, closeFn func(context.Context) error, err error) {
	cfg, err := auditexport.ConfigFromEnv(lookup)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Enabled() {
		return nil, nil, nil
	}
	if pool == nil {
		return nil, nil, fmt.Errorf("audit export needs the platform database")
	}
	service, err = auditexport.Build(ctx, cfg, auditexport.Deps{DB: pool, Logger: logger})
	if err != nil {
		return nil, nil, err
	}
	go func() {
		if runErr := service.Run(ctx, admission.Governed()); runErr != nil && ctx.Err() == nil {
			logger.Error("audit export relay stopped", "err", runErr)
		}
	}()
	logger.Info("audit export started",
		"otlp", cfg.OTLPEndpoint != "", "webhook", cfg.WebhookURL != "",
		"audit_events", cfg.AuditEvents, "security_audit", admission.Governed())
	return service, service.Close, nil
}

// securityAuditExporter is the exporter for the security audit relay: nil
// without an audit export, so the relay marks rows exported when stored.
func securityAuditExporter(service *auditexport.Service) securityaudit.Exporter {
	if service == nil {
		return nil
	}
	return service.SecurityExporter()
}
