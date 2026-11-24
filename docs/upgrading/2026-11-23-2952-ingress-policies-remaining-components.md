# Helm: ingress NetworkPolicies for the scheduler, LLM gateway, OTel collector and PgBouncer; gateway egress policy on by default (#2952, ADR-0036)

- **What changed:** with `networkPolicies.enabled` (the default) the chart
  renders four more default-deny ingress policies:
  - `elitea-scheduler-netpol`: the health port from
    `networkPolicies.probeFrom` and `networkPolicies.metricsFrom` only;
  - `elitea-llm-gateway-netpol`: the gRPC port from elitea-main and
    `networkPolicies.llmGateway.extraIngressFrom`; 8083 (health and
    metrics) from `probeFrom` and `metricsFrom`;
  - `elitea-otel-collector-netpol`: OTLP 4317/4318 from the chart's pods that
    export traces (elitea-main, the scheduler, the gateway, the worker,
    DeepWiki, Inventory, elitea-vector) and
    `networkPolicies.otelCollector.extraIngressFrom`;
  - `elitea-pgbouncer-netpol`: the pooler's port from the chart's pods that
    read the database Secret (elitea-main and its migration Jobs, the
    scheduler, the gateway, the worker and its workload-session Jobs,
    DeepWiki, Inventory and their migration Jobs, the dbInit Job) and
    `networkPolicies.pgbouncer.extraIngressFrom`.

  The gateway's egress policy (`elitea-llm-gateway-egress`) is now on by
  default (`llmGateway.networkPolicy.enabled: true`) and renders only with
  `networkPolicies.enabled`. It admits DNS, the bundled NATS (or
  `networkPolicies.egressPeers.nats`), PostgreSQL
  (`networkPolicies.egressPeers.postgresql`) and the bundled PgBouncer, the
  bundled OTel collector, `llmGateway.networkPolicy.egress`, and the public
  internet on 443 minus every private, link-local and cluster range (now IPv6
  too). It needs `networkPolicies.clusterCIDRs` and
  `networkPolicies.egressPeers.postgresql.to`, like the worker's (#2951).
  Turning it off needs `llmGateway.networkPolicy.externallyManaged: true`.
- **Who is affected:** installs with `networkPolicies.enabled` on a CNI that
  enforces NetworkPolicy:
  - a Prometheus that scrapes the scheduler or the gateway's 8083 (a
    PodMonitor, or the gateway's autoscaler on
    `gateway_llm_sse_active_connections`) stops getting samples until it is
    in `networkPolicies.metricsFrom`; the autoscaler then reads zero;
  - a gateway caller you added to `GATEWAY_GRPC_CLIENTS`, an OTLP sender of
    your own, or a PgBouncer client outside the chart (a KEDA `postgresql`
    trigger pointed at the pooler, a backup job) is refused until it is in
    the matching `extraIngressFrom`;
  - a gateway that reaches a self-hosted model on a private address or in
    the cluster, an OTLP collector named by `global.otelEndpoint`, or a
    public provider on a port other than 443, fails until that destination
    is in `llmGateway.networkPolicy.egress` or `publicEgressPorts`;
  - EKS Pod Identity serves credentials at the link-local
    `169.254.170.23:80`, which the public rule excepts: add a rule for it to
    `llmGateway.networkPolicy.egress` if the gateway uses Pod Identity;
  - an install that set `llmGateway.networkPolicy.enabled: true` before
    keeps its `egress` entries; its PostgreSQL and NATS entries can move to
    `networkPolicies.egressPeers`.
- **What to do, before you upgrade:** set `networkPolicies.clusterCIDRs` and
  `networkPolicies.egressPeers.postgresql.to` (see the #2951 note), list
  your Prometheus in `networkPolicies.metricsFrom`, add any extra gateway,
  collector or pooler client to the matching
  `networkPolicies.<component>.extraIngressFrom`, and every private model
  host to `llmGateway.networkPolicy.egress`. If your own policy already
  restricts the gateway's egress, set `llmGateway.networkPolicy.enabled:
  false` with `llmGateway.networkPolicy.externallyManaged: true`.
