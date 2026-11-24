# Helm: no service-account token in pods that do not call the Kubernetes API (#2953)

- **What changed:** every pod the chart renders sets
  `automountServiceAccountToken: false`, and so does every ServiceAccount it
  creates for them, except the workloads that call the Kubernetes API: the
  sandbox supervisor and the KEDA uninstall hook (both set `true`), and the
  NATS bootstrap Job, whose `issue` container mounts a projected token of its
  own. The worker gets its own ServiceAccount, `elitea-worker`
  (`worker.serviceAccount.create`, default true); before it ran as the
  namespace's `default` account. A new value per component,
  `<component>.serviceAccount.automountToken` (default `false`), mounts the
  token again for `main`, `web`, `scheduler`, `llmGateway`, `deepwiki`,
  `inventory`, `otelCollector` and `worker`.
- **Who is affected:** an install whose mesh, sidecar or agent reads the
  automounted token at `/var/run/secrets/kubernetes.io/serviceaccount` in
  one of these pods. GKE Workload Identity, EKS Pod Identity, IRSA and Azure
  Workload Identity are not affected: they use their own projected tokens or
  the metadata server. An install that granted the namespace's `default`
  ServiceAccount something the worker relied on (an image pull secret
  attached to `default`, a Workload Identity annotation) must give it to
  `elitea-worker`, or set `worker.serviceAccount.create: false` with
  `worker.serviceAccount.name: default`.
- **What to do:** nothing for the shipped install. Where a sidecar needs the
  token, set `<component>.serviceAccount.automountToken: true` for that
  component only. Move any `imagePullSecrets` or annotations you put on
  `default` for the worker to `worker.imagePullSecrets` or
  `worker.serviceAccount.annotations`.
