package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/EliteaAI/elitea-platform/libs/go/observability"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/auditretention"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/authstateretention"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/budgetwriteback"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/config"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/health"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/maintenance"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/nativeauthretention"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/pricesync"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/syncretention"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := config.FromEnv()

	// Observability (issue #250): same collector elitea-main's tracing ingest
	// routes and the OTEL_EXPORTER_OTLP_ENDPOINT-configured collector service
	// receive. Disabled deployments (OTEL_SDK_DISABLED=true) get a no-op
	// provider with zero behavior change.
	obsProvider, err := observability.New(ctx, observability.ConfigFromEnv("elitea-scheduler", ""))
	if err != nil {
		slog.Error("failed to initialize observability", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := obsProvider.Shutdown(shutdownCtx); err != nil {
			slog.Error("failed to shut down observability", "err", err)
		}
	}()

	// Database
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to create db pool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		slog.Error("database unreachable", "err", err)
		os.Exit(1)
	}

	// The maintenance switch the retention sweepers share. The legacy
	// centry.schedule → `elitea_rpc` Redis dispatcher that used to live here
	// was deleted: nothing in the Go stack consumed that channel (issue #305),
	// and with it went this daemon's only use of Redis.
	maintenanceSwitch := maintenance.New(pool)

	// Health server
	mux := http.NewServeMux()
	mux.Handle("/healthz", health.New(pool))
	// The budget write-back consumer's state (attached / waiting / disabled /
	// misconfigured). Not the pod's probe: a NATS outage must not restart a
	// scheduler whose other jobs are fine; it is what an operator reads to
	// tell "draining" from "silently not draining".
	writeBackStatus := budgetwriteback.NewStatus()
	mux.Handle("/readyz/budget-writeback", writeBackStatus)

	srv := &http.Server{
		Addr:        cfg.HTTPAddr,
		Handler:     otelhttp.NewHandler(mux, "elitea-scheduler"),
		ReadTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("starting health server", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("health server error", "err", err)
		}
	}()

	// Price-catalog sync worker (design §8.8): refreshes gateway.gateway_models
	// from ordered PriceSources on a ~24h cadence, off the /llm hot path.
	if cfg.PriceSyncEnabled {
		var sources []pricesync.PriceSource
		if cfg.PriceSyncLiteLLM {
			sources = append(sources, pricesync.NewLiteLLMSource(cfg.PriceSyncURL, nil))
		}
		if cfg.PriceSyncSeed {
			sources = append(sources, pricesync.NewSeedSource())
		}
		if len(sources) == 0 {
			slog.Warn("price-sync enabled but no sources configured; skipping worker")
		} else {
			syncer := pricesync.NewSyncer(pricesync.NewPoolDB(pool), sources, logger)
			worker := pricesync.NewWorker(syncer, cfg.PriceSyncInterval, logger)
			slog.Info("starting price-sync worker", "interval", cfg.PriceSyncInterval, "sources", len(sources))
			go worker.Run(ctx)
		}
	}

	// Audit-event retention sweep (issue #619): bounded batched DELETEs that
	// keep centry.audit_events from growing for the life of the deployment.
	//
	// Every outcome of the construction is stated in the log, and that is the
	// point of the shape below. A sweeper that failed to start silently would
	// leave the table unbounded and look exactly like one that is running with
	// nothing to remove.
	//
	// maintenanceSwitch.Active is the SAME gate the sync sweep consults, not
	// a second reading of the switch — see internal/maintenance.
	auditSweeper, auditErr := auditretention.New(pool, maintenanceSwitch.Active, auditretention.Config{
		RetentionDays:     cfg.AuditRetentionDays,
		Interval:          cfg.AuditRetentionInterval,
		BatchSize:         cfg.AuditRetentionBatchSize,
		MaxBatchesPerPass: cfg.AuditRetentionMaxBatches,
	}, logger)
	switch {
	case errors.Is(auditErr, auditretention.ErrDisabled):
		slog.Warn("audit retention sweep is OFF (AUDIT_RETENTION_DAYS<=0); "+
			"centry.audit_events grows without bound on this deployment",
			"audit_retention_days", cfg.AuditRetentionDays)
	case auditErr != nil:
		// A refused window is a configuration mistake, not a reason to take the
		// whole daemon down: price sync, budget write-back and the other
		// sweepers are unrelated to it and an operator needs them running
		// while they correct the value.
		slog.Error("audit retention sweep did not start; centry.audit_events grows without bound",
			"err", auditErr, "audit_retention_days", cfg.AuditRetentionDays)
	default:
		slog.Info("starting audit retention sweeper",
			"window_days", cfg.AuditRetentionDays, "interval", cfg.AuditRetentionInterval)
		go auditSweeper.Run(ctx)
	}

	// Incremental-sync tombstone retention (ADR-0025 WP6, elitea-main tenant
	// 0144 and shared 0144): bounded batched deletes of tombstones older than
	// the window elitea-main still serves a `changes_since` cursor for. The
	// window can only be raised above that floor. Gated on maintenance like
	// the audit sweep, since it writes to every tenant schema.
	if syncSweeper, raised, syncErr := syncretention.New(pool, maintenanceSwitch.Active, syncretention.Config{
		RetentionDays: cfg.SyncTombstoneRetentionDays,
	}, logger); syncErr != nil {
		slog.Error("sync tombstone retention sweep did not start; tombstone tables grow without bound", "err", syncErr)
	} else {
		if raised {
			slog.Warn("SYNC_TOMBSTONE_RETENTION_DAYS is below the cursor window elitea-main serves; raised to the floor",
				"configured_days", cfg.SyncTombstoneRetentionDays, "floor_days", syncretention.MinimumRetentionDays)
		}
		go syncSweeper.Run(ctx)
	}

	// Native authorization retention (ADR-0025 WP2, elitea-main shared 0141):
	// bounded batched deletes of expired access tokens and authorization
	// requests, consumed refresh tokens past their family's idle TTL, idle
	// families (revoked as `expired`, anchors removed) and families revoked
	// more than 90 days ago. Correctness never depends on it: elitea-main
	// checks every expiry at read time. Not gated on maintenance mode: it
	// only removes credentials that can no longer be used.
	if nativeSweeper, nativeErr := nativeauthretention.New(pool, nativeauthretention.Config{}, logger); nativeErr != nil {
		slog.Error("native auth retention sweep did not start", "err", nativeErr)
	} else {
		go nativeSweeper.Run(ctx)
	}

	// Browser sign-in state retention (elitea-main shared 0153 and 0117):
	// bounded batched deletes of expired Form sessions, Form login
	// transactions and attempt windows, and of expired OIDC/SAML browser
	// sessions, whose store had a DeleteExpired that nothing called.
	// Correctness never depends on it: elitea-main filters on expiry at read
	// time. Gated on maintenance like the audit and sync sweeps.
	if authStateSweeper, authStateErr := authstateretention.New(
		pool, maintenanceSwitch.Active, authstateretention.Config{}, logger,
	); authStateErr != nil {
		slog.Error("auth state retention sweep did not start; the sign-in state tables grow without bound",
			"err", authStateErr)
	} else {
		go authStateSweeper.Run(ctx)
	}

	// Budget write-back consumer (design §8.6): drains GATEWAY_BUDGET_DELTAS
	// into gateway.llm_budget_accumulators through the durable pull consumer
	// the nats-bootstrap Job creates. Disabled unless both the flag and a NATS
	// URL are set. A NATS that is down, or a consumer the bootstrap has not
	// created yet, is NOT a reason to give up: the Supervisor keeps
	// attaching with backoff (one WARN per outage) and re-attaches when the
	// consumer is lost, and /readyz/budget-writeback reports the state. A
	// refused configuration is the one permanent failure.
	var natsConn *budgetwriteback.Connector
	if cfg.BudgetWriteBackEnabled && cfg.BudgetWriteBackNATSURL != "" {
		dialCfg := budgetwriteback.DialConfig{
			URL:         cfg.BudgetWriteBackNATSURL,
			TLSCAFile:   cfg.BudgetWriteBackNATSTLSCAFile,
			TLSCertFile: cfg.BudgetWriteBackNATSTLSCertFile,
			TLSKeyFile:  cfg.BudgetWriteBackNATSTLSKeyFile,
		}
		if err := budgetwriteback.CheckDialConfig(dialCfg); err != nil {
			slog.Error("budget write-back: the NATS settings are refused; consumer disabled", "err", err)
			writeBackStatus.SetMisconfigured(err)
		} else {
			natsConn = &budgetwriteback.Connector{Config: dialCfg, Logger: logger}
			wbCfg := budgetwriteback.Config{BatchSize: cfg.BudgetWriteBackBatchSize}
			db := budgetwriteback.NewPoolDB(pool)
			supervisor := &budgetwriteback.Supervisor{
				Bind: func(ctx context.Context) (*budgetwriteback.Consumer, error) {
					js, err := natsConn.JetStream()
					if err != nil {
						return nil, err
					}
					return budgetwriteback.Bind(ctx, js, db, wbCfg, logger)
				},
				Status: writeBackStatus,
				Logger: logger,
			}
			slog.Info("starting budget write-back consumer", "batch", wbCfg.BatchSize)
			go supervisor.Run(ctx)
		}
	}

	<-ctx.Done()
	slog.Info("shutting down")

	if natsConn != nil {
		natsConn.Close()
	}

	shutCtx, sc := context.WithTimeout(context.Background(), 10*time.Second)
	defer sc()
	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Error("health server shutdown error", "err", err)
	}
}
