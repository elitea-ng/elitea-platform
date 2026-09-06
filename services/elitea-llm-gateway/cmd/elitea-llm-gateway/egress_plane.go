package main

// egress_plane.go — the runtime half of the egress allowlist (gap G6).
//
// # The gap
//
// GATEWAY_EGRESS_ALLOWLIST was the only way to state the operator's egress
// policy. Empty meant two things at once: every public host is accepted, and no
// private host is reachable. A private on-premise model endpoint — a vLLM at
// http://192.168.29.60:8000/v1, say — therefore needed a chart edit and a pod
// restart, and there was no admin surface for it at all.
//
// The definitions now live in `gateway.governance_config` beside every other
// authored control, and this file is what makes an authored row take effect on
// a RUNNING gateway.
//
// # Two things have to happen, not one
//
//  1. The per-credential host check has to see the new entries. That is a
//     pointer bind: the account reads the live snapshot through the store on
//     every credential resolution, so a refresh reaches it with no further work.
//
//  2. bifrost's SSRF-safe dialer has to be rebuilt. AllowPrivateNetwork is read
//     by ConfigureDialer when a provider worker is CREATED, so a worker that
//     already exists keeps the decision it was born with. UpdateProvider re-reads
//     GetConfigForProvider and rebuilds the worker.
//
// Doing only the first would be the failure this whole effort is about: the
// admin saves a row, /governance/status shows it loaded, the host check passes,
// and the dial still fails — with no signal anywhere that says why.

import (
	"context"
	"log/slog"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
)

// egressWatchInterval is how often the merged private-network decision is
// re-read. It is deliberately shorter than the governance poll
// (policy.DefaultRefreshInterval, 30 s) so a change is picked up in the poll
// after the one that loaded it, rather than up to two polls later.
const egressWatchInterval = 5 * time.Second

// privateNetworkProviders are the classes whose per-key URL is tenant-authored
// and routinely private. They are the only ones GetConfigForProvider relaxes,
// so they are the only ones worth rebuilding.
var privateNetworkProviders = []schemas.ModelProvider{schemas.VLLM, schemas.Ollama}

// providerRefresher is the bifrost surface this file needs. It is declared here,
// at the consumer, so the watcher is testable without a live bifrost client.
type providerRefresher interface {
	UpdateProvider(provider schemas.ModelProvider) error
}

// egressPlane is the wiring for the runtime allowlist.
type egressPlane struct {
	// account is the vault-backed account. nil (no database pool) makes the
	// whole call a no-op: there are no credentials to govern.
	account *account.EliteaAccount
	// source is the compiled governance snapshot, read live.
	source account.EgressSource
	// core is the bifrost client whose provider workers latch
	// AllowPrivateNetwork. nil skips the rebuild half.
	core *bifrost.Bifrost
	// logger receives the mode transitions.
	logger *slog.Logger
	// interval overrides egressWatchInterval in tests.
	interval time.Duration
}

// startEgressAllowlistPlane binds the authored rows to the account and starts
// the watcher that rebuilds the affected provider workers.
//
// It is safe on every posture: a nil account, a nil source or a nil bifrost
// client each reduce the call to the part that still makes sense.
func startEgressAllowlistPlane(ctx context.Context, p egressPlane) {
	if p.account == nil || p.source == nil {
		return
	}
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}

	p.account.SetEgressSource(p.source)
	rep := p.account.EgressReport()
	logger.Info("EGRESS ALLOWLIST PLANE ARMED: authored egress_allowlist rows are unioned with the "+
		"GATEWAY_EGRESS_ALLOWLIST floor",
		"env_entries", len(rep.Env),
		"authored_entries", len(rep.DB),
		"effective_entries", len(rep.Effective),
		"private_network", rep.PrivateNetwork,
	)
	if !rep.PrivateNetwork {
		logger.Warn("PRIVATE-NETWORK EGRESS CLOSED: no allowlist entry names a private host or CIDR, so " +
			"bifrost's SSRF-safe dialer refuses RFC-1918 and loopback destinations for every provider. " +
			"Add the host AND its block (for example 192.168.29.60:8000 and 192.168.29.0/24) in " +
			"Admin > LLM Proxy > Governance to reach a self-hosted endpoint")
	}

	var refresher providerRefresher
	if p.core != nil {
		refresher = p.core
	}
	interval := p.interval
	if interval <= 0 {
		interval = egressWatchInterval
	}
	go watchEgressPrivateNetwork(ctx, p.account, refresher, interval, logger)
}

// watchEgressPrivateNetwork rebuilds the self-hosted provider workers when the
// merged private-network decision changes.
//
// It rebuilds on a CHANGE only. UpdateProvider drains and replaces a worker, so
// calling it every tick would recycle live provider connections for nothing.
func watchEgressPrivateNetwork(
	ctx context.Context,
	acct *account.EliteaAccount,
	refresher providerRefresher,
	interval time.Duration,
	logger *slog.Logger,
) {
	last := acct.EgressPrivateNetworkAllowed()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := acct.EgressPrivateNetworkAllowed()
			if now == last {
				continue
			}
			last = now
			logger.Info("EGRESS PRIVATE-NETWORK DECISION CHANGED: rebuilding the self-hosted provider workers "+
				"so bifrost's dialer picks the new setting up", "private_network", now)
			if refresher == nil {
				// Say it rather than let the log line above imply a rebuild
				// that did not happen.
				logger.Warn("no bifrost client is wired, so the dialer keeps its previous setting until restart")
				continue
			}
			for _, provider := range privateNetworkProviders {
				if err := refresher.UpdateProvider(provider); err != nil {
					// A provider with no worker yet has nothing to rebuild, and
					// it will read the new value when it is first created. Log
					// at debug rather than alarm an operator about the normal
					// case.
					logger.Debug("egress: provider worker not rebuilt",
						"provider", string(provider), "err", err)
				}
			}
		}
	}
}
