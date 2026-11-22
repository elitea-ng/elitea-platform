# NATS bootstrap: the Job creates its client Certificates, no hook Certificates (#2911)

On a GitOps-managed install, a sync could fail on one of the four bootstrap
Certificates (`elitea-nats-bootstrap-<account>-nats-client`) with the message
`Issuing certificate as Secret does not exist`, although cert-manager issued it
a second later. Argo CD gates a hook on its health, and its Certificate health
check reads the `Ready=False` that a new Certificate can carry for a moment,
before cert-manager sets `Issuing=True`, as `Degraded`. A retry then failed on
`elitea-nats-bootstrap-cleanup`: it deleted a Secret that the previous
attempt's Certificate wrote straight back.

- **Now:** the chart no longer renders the bootstrap Certificates, and the
  `elitea-nats-bootstrap-cleanup` Job is gone. The `elitea-nats-bootstrap`
  Job has an init container, `issue`, that creates the four Certificates,
  owned by the Job, and waits until each is `Ready`. A transient
  `Ready=False` no longer fails anything. It then hands the certificates to
  the bootstrap container in memory, and the bootstrap mounts no Secret.
  Before it creates a Certificate, it deletes the one an earlier run left
  behind and waits until it is gone, and only then deletes the old Secret
  (#1370). The garbage collector deletes the Certificates with the Job, so
  they are still short-lived and nothing renews them.
- **New objects:** a hook ServiceAccount, Role and RoleBinding named
  `elitea-nats-bootstrap-issuer` (Helm weight `-30`, Argo CD wave `-11`),
  which replace the cleanup's. The Role gets and deletes only the four
  bootstrap Certificates and their Secrets, by name, and may create
  Certificates: Kubernetes RBAC cannot limit `create` by name. The pod
  automounts no token. Only the init container mounts a projected token,
  which expires in 10 minutes.
- **New values:**
  - `natsBootstrap.tls.certificate.issueWait` (default `120`) is how long, in
    seconds, the init container waits for the Certificates to become Ready.
    `connectWait` plus `issueWait` must leave a minute of
    `activeDeadlineSeconds`. The default 300 + 120 fits the default 600.
  - `natsBootstrap.tls.certificate.admissionPolicy` (default `false`;
    `true` or `"auto"`) renders a ValidatingAdmissionPolicy (Kubernetes
    1.30+). For the issuer ServiceAccount alone, it admits only the four
    bootstrap Certificates exactly as rendered. It is cluster-scoped, so turn
    it on only where the deployer may create cluster-scoped objects.
- **Now refused at render:**
  - `natsBootstrap.tls.secretNames` together with `certificate.create: true`.
    The Job would delete those Secrets and issue into them on every run, and
    the old cleanup Job already did the first. Set `certificate.create: false`
    to use your own Secrets.
  - `natsBootstrap.tls.mountPath: /` with `certificate.create: true`.
- **Who is affected:** installs with `natsBootstrap.enabled` and
  `natsBootstrap.tls.certificate.create` (both default `true`). With
  `certificate.create: false` the Job mounts your Secrets as before, with no
  init container.
- **What to do:** nothing on Helm. On Argo CD, sync as usual. Certificates
  that a failed sync left behind are deleted by the init container on the
  first run. If you allowlist the objects a sync may create (an AppProject's
  `namespaceResourceWhitelist`), the chart's ServiceAccount, Role and
  RoleBinding are the same kinds as before. If an admission policy limits
  who may create cert-manager `Certificate`s in the namespace, allow the
  `elitea-nats-bootstrap-issuer` ServiceAccount.
- **Troubleshooting:** `kubectl -n <namespace> logs job/elitea-nats-bootstrap -c issue`
  names each object, the HTTP status and the API's reason, or, after
  `issueWait`, the conditions of a Certificate that did not become Ready.
