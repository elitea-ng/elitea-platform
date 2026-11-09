---
title: "DeepWiki, Inventory and the runtime on by default; one switch per feature; Qdrant is the upstream chart — BREAKING"
issue: 1390
date: 2026-10-22
order: 24
---

**Affects:** every Kubernetes install of the `elitea` chart. Installs that
relied on a feature being OFF by default, installs that ran the chart's own
Qdrant (`vector.qdrant.mode: internal`), and AppProjects that restrict
`sourceRepos` for a git source.

**What changed.**

* **On by default:** the runtime plane (`main.runtime.enabled`, with agent
  execution, index ingest and toolkit discovery on its default streams), the
  agent worker (`worker.enabled`), elitea-vector (`vector.enabled`) with a
  bundled one-node Qdrant (`qdrant.enabled`), DeepWiki (`deepwiki.enabled`)
  and Inventory (`inventory.enabled`). Other default changes:
  `postgresql.maxConnections` 100 → 500 (elitea-main's default ceiling of
  10 replicas at about 44 connections each; set it to your server's
  `max_connections`, and lower `main.autoscaling.maxReplicas` to fit, as
  `values-gke.yaml` does with 3), `vector.collection.replicationFactor` 2 → 1,
  `main.runtime.signingKeyId` `standalone-runtime-v1` (the default key id of
  `deploy/scripts/gen-runtime-certs.sh`). New, with the old behaviour as
  the default: `main.rollingUpdate` and `worker.rollingUpdate` (`maxSurge:
  1, maxUnavailable: 0`); `values-gke.yaml` sets both to `maxSurge: 0,
  maxUnavailable: 1`, so no rollout exceeds its 4 vCPU / 10 GiB budget.
* **One switch per feature.** The chart derives elitea-main's side from the
  provider's block: `ELITEA_DEEPWIKI_ENABLED` / `ELITEA_INVENTORY_ENABLED`, the
  git allowlists (copied from `deepwiki.env` / `inventory.env`), the client
  certificate paths and their mounts, the platform gRPC targets and the
  `ELITEA_VECTOR_*` settings. An explicit `main.env` value that agrees is kept;
  one that disagrees fails the render and names the switch to set instead.
  `main.fileConfig.deepwikiClientMaterial.enabled` and
  `inventoryClientMaterial.enabled` now default to `null` (follow the
  provider); an explicit `false` beside an enabled provider is refused.
  `main.runtime.agentExecutionDispatch` / `indexIngestDispatch` settings with
  `enabled: false` are no longer refused: `enabled` is the switch.
* **No hand-minted certificates.** With cert-manager, the DeepWiki and
  Inventory callback client certificates (`elitea-deepwiki-callback-client`,
  `elitea-inventory-callback-client`, one DNS SAN `elitea-subapp-callback`)
  and Qdrant's TLS certificate (`elitea-qdrant-tls`) are issued by the chart
  from the runtime-CA issuer (`elitea-runtime-ca`, kind `ClusterIssuer` by
  default; the same issuer as `vector.mtls.issuerRef`). To bring your own
  callback certificate, set `<app>.callbackClient.certManager.enabled: false`.
* **Fixed Secret names.** Every Secret the chart reads has a default name, and
  `externalSecrets.enabled` renders an ExternalSecret for each:

  | Secret | Keys |
  |---|---|
  | `elitea-main-db` | `database-url` |
  | `elitea-runtime-material` | the nine runtime files (`runtime-ca.crt`, `command-signing-key.pem`, `command-signing-keyring.json`, the control/output/content keypairs) |
  | `elitea-worker-material` | `runtime-ca.crt`, `agent-worker-client.crt/.key`, `command-signing-keyring.json`, `worker-output-spool-key`, `agent-checkpoint-connection` |
  | `elitea-main-auth-material` | `auth-attempt-key`, `auth-pat-signing-key`, `auth-form-users.json` |
  | `elitea-main-llm-gateway-secrets` | `gateway-identity-secret` |
  | `elitea-llm-gateway-secrets` | `identity-secret`, `secrets-master-key` |
  | `elitea-qdrant-api` | `api-key` (was `vector.qdrant.apiKeySecret.name`, no default) |
  | `elitea-deepwiki-secrets` | `identity-secret` |
  | `elitea-inventory-secrets` | `identity-secret` |

* **Missing values fail the render with ONE message.** A values file that
  lacks something only you know (the authentication document and its
  `public_origin`, `platform.aiProjectId`, `DEPLOYMENT_URL`,
  `worker.runtimeSession.expectedIdentity`, the two git allowlists,
  `networkPolicies.main.ingressFrom`, `GATEWAY_SELF_LLM_ORIGINS`,
  `llmGateway.egressPosture`, and `vector.qdrant.external.url` when Qdrant is
  not bundled) is refused with every missing value and every disagreeing
  switch listed together, starting `this install cannot render yet.`
* **Qdrant is the upstream qdrant chart** (1.19.2, `condition:
  qdrant.enabled`, pinned in `Chart.lock`, carried by the published chart)
  instead of the chart's own StatefulSet. It runs one node; layer
  `values-qdrant-ha.yaml` for three (Raft clustering with peer TLS, a
  PodDisruptionBudget, anti-affinity, `replicationFactor: 2`). REST and gRPC
  are TLS (the old StatefulSet was plaintext inside the cluster), peer traffic
  too when clustered. `qdrant.enabled: false` is bring-your-own Qdrant at
  `vector.qdrant.external.url` (`vector.qdrant.external.caSecretName` for a
  private CA). Two things the upstream chart cannot do itself, and how this
  chart covers them:
  - Qdrant runs as the chart's own ServiceAccount `elitea-vectordb`, with
    `automountServiceAccountToken: false`, because the subchart's account has
    no such setting.
  - The subchart cannot read `image.pullSecrets`: if Qdrant's image comes
    through your registry, list the same Secrets under
    `qdrant.imagePullSecrets` (the install NOTES warn when you set one and
    not the other).

  The upstream chart's `helm test` Pod is not supported. It is a
  `helm.sh/hook: test` resource, so `helm install` and `helm upgrade` never
  create it and Argo CD skips Helm test hooks; do not run `helm test` on
  this release.

  | Before | After |
  |---|---|
  | `vector.qdrant.mode: internal` | `qdrant.enabled: true` (the default) |
  | `vector.qdrant.mode: external` | `qdrant.enabled: false` with `vector.qdrant.external.url` |
  | `vector.qdrant.internal.replicas` | `qdrant.replicaCount` (above 1 needs clustering: use `values-qdrant-ha.yaml`) |
  | `vector.qdrant.internal.storage.size` / `.storageClassName` | `qdrant.persistence.size` / `.storageClassName` |
  | `vector.qdrant.internal.resources` | `qdrant.resources` |
  | `vector.qdrant.internal.image` | `qdrant.image` (`tag` without `-unprivileged`; `useUnprivilegedImage: true` adds it) |
  | `vector.qdrant.internal.podSecurityContext` / `securityContext` | `qdrant.podSecurityContext` / `qdrant.containerSecurityContext` |
  | `vector.qdrant.internal.nodeSelector` / `tolerations` / `affinity` | `qdrant.nodeSelector` / `tolerations` / `affinity` |
  | StatefulSet `elitea-qdrant` | StatefulSet `elitea-vectordb` |
  | PVCs `storage-elitea-qdrant-<n>` | PVCs `qdrant-storage-elitea-vectordb-<n>` |
  | Services `elitea-qdrant`, `elitea-qdrant-headless` | Services `elitea-vectordb`, `elitea-vectordb-headless` |
  | NetworkPolicy `elitea-qdrant-netpol` | NetworkPolicy `elitea-vectordb-netpol` |

  The render refuses `vector.qdrant.mode` and `vector.qdrant.internal` with
  this map, and refuses `qdrant.apiKey` (the upstream chart's `lookup` breaks
  `helm template` and Argo CD): the key always comes from
  `elitea-qdrant-api`.

**What to do.**

1. **Keep a feature off by saying so.** Each of these is now on unless your
   values set it to `false`:

   ```yaml
   main:
     runtime:
       enabled: false
   worker:
     enabled: false
   vector:
     enabled: false
   qdrant:
     enabled: false     # with vector off: the render refuses a Qdrant nobody reads
   deepwiki:
     enabled: false
   inventory:
     enabled: false
   ```

   The render refuses a switch that another feature still needs (vector off
   under DeepWiki, say) and lists every such conflict in one message.
2. **Give Qdrant a volume.** The bundled Qdrant claims a 50Gi
   PersistentVolumeClaim from the default StorageClass
   (`qdrant.persistence.storageClassName: ""`). A cluster with no default
   StorageClass needs one named.
3. **Create the Secrets under their fixed names** (table above), or let
   `externalSecrets` render them. Drop `vector.qdrant.apiKeySecret.name` if
   it named `elitea-qdrant-api`; another name still works when the
   `QDRANT__SERVICE__API_KEY` entry in `qdrant.env` names it too.
4. **Remove the derived settings from `main.env`** (`ELITEA_DEEPWIKI_*` /
   `ELITEA_INVENTORY_*` enable flags, allowlists and client paths) and the
   `main.fileConfig.*ClientMaterial` blocks, or keep them only where they
   agree. Delete a hand-made `elitea-deepwiki-callback-client` Secret, or set
   `callbackClient.certManager.enabled: false` to keep it.
5. **Move the Qdrant data.** The upgrade deletes the old `elitea-qdrant`
   StatefulSet; its PVCs stay, but the new `elitea-vectordb` starts empty.
   Either:
   * **snapshot and restore**: BEFORE the upgrade, for each collection,
     `POST /collections/<name>/snapshots` on the old Qdrant (port 6333, header
     `api-key`) and download the snapshot; AFTER it, upload each to the new one
     with `POST /collections/<name>/snapshots/upload?priority=snapshot` over
     HTTPS (trust the `ca.crt` of the `elitea-qdrant-tls` Secret); or
   * **rebuild**: regenerate the DeepWiki wikis, rebuild the Inventory graphs
     and re-run the index ingests; the sources of truth are in PostgreSQL and
     the git hosts.

   Then delete the old claims: `kubectl -n elitea delete pvc -l
   app.kubernetes.io/name=elitea-qdrant` (only the old `storage-elitea-qdrant-*`
   claims carry that label; the new ones are labelled `app: elitea-qdrant`).
   Check the list with `get` first.
6. **Argo CD git source:** a restricted AppProject must also list
   `https://qdrant.github.io/qdrant-helm` in `sourceRepos`. The published OCI
   chart carries the qdrant chart, so an OCI source needs nothing; an
   air-gapped install mirrors the OCI chart.
