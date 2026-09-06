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
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/EliteaAI/elitea-platform/libs/go/observability"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/auditretention"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/budgetwriteback"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/config"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/health"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/pricesync"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/rpc"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/scheduler"
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

	// Redis
	rdb := goredis.NewClient(&goredis.Options{Addr: cfg.RedisURL})
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Error("redis unreachable", "err", err)
		os.Exit(1)
	}
	defer func() { _ = rdb.Close() }()

	rpcClient := rpc.New(rdb, cfg.RPCChannel, cfg.RPCHMACKey)
	sched := scheduler.New(pool, rdb, rpcClient, cfg)

	// Health server
	mux := http.NewServeMux()
	mux.Handle("/healthz", health.New(pool))

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

	slog.Info("starting scheduler", "instance", cfg.InstanceID, "rpc_channel", cfg.RPCChannel)
	go sched.Run(ctx)

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
	// sched.MaintenanceActive is the SAME gate the dispatch tick consults, not
	// a second reading of the switch — see internal/scheduler/maintenance.go.
	auditSweeper, auditErr := auditretention.New(pool, sched.MaintenanceActive, auditretention.Config{
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
		// whole daemon down: the schedule poller, price sync and budget
		// write-back are unrelated to it and an operator needs them running
		// while they correct the value.
		slog.Error("audit retention sweep did not start; centry.audit_events grows without bound",
			"err", auditErr, "audit_retention_days", cfg.AuditRetentionDays)
	default:
		slog.Info("starting audit retention sweeper",
			"window_days", cfg.AuditRetentionDays, "interval", cfg.AuditRetentionInterval)
		go auditSweeper.Run(ctx)
	}

	// Budget write-back consumer (design §8.6): durable pull consumer draining
	// GATEWAY_BUDGET_DELTAS into gateway.llm_budget_accumulators. Disabled unless
	// both the flag and a NATS URL are set, so environments without NATS are
	// unaffected. A NATS blip at boot is non-fatal (the scheduler keeps running
	// its other jobs); the consumer resumes when NATS recovers on next restart.
	var natsConn *nats.Conn
	if cfg.BudgetWriteBackEnabled && cfg.BudgetWriteBackNATSURL != "" {
		nc, err := nats.Connect(cfg.BudgetWriteBackNATSURL,
			nats.Name("elitea-scheduler-budget-writeback"),
			nats.Timeout(time.Second),
			nats.MaxReconnects(-1),
			nats.ReconnectWait(500*time.Millisecond),
		)
		if err != nil {
			slog.Warn("budget write-back: NATS connect failed; consumer disabled", "err", err)
		} else if js, jerr := jetstream.New(nc); jerr != nil {
			slog.Warn("budget write-back: JetStream init failed; consumer disabled", "err", jerr)
			nc.Close()
		} else {
			wbCfg := budgetwriteback.Config{
				BatchSize:  cfg.BudgetWriteBackBatchSize,
				AckWait:    cfg.BudgetWriteBackAckWait,
				MaxDeliver: cfg.BudgetWriteBackMaxDeliver,
			}
			bindCtx, bc := context.WithTimeout(ctx, 5*time.Second)
			consumer, berr := budgetwriteback.Bind(bindCtx, js, budgetwriteback.NewPoolDB(pool), wbCfg, logger)
			bc()
			if berr != nil {
				slog.Warn("budget write-back: bind consumer failed; consumer disabled", "err", berr)
				nc.Close()
			} else {
				natsConn = nc
				slog.Info("starting budget write-back consumer", "batch", wbCfg.BatchSize)
				go consumer.Run(ctx)
			}
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
	sched.Stop()
}
