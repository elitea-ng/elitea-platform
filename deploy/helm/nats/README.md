# NATS JetStream — the platform's shared NATS

One NATS JetStream server (or 3-node cluster) serves three clients:

| Client | Uses |
|--------|------|
| `elitea-llm-gateway` | budget and rate-limit counters (`Nats-Incr`), write-behind deltas, the soft-alert cooldown KV, `budget.soft_alert` / ops events |
| `elitea-scheduler` | the `budget-writeback` durable consumer that drains the deltas into Postgres |
| `elitea-main` | the project SSE relay (`gateway.events.project.>`) and canvas presence (KV + `canvas.editors` rosters) |

and one owner: the `nats-bootstrap` hook Job (`../nats-bootstrap`), which
creates every asset. The runtime command bus (`elitea.rt.v1.>`) and its worker
identity are reserved in the permission table and land with that change.

## Security (#1076)

This chart ships ONE posture. There is no "security off" switch; the plaintext
posture is compose's (`deploy/docker-compose.yml`), and
`deploy/docker-compose.nats-secure.yml` runs this chart's config in compose.

### Identity: mTLS with `verify_and_map`

* **A dedicated NATS CA.** `templates/ca.yaml` renders the cert-manager chain
  `Issuer elitea-nats-ca-selfsigned → Certificate elitea-nats-ca (isCA) →
  Issuer elitea-nats-ca`, namespaced in this release's namespace. It is NOT
  `elitea-internal-ca`: whoever may create a `Certificate` against an issuer
  may mint any identity it signs, and a NATS identity carries publish rights
  on the budget counters. Keep `Certificate`-create RBAC on `elitea-nats-ca`
  as narrow as read access to the Secrets it writes.
  `security.ca.create: false` + `security.issuerRef` uses an issuer you run
  instead (it must sign NATS identities only).
* **Server certificate** (`templates/server-certificate.yaml`): the Service
  names, plus `*.<release>-headless` for the HA routes; `server auth` and
  `client auth` (routes verify each other). The upstream config reloader picks
  up a renewal without a restart.
* **Client certificates** carry a URI SAN `spiffe://elitea.internal/nats/<identity>`.
  The server requires TLS, verifies the certificate against the NATS CA, and
  maps the SAN to a user (`verify_and_map`). A certificate without the SAN —
  for instance any other certificate from a shared CA — maps to nothing and is
  refused. There is no `no_auth_user` and no `allow_non_tls`.
  * `elitea-main`, `elitea-llm-gateway`, `elitea-scheduler`: issued by the
    platform chart (`deploy/helm/elitea`, `nats.tls.*`), mounted at
    `/etc/elitea/nats-client`, read via `<PREFIX>_NATS_TLS_{CA,CERT,KEY}_FILE`
    (`ELITEA_EVENTS_` for main, `GATEWAY_` for gateway and scheduler).
  * `elitea-nats-bootstrap`: issued by `deploy/helm/nats-bootstrap`.
  * The clients (`libs/go/natsconn`) re-read the files on every TLS handshake,
    so a cert-manager renewal is presented on the next reconnect. TLS 1.3 only.
* **URLs carry no credential**: `tls://elitea-nats.<ns>.svc.cluster.local:4222`
  in a ConfigMap is fine. The platform chart refuses `nats://`, and
  `user:pass@`/`token@`, in any NATS URL it renders.

Because the CA Issuer is namespaced, **install NATS, its bootstrap and the
platform in the same namespace** (the Argo CD sample uses `elitea`). To run
NATS elsewhere, back a `ClusterIssuer` with a dedicated NATS CA and point
`security.issuerRef`, the bootstrap's `tls.certificate.issuerRef` and the
platform's `nats.tls.issuerRef` at it.

### The permission table

`values.yaml` → `nats.config.merge.authorization.users`. All users are in the
top-level `authorization {}` block, i.e. the global account, which is where
every stream already lives — nothing moves. Every user may subscribe to its
own inbox prefix `_INBOX_<identity>.>` only (each client sets
`nats.CustomInboxPrefix`), so nobody reads another client's JetStream API
replies or acks.

| Identity | May publish | May subscribe |
|---|---|---|
| `elitea-main` | `gateway.events.project.*.events`; `$KV.ELITEA_CANVAS_PRESENCE.>`; `$JS.API.STREAM.INFO.KV_ELITEA_CANVAS_PRESENCE`; `$JS.API.CONSUMER.{CREATE.KV_ELITEA_CANVAS_PRESENCE.>,DELETE.KV_ELITEA_CANVAS_PRESENCE.*}` (the presence watcher); `$JS.FC.KV_ELITEA_CANVAS_PRESENCE.>`. Denied: `gateway.budget.>`, `gateway.ratelimit.>`, `gateway.events.ops.>`, stream admin | `gateway.events.project.>`, `_INBOX_elitea-main.>` |
| `elitea-llm-gateway` | `gateway.budget.counter.>`, `gateway.ratelimit.counter.>`, `gateway.budget.delta`, `gateway.events.project.*.events`, `gateway.events.ops.>`, `$KV.GATEWAY_ALERT_COOLDOWN.>`; `STREAM.INFO` on its four assets; `DIRECT.GET` on `GATEWAY_BUDGET`, `GATEWAY_RATELIMIT`, `KV_GATEWAY_ALERT_COOLDOWN`. Denied: stream admin | `_INBOX_elitea-llm-gateway.>` |
| `elitea-scheduler` | `$JS.API.CONSUMER.CREATE.GATEWAY_BUDGET_DELTAS.budget-writeback.>`, `$JS.API.CONSUMER.MSG.NEXT.GATEWAY_BUDGET_DELTAS.budget-writeback`, `$JS.ACK.GATEWAY_BUDGET_DELTAS.budget-writeback.>`. Denied: stream admin | `_INBOX_elitea-scheduler.>` |
| `elitea-nats-bootstrap` | `$JS.API.INFO`, `STREAM.{NAMES,LIST,INFO,CREATE,UPDATE,DELETE,PURGE}`, `CONSUMER.{CREATE,DURABLE.CREATE,INFO,NAMES,LIST}` — the only identity that may administer a stream | `_INBOX_elitea-nats-bootstrap.>` |
| `elitea-worker` (reserved) | pull/ack/info on the `ELITEA_RT_V1_{VALIDATE,AGENT,INDEX}` consumers. Denied: `elitea.rt.v1.*.d.>` (no command injection), `gateway.>`, stream and consumer admin | `_INBOX_elitea-worker.>` |

Every grant is exercised by a test that runs the service's REAL code against a
nats-server started from this chart's rendered `nats.conf`, after the real
`bootstrap.sh` (`libs/go/natsconn/natstest`; run by `ci-go.yml` and
`ci-gateway.yml`). That is how the table was trimmed: grants the code never
used are gone, and the two the first draft missed (the cooldown bucket's direct
get, and direct gets — not `MSG.GET` — on the counter streams) are in. The
exception is `$JS.FC.KV_ELITEA_CANVAS_PRESENCE.>`, which the server only asks
for when a watcher falls behind. To probe a grant locally:

```bash
scripts/nats/render-secure-conf.sh /tmp/nats-secure.conf
ELITEA_TEST_NATS_SERVER_BIN=$(command -v nats-server) \
ELITEA_TEST_NATS_SECURE_CONF=/tmp/nats-secure.conf \
ELITEA_TEST_NATS_CLI_BIN=$(command -v nats) \
  go test -run Secured ./services/elitea-main/cmd/elitea-main/
```

`templates/guards.yaml` refuses a render that turns TLS, `verify_and_map`, the
NetworkPolicy or route TLS off, adds `no_auth_user`/`allow_non_tls`/`accounts`,
adds a non-URI or password user, lets a user subscribe to `_INBOX.>`/`>`/
`$JS.API…`, or gives stream admin to anyone but the bootstrap.

### Network

`templates/networkpolicy.yaml` admits 4222 only from pods labelled
`app.kubernetes.io/name` ∈ {`elitea-main`, `elitea-llm-gateway`,
`elitea-scheduler`, `nats-bootstrap`} (`networkPolicy.clients`, per-entry
namespace), 6222 only between the NATS pods (HA), and 7777 (exporter) from
`networkPolicy.metricsFrom`. **8222 (monitoring) has no rule**: the exporter
sidecar scrapes it over localhost and kubelet probes are not subject to
NetworkPolicy. KEDA (runtime command bus) will need a 8222 rule then.

## Assets (all owned by `nats-bootstrap`)

| Asset | Kind | Purpose |
|-------|------|---------|
| `GATEWAY_BUDGET` | stream (AllowMsgCounter, allow_direct, 1 msg/subject, 12m dedup) | int64 nano-USD budget counters (`Nats-Incr`) |
| `GATEWAY_RATELIMIT` | stream (AllowMsgCounter, allow_direct, MaxAge 5m) | per-minute rate-limit counters |
| `GATEWAY_BUDGET_DELTAS` | stream (72h / 1 GiB / 5M, 12m dedup) | write-behind deltas, drained by `budget-writeback` |
| `GATEWAY_ALERT_COOLDOWN` | KV (TTL 4h) | 80% soft-alert cooldown (`kv.Create` = SETNX-with-TTL) |
| `ELITEA_CANVAS_PRESENCE` | KV (TTL 2m, history 1) | canvas presence rosters |

The services **bind** and verify what their code depends on (counter flag,
direct get, dedup windows, TTLs); they create nothing, and a missing asset is a
boot error naming the bootstrap Job. Re-running the bootstrap reconciles drift
(`stream edit` / `kv edit`). Earlier bootstraps also created a KV bucket
`GATEWAY_BUDGET` (stream `KV_GATEWAY_BUDGET`) that nothing read; delete it by
hand if present (`nats kv del GATEWAY_BUDGET`, as the bootstrap identity).

**`max_file_store` must hold the reservations**: `GATEWAY_BUDGET_DELTAS`
reserves its 1 GiB `MaxBytes` up front, so a smaller JetStream file store
refuses the stream ("insufficient storage resources").

## NATS Server version

**NATS Server 2.12.0+ is required** for `Nats-Incr` (ADR-49). Both profiles
pin `nats:2.12.0-alpine`. The bootstrap needs natscli 0.3.0+ (`--allow-counter`;
nats-box 0.19.7 ships 0.4.0).

## Profiles (design §8.1.1)

`values.yaml` (security, permission table) is always applied; a profile adds
the topology. `Chart.yaml` pins the upstream chart as a dependency;
`helm dependency build` vendors it into `charts/`.

### scale-1 (`values-scale1.yaml`) — the deployment default

Single NATS node, assets at `replicas = 1`, file storage. HA is intentionally
waived. Pair with `LLM_BUDGET_EXPECTED_REPLICAS=1`.

```bash
helm dependency build deploy/helm/nats
helm upgrade --install elitea-nats deploy/helm/nats \
  -n elitea --create-namespace \
  -f deploy/helm/nats/values-scale1.yaml
```

### HA (`values-ha.yaml`) — opt-in

3 nodes, assets at `replicas = 3`, routes over TLS with mutual verification.
HA operators MUST also set the gateway's `LLM_BUDGET_EXPECTED_REPLICAS` to the
real gateway replica count and run the bootstrap with `replicas=3`.

```bash
helm upgrade --install elitea-nats deploy/helm/nats \
  -n elitea --create-namespace \
  -f deploy/helm/nats/values-ha.yaml
```

## Verifying

The server refuses a connection without a mapped certificate, so operator
commands present one. The bootstrap identity can read everything:

```bash
kubectl -n elitea get secret elitea-nats-bootstrap-nats-client-tls -o json \
  | jq -r '.data | to_entries[] | "\(.key) \(.value)"' \
  | while read -r k v; do echo "$v" | base64 -d > "/tmp/nats-$k"; done
kubectl -n elitea port-forward svc/elitea-nats 4222:4222 &
N="nats --server tls://elitea-nats:4222 --tlsca /tmp/nats-ca.crt --tlscert /tmp/nats-tls.crt --tlskey /tmp/nats-tls.key --inbox-prefix _INBOX_elitea-nats-bootstrap"
# (map elitea-nats to 127.0.0.1 in /etc/hosts for the server name to verify)
$N stream ls          # GATEWAY_BUDGET, GATEWAY_RATELIMIT, GATEWAY_BUDGET_DELTAS, KV_...
$N kv ls              # GATEWAY_ALERT_COOLDOWN, ELITEA_CANVAS_PRESENCE
```

Who is connected, as whom (from inside the pod; 8222 is not reachable from
elsewhere):

```bash
kubectl -n elitea exec elitea-nats-0 -c nats -- wget -qO- 'http://127.0.0.1:8222/connz?auth=1' \
  | jq -r '.connections[] | "\(.name)\t\(.authorized_user)"'
```

Every connection must show one of the five `spiffe://elitea.internal/nats/…`
users. A plaintext `nats pub` or a pod outside the four clients must fail
(the first with a TLS/authorization error, the second with a connect timeout).
