// Command elitea-egress-proxy is the egress boundary for cloud-executor
// sandboxes whose project restricts network access to a host allowlist
// (ADR-0034 decision 9). See the service README for the threat model, the
// limits and the operating notes.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-egress-proxy/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-egress-proxy/internal/config"
	"github.com/EliteaAI/elitea-platform/services/elitea-egress-proxy/internal/proxy"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfg, cfgErr := config.FromEnv(os.Getenv)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	if cfgErr != nil {
		logger.Error("egress_proxy.config_invalid", "error", cfgErr.Error())
		return 1
	}

	metrics := proxy.NewMetrics()
	keys, err := proxy.LoadFileKeys(cfg.PublicKeys, logger, metrics)
	if err != nil {
		logger.Error("egress_proxy.keys_invalid", "error", err.Error())
		return 1
	}
	metrics.Gauge("egress_proxy_token_keys", "Run-token public keys in the current key set.",
		func() int64 { return int64(len(keys.Current().KeyIDs())) })

	srv, err := proxy.New(proxy.Options{
		Keys:                  keys,
		DenyCIDRs:             cfg.DenyCIDRs,
		MaxConnections:        cfg.MaxConnections,
		MaxHeaderBytes:        cfg.MaxHeaderBytes,
		ReadHeaderTimeout:     cfg.ReadHeaderTimeout,
		DialTimeout:           cfg.DialTimeout,
		RunMaxConnections:     cfg.RunMaxConnections,
		RunMaxBytes:           cfg.RunMaxBytes,
		IdleTimeout:           cfg.IdleTimeout,
		MaxConnectionLifetime: cfg.MaxConnectionLifetime,
		Logger:                logger,
		Metrics:               metrics,
	})
	if err != nil {
		logger.Error("egress_proxy.config_invalid", "error", err.Error())
		return 1
	}

	// The per-turn audit (ADR-0034 decision 9): every run summary also goes to
	// elitea-main's EgressAuditService over mTLS.
	var reporter *audit.Reporter
	if cfg.Audit.Target != "" {
		reporter, err = audit.Dial(cfg.Audit, logger)
		if err != nil {
			logger.Error("egress_proxy.config_invalid", "error", err.Error())
			return 1
		}
		srv.Runs().SetSink(reporter.Submit)
		metrics.Gauge("egress_proxy_audit_reports_delivered", "Run summaries delivered to elitea-main since start.", reporter.Delivered)
		metrics.Gauge("egress_proxy_audit_reports_dropped", "Run summaries that could not be delivered to elitea-main since start (they stay in this log).", reporter.Dropped)
		reporter.Start()
		logger.Info("egress_proxy.audit_enabled", "target", cfg.Audit.Target)
	} else {
		logger.Warn("egress_proxy.audit_disabled", "reason", "no ELITEA_EGRESS_PROXY_AUDIT_TARGET: run summaries stay in this log only")
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		logger.Error("egress_proxy.listen_failed", "addr", cfg.ListenAddr, "error", err.Error())
		return 1
	}
	admin := &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           proxy.AdminHandler(func() bool { return !srv.Draining() && keys.Current() != nil }, metrics),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	loops, stopLoops := context.WithCancel(context.Background())
	defer stopLoops()
	go keys.Run(loops, cfg.KeysReloadInterval)
	go srv.Runs().Run(loops, cfg.SummaryInterval, time.Now)

	serveErr := make(chan error, 2)
	go func() { serveErr <- srv.Serve(ln) }()
	go func() {
		if err := admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	logger.Info("egress_proxy.started", "listen_addr", cfg.ListenAddr, "admin_addr", cfg.AdminAddr,
		"key_ids", keys.Current().KeyIDs(), "deny_cidrs", len(cfg.DenyCIDRs),
		"run_max_connections", cfg.RunMaxConnections, "run_max_bytes", cfg.RunMaxBytes,
		"idle_timeout", cfg.IdleTimeout.String(), "max_connection_lifetime", cfg.MaxConnectionLifetime.String())

	code := 0
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil {
			logger.Error("egress_proxy.serve_failed", "error", err.Error())
			code = 1
		}
	}

	// Drain: fail readiness first, so the Service removes this endpoint, then
	// stop accepting and wait for open connections.
	srv.SetDraining()
	logger.Info("egress_proxy.draining", "drain_delay", cfg.DrainDelay.String(), "shutdown_timeout", cfg.ShutdownTimeout.String())
	time.Sleep(cfg.DrainDelay)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("egress_proxy.shutdown_forced", "error", err.Error())
	}
	stopLoops()
	srv.Runs().Sweep(time.Now(), cfg.SummaryInterval, true)
	if reporter != nil {
		auditCtx, auditCancel := context.WithTimeout(context.Background(), 10*time.Second)
		reporter.Close(auditCtx)
		auditCancel()
	}
	adminCtx, adminCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer adminCancel()
	_ = admin.Shutdown(adminCtx)
	logger.Info("egress_proxy.stopped")
	return code
}
