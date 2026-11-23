# NATS bootstrap: the Job's init container needs egress to the Kubernetes API server (#2911)

Two things the upgrade to the chart that creates the bootstrap Certificates in
the Job (`2026-11-22-2911-nats-bootstrap-certificates-created-by-the-job.md`,
7.9.0) can run into, found on a GitOps-managed install upgrading from 7.5.0.

## 1. API server egress

- **What changed:** the `elitea-nats-bootstrap` Job's `issue` init container
  calls the Kubernetes API server (`https://kubernetes.default.svc`) to create
  and read the four bootstrap Certificates and their Secrets. The old
  `elitea-nats-bootstrap-cleanup` Job made the same kind of calls, but from a
  pod with other labels. The new pod carries the bootstrap Job's labels:
  `app.kubernetes.io/name: nats-bootstrap` and
  `app.kubernetes.io/instance: <release>`.
- **Who is affected:** installs where an egress NetworkPolicy covers the
  namespace and does not admit the API server for that pod: a default-deny
  egress policy, or one that allows egress to everything but the private
  ranges. **A cluster with no egress NetworkPolicy needs nothing.** Neither
  does an install with `natsBootstrap.tls.certificate.create: false`, which
  runs no init container.
- **Symptom:** the Job stays in `Init:` or fails in its `issue` container, and
  `kubectl -n <namespace> logs job/elitea-nats-bootstrap -c issue` shows, for
  every attempt:

  ```
  curl: (7) Failed to connect to kubernetes.default.svc port 443
  nats-bootstrap-issue: FAILED: GET Certificate elitea-nats-bootstrap-main-nats-client answered HTTP 000
  ```

- **What to do:** allow egress from the bootstrap pod to the API server's
  endpoints. Find their addresses and port:

  ```sh
  kubectl get endpointslice -n default -l kubernetes.io/service-name=kubernetes
  ```

  The port is the endpoints' port (`6443` on many clusters, `443` on others):
  most CNIs apply the policy after the Service's `443` is translated to the
  endpoint. Either set the chart's opt-in policy:

  ```yaml
  natsBootstrap:
    networkPolicy:
      enabled: true
      apiServer:
        cidrs: ["10.0.0.2/32"]   # each endpoint address, as a /32
        ports: [443, 6443]       # the default
  ```

  It renders `elitea-nats-bootstrap-egress` (Argo CD wave `-11`, before the
  Job), which opens DNS, the bundled NATS server's `4222` and those endpoints
  for the bootstrap pod only. With `nats.enabled: false`, add a rule for your
  NATS server to `natsBootstrap.networkPolicy.extraEgress` (the chart refuses
  to render without one). Or apply your own policy, for example:

  ```yaml
  apiVersion: networking.k8s.io/v1
  kind: NetworkPolicy
  metadata:
    name: nats-bootstrap-apiserver-egress
    namespace: elitea
  spec:
    podSelector:
      matchLabels:
        app.kubernetes.io/name: nats-bootstrap
        app.kubernetes.io/instance: elitea
    policyTypes: [Egress]
    egress:
      - to:
          - ipBlock:
              cidr: 10.0.0.2/32   # each API server endpoint
        ports:
          - protocol: TCP
            port: 6443          # or 443, as the endpointslice shows
  ```

  NetworkPolicies add up, so this opens the API server for that pod alone,
  next to what your existing policies allow it (DNS, NATS). On Cilium the API
  server is the `kube-apiserver` entity, which an `ipBlock` may not match; use
  a `CiliumNetworkPolicy` with `toEntities: [kube-apiserver]` there.

## 2. A stale cleanup Job

- **What changed:** 7.6.0 to 7.8.x rendered an `elitea-nats-bootstrap-cleanup`
  Job. Its delete policy did not cover every outcome, so one from an earlier
  sync can be left in the namespace, and 7.9.0 no longer renders it, so
  nothing removes it.
- **Who is affected:** installs that ran a 7.6.0 to 7.8.x chart.
- **What to do:** delete it. Nothing reads it, and the new Job does its work:

  ```sh
  kubectl -n <namespace> delete job elitea-nats-bootstrap-cleanup
  ```
