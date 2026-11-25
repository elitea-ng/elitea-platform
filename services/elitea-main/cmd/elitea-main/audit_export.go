package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/breaker"
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
//
// The export's metrics listener (ELITEA_AUDIT_EXPORT_METRICS_ADDR) is the
// main metrics listener: it also serves the runtime safety metrics
// (safetyMetrics: active suspensions, breaker trips) and the break-glass
// metrics (ADR-0059 R2.10a: active elevations, activations). With no
// exporter configured, the listener still starts when the address is set,
// and serves those alone, so the breaker, kill switch and break-glass alerts
// work on every install.
func auditExportAtBoot(
	ctx context.Context,
	lookup func(string) (string, bool),
	admission profile.Admission,
	pool *pgxpool.Pool,
	safetyMetrics *breaker.Metrics,
	breakGlassMetrics *adminaccess.BreakGlassMetrics,
	logger *slog.Logger,
) (service *auditexport.Service, closeFn func(context.Context) error, err error) {
	cfg, err := auditexport.ConfigFromEnv(lookup)
	if err != nil {
		return nil, nil, err
	}
	extra := mainMetrics{}
	if safetyMetrics != nil {
		extra = append(extra, safetyMetrics)
	}
	if breakGlassMetrics != nil {
		extra = append(extra, breakGlassMetrics)
	}
	if !cfg.Enabled() {
		if cfg.MetricsAddr != "" && len(extra) > 0 {
			return nil, serveSafetyMetrics(ctx, cfg.MetricsAddr, extra, logger), nil
		}
		return nil, nil, nil
	}
	if pool == nil {
		return nil, nil, fmt.Errorf("audit export needs the platform database")
	}
	deps := auditexport.Deps{DB: pool, Logger: logger}
	if len(extra) > 0 {
		deps.ExtraMetrics = extra
	}
	service, err = auditexport.Build(ctx, cfg, deps)
	if err != nil {
		return nil, nil, err
	}
	go func() {
		// The security audit stream is relayed on every install (anchors and
		// lifecycle events are appended with the governed profile off), so its
		// backlog is always reported.
		if runErr := service.Run(ctx, true); runErr != nil && ctx.Err() == nil {
			logger.Error("audit export relay stopped", "err", runErr)
		}
	}()
	logger.Info("audit export started",
		"otlp", cfg.OTLPEndpoint != "", "webhook", cfg.WebhookURL != "",
		"audit_events", cfg.AuditEvents, "run_steps", cfg.RunSteps, "security_audit", true,
		"governed", admission.Governed())
	return service, service.Close, nil
}

// mainMetrics are the figures the main metrics listener serves after the
// audit export's own, in order.
type mainMetrics []auditexport.ExtraMetrics

// WriteTextContext writes each set in turn; the first error stops it.
func (m mainMetrics) WriteTextContext(ctx context.Context, w io.Writer) error {
	for _, metrics := range m {
		if err := metrics.WriteTextContext(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

// serveSafetyMetrics serves GET /metrics with the runtime safety and
// break-glass metrics alone, on an install with no audit exporter. closeFn
// stops it.
func serveSafetyMetrics(ctx context.Context, addr string, metrics mainMetrics, logger *slog.Logger) func(context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = metrics.WriteTextContext(r.Context(), w)
	}))
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics listener stopped", "addr", addr, "err", err)
		}
	}()
	logger.Info("metrics listener started (runtime safety, break-glass)", "addr", addr)
	return server.Shutdown
}

// securityAuditExporter is the exporter for the security audit relay: nil
// without an audit export, so the relay marks rows exported when stored.
func securityAuditExporter(service *auditexport.Service) securityaudit.Exporter {
	if service == nil {
		return nil
	}
	return service.SecurityExporter()
}
