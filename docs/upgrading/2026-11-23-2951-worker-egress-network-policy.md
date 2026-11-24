# Helm: the worker's egress NetworkPolicy admits only named peers and the public internet (#2951, ADR-0044)

- **What changed:** the worker's NetworkPolicy (`elitea-worker-netpol`) now
  restricts egress as well as ingress, whenever `networkPolicies.enabled` is
  true (the default). The worker pod may connect only to:
  - cluster DNS (pods labelled `k8s-app: kube-dns`, or
    `networkPolicies.dnsEgress`);
  - elitea-main's runtime listeners (9443-9446);
  - the bundled NATS server on 4222 (or `networkPolicies.egressPeers.nats`);
  - PostgreSQL, as `networkPolicies.egressPeers.postgresql` names it, and the
    bundled PgBouncer when it runs;
  - the sandbox supervisors, elitea-vector and the bundled OTel collector,
    while each runs;
  - `worker.networkPolicy.privateEgress`, and the CIDR and IP entries of
    `main.env.ELITEA_WORKER_EGRESS_PRIVATE_PERMIT`;
  - the public internet on `worker.networkPolicy.publicEgressPorts` (443),
    minus RFC 1918, loopback, link-local (the cloud metadata endpoints),
    carrier-grade NAT, IPv6 unique-local and link-local, and
    `networkPolicies.clusterCIDRs`;
  - `worker.networkPolicy.extraEgress`, verbatim.

  Everything else is refused, including the Kubernetes API server and every
  other in-cluster Service. `worker.networkPolicy.mode: proxy` replaces the
  public rule with the egress proxy. Two new values have no default and the
  chart lists them with the other missing values: `networkPolicies.clusterCIDRs`
  (the cluster's pod and Service CIDRs; empty, the egress proxy's
  `podCidrs` and `serviceCidrs` are used) and
  `networkPolicies.egressPeers.postgresql.to`. The LLM gateway's egress
  policy needs the same two (#2952).
- **Who is affected:** every install that runs the worker with
  `networkPolicies.enabled` on a CNI that enforces NetworkPolicy. The
  upgrade is refused until the two values are set. After it, toolkits,
  OpenAPI tools, index sources and remote MCP servers on a private network,
  in the cluster, or on a public host on a port other than 443 fail at the
  network until you open them. A cluster with NodeLocal DNSCache resolves
  nothing from the worker until `networkPolicies.dnsEgress` names it. A
  worker that dials a vector store, supervisor, NATS or OTLP collector the
  chart does not run needs a rule for it.
- **What to do, before you upgrade:**
  1. Set `networkPolicies.clusterCIDRs` to the cluster's pod and Service
     CIDRs (`kubectl cluster-info dump | grep -m1 cluster-cidr` and the
     API server's `--service-cluster-ip-range`; on GKE, the cluster's Pod and
     Service ranges).
  2. Set `networkPolicies.egressPeers.postgresql.to`, for example
     `[{ipBlock: {cidr: 10.20.0.5/32}}]` for a managed database, or a
     `namespaceSelector` with a `podSelector` for one in the cluster. Change
     `ports` if it does not listen on 5432.
  3. List each private host your toolkits reach in
     `worker.networkPolicy.privateEgress`, a CIDR and its ports each
     (`{cidr: 10.30.0.0/16, ports: [{protocol: TCP, port: 443}]}`). The
     **Worker egress hosts** panel (Configuration, Worker Egress) lists the
     hosts runs reached. CIDR and IP entries of
     `ELITEA_WORKER_EGRESS_PRIVATE_PERMIT` need no entry; a host name entry
     there, and every entry of the console's **Private Permit**, does. The
     chart refuses a host name in the permit with no `privateEgress` or
     `extraEgress` at all.
  4. Add other public ports to `worker.networkPolicy.publicEgressPorts`, and
     raw rules for anything else to `worker.networkPolicy.extraEgress`. With
     NodeLocal DNSCache, set `networkPolicies.dnsEgress`, for example
     `[{to: [{ipBlock: {cidr: 169.254.20.10/32}}], ports: [{protocol: UDP, port: 53}, {protocol: TCP, port: 53}]}]`.
  5. If your own policy already restricts the worker's egress, set
     `worker.networkPolicy.egressEnabled: false` with
     `worker.networkPolicy.externallyManaged: true`; the chart refuses the
     first without the second.

  After the upgrade, a run that fails with a connection timeout (not
  `egress_host_not_allowed`) names a destination the policy does not admit.
