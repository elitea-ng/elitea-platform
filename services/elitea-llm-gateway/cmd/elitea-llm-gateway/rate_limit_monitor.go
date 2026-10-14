package main

// rate_limit_monitor.go — per-minute rate limits recover when NATS comes up
// late, and their health is observed by a dedicated probe.
//
// The limiter used to be bound once at startup, so a gateway that booted while
// NATS was down never enforced authored tokens_per_min / requests_per_min
// ceilings until restarted. This loop is the SINGLE WRITER of the limiter's
// state: it binds the counter when NATS becomes reachable (independently of the
// budget engine, which has its own loop and may be installed by another path)
// and then probes the counter every interval with its own context. Request
// outcomes never set the state, so a client cancelling mid-call cannot make the
// gateway report degraded, and a NATS drop with no traffic is still noticed.
//
// Policy is unchanged: while the counter is unavailable the limiter FAILS OPEN
// (see policy.Limiter) — a rate limit protects against overload, and the budget
// gate is the spend control with its own fail-mode FSM.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/policy"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/server"
)

// rateLimitProbeInterval is how often the monitor re-dials (while unbound) or
// probes (while bound). A constant, like budgetRecoveryInterval.
const rateLimitProbeInterval = 15 * time.Second

// natsRedialer is the slice of *server.Server the monitor needs.
type natsRedialer interface {
	RedialNATS(ctx context.Context) (server.NATSClient, error)
}

type rateLimitMonitor struct {
	srv      natsRedialer
	limiter  *policy.Limiter
	logger   *slog.Logger
	interval time.Duration
}

// startRateLimitMonitor starts the loop only when there is something to do: a
// limiter exists and NATS is configured.
func startRateLimitMonitor(ctx context.Context, m rateLimitMonitor, natsConfigured bool) {
	if m.limiter == nil || !natsConfigured {
		return
	}
	go m.run(ctx)
}

func (m rateLimitMonitor) run(ctx context.Context) {
	if m.step(ctx) {
		return
	}
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if m.step(ctx) {
				return
			}
		}
	}
}

// step performs one bind-or-probe. It reports whether the loop should stop.
func (m rateLimitMonitor) step(ctx context.Context) (stop bool) {
	before := m.limiter.Status()
	if !m.limiter.Enabled() {
		nc, err := m.srv.RedialNATS(ctx)
		if err != nil {
			if errors.Is(err, server.ErrNATSNotConfigured) || errors.Is(err, server.ErrServerClosed) {
				return true
			}
			if ctx.Err() == nil {
				m.logger.Warn("rate limits: NATS still unreachable; per-minute limits stay disabled (fail-open) and are retried",
					"err", err, "retry_in", m.interval)
			}
			return false
		}
		rc, ok := nc.(policy.RateCounter)
		if !ok {
			m.logger.Error("rate limits: the NATS client has no rate-limit counter; limits cannot be enforced")
			return true
		}
		m.limiter.Bind(rc)
	} else {
		m.limiter.Probe(ctx)
	}
	after := m.limiter.Status()
	if after != before {
		switch after {
		case policy.RateStatusEnforced:
			m.logger.Info("GOVERNANCE RATE LIMITS ENFORCED: the NATS counter is reachable; per-minute limits are in force", "was", string(before))
		default:
			m.logger.Warn("GOVERNANCE RATE LIMITS DEGRADED: the NATS counter probe failed; requests are admitted without their per-minute ceiling (fail-open) until it recovers", "status", string(after))
		}
	}
	return false
}
