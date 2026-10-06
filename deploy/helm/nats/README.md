# NATS JetStream — the platform's shared NATS

One NATS JetStream server (or 3-node cluster) serves three clients:

| Client | Uses |
|--------|------|
| `elitea-llm-gateway` | budget and rate-limit counters (`Nats-Incr`), write-behind deltas, the soft-alert cooldown KV, `budget.soft_alert` / ops events |
| `elitea-scheduler` | the `budget-writeback` durable consumer that drains the deltas into Postgres |
| `elitea-main` | the project SSE relay (`gateway.events.project.>`) and canvas presence (KV + `canvas.editors` rosters) |

and one owner: the `nats-bootstrap` hook Job (`../nats-bootstrap`), which
creates every asset, connecting to each plane's account as that account's own
bootstrap identity. The runtime command bus (`elitea.rt.v1.>`), its producer
and worker identities and its account (`RUNTIME`) are reserved in the
permission table and land with that change.

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
  * `elitea-nats-bootstrap-{main,gateway,runtime}`: issued by
    `deploy/helm/nats-bootstrap`, one per account.
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

### Accounts: one per plane

`values.yaml` → `nats.config.merge.accounts`. Each plane is its own NATS
account — its own subject space and its own JetStream — and every user is
declared in exactly one:

| Account | Identities | Assets |
|---|---|---|
| `MAIN` | `elitea-main`, `elitea-nats-bootstrap-main` | `ELITEA_CANVAS_PRESENCE` (KV) |
| `GATEWAY` | `elitea-llm-gateway`, `elitea-scheduler`, `elitea-nats-bootstrap-gateway` | `GATEWAY_BUDGET`, `GATEWAY_RATELIMIT`, `GATEWAY_BUDGET_DELTAS`, `GATEWAY_ALERT_COOLDOWN` (KV) |
| `RUNTIME` (reserved) | `elitea-main-runtime`, `elitea-worker`, `elitea-nats-bootstrap-runtime` | the command bus's `ELITEA_RT_V1_*` streams and `ELITEA_RT_QUARANTINE` (KV), with that change |

Why accounts and not only per-user permissions: a JetStream **push
consumer's deliver subject is not checked against its creator's publish
permissions** (nats-server `consumer.go`). In one shared account,
elitea-main's grant to create its presence watcher was enough to point a
consumer's deliveries at `gateway.budget.delta` and write into
`GATEWAY_BUDGET_DELTAS`, and a consumer-create grant on the deltas stream
could re-deliver spend deltas onto a project's SSE subject. Across accounts a
redirected delivery can only land inside its creator's own account, where the
other plane's streams do not exist. `TestSecuredRedirectedDeliveryStaysInMain`
(elitea-main) reproduces the attack against the rendered config and asserts
that nothing reaches `GATEWAY_BUDGET_DELTAS`.

**The one cross-account flow**: `GATEWAY` exports the stream
`gateway.events.project.*.events` (the per-project `budget.soft_alert`) to
`MAIN` only, and `MAIN` imports it, so the project SSE relay can forward it.
`gateway.events.ops.>` (the operator-only loss record) is not exported, and
nothing else crosses.

**One subject family per producer.** The project SSE route
(`GET /api/v2/events/prompt_lib/{projectID}`) reads two subjects and accepts
ONE event type from each:

| Family | Subject | Account | Publisher | Accepted type |
|---|---|---|---|---|
| presence | `elitea.events.project.<id>.presence` | MAIN | elitea-main only | `canvas.editors` |
| gateway | `gateway.events.project.<id>.events` | GATEWAY, imported into MAIN | elitea-llm-gateway only | `budget.soft_alert` |

So the gateway cannot forge a `canvas.editors` roster into a project's stream
(its subject is the gateway's, and the route drops that type there), and
elitea-main cannot publish on the gateway's family at all.

Every user may subscribe to its own inbox prefix `_INBOX_<identity>.>` only
(each client sets `nats.CustomInboxPrefix`), so nobody reads another client's
JetStream API replies or acks. Nobody may delete or purge a stream; only each
account's bootstrap identity may create or update one, or create a consumer —
a consumer-create grant is also the right to aim a push consumer's deliveries
anywhere in the account. The one exception is elitea-main's presence watcher
(an ordered consumer on its own bucket, inside MAIN). So the scheduler binds
to the `budget-writeback` pull consumer the GATEWAY bootstrap creates (with
the AckWait and MaxDeliver in `deploy/helm/nats-bootstrap` `deltas.writeback`)
and cannot redefine it, and the gateway holds no consumer grant at all.

### The permission table

| Account | Identity | May publish | May subscribe |
|---|---|---|---|
| MAIN | `elitea-main` | `elitea.events.project.*.presence` (its presence family); `$KV.ELITEA_CANVAS_PRESENCE.>`; `$JS.API.STREAM.INFO.KV_ELITEA_CANVAS_PRESENCE`; `$JS.API.CONSUMER.{CREATE.KV_ELITEA_CANVAS_PRESENCE.>,DELETE.KV_ELITEA_CANVAS_PRESENCE.*}` (the presence watcher); `$JS.FC.KV_ELITEA_CANVAS_PRESENCE.>`. Denied: `gateway.>` (it never speaks for the gateway), stream admin | `elitea.events.project.*.presence`, `gateway.events.project.*.events` (imported), `_INBOX_elitea-main.>` |
| MAIN | `elitea-nats-bootstrap-main` | `$JS.API.INFO`, `STREAM.{NAMES,LIST}`, `STREAM.{INFO,CREATE,UPDATE}.KV_ELITEA_CANVAS_PRESENCE` | `_INBOX_elitea-nats-bootstrap-main.>` |
| GATEWAY | `elitea-llm-gateway` | `gateway.budget.counter.>`, `gateway.ratelimit.counter.>`, `gateway.budget.delta`, `gateway.events.project.*.events`, `gateway.events.ops.>`, `$KV.GATEWAY_ALERT_COOLDOWN.>`; `STREAM.INFO` on its four assets; `DIRECT.GET` on `GATEWAY_BUDGET`, `GATEWAY_RATELIMIT`, `KV_GATEWAY_ALERT_COOLDOWN`. Denied: stream admin, every `$JS.API.CONSUMER.>` | `_INBOX_elitea-llm-gateway.>` |
| GATEWAY | `elitea-scheduler` | `$JS.API.CONSUMER.{INFO,MSG.NEXT}.GATEWAY_BUDGET_DELTAS.budget-writeback`, `$JS.ACK.GATEWAY_BUDGET_DELTAS.budget-writeback.>` — it binds to the consumer the bootstrap creates. Denied: stream admin, consumer create/delete | `_INBOX_elitea-scheduler.>` |
| GATEWAY | `elitea-nats-bootstrap-gateway` | `$JS.API.INFO`, `STREAM.{NAMES,LIST}`, `STREAM.{INFO,CREATE,UPDATE}` on its four assets, `CONSUMER.{CREATE,INFO}` on `GATEWAY_BUDGET_DELTAS.budget-writeback` | `_INBOX_elitea-nats-bootstrap-gateway.>` |
| RUNTIME | `elitea-main-runtime` (reserved) | `elitea.rt.v1.*.d.*`; `STREAM.INFO` and `CONSUMER.INFO` on `ELITEA_RT_V1_{VALIDATE,AGENT,INDEX}`. Denied: stream admin, consumer create/delete | `_INBOX_elitea-main-runtime.>` |
| RUNTIME | `elitea-worker` (reserved) | `CONSUMER.{INFO,MSG.NEXT}` and `$JS.ACK` on `elitea-<route>-worker-v1` of `ELITEA_RT_V1_<ROUTE>`; `$KV.ELITEA_RT_QUARANTINE.>` + its `STREAM.INFO` (dead letters). Denied: `elitea.rt.v1.*.d.>` (no command injection), stream admin, consumer create/delete | `_INBOX_elitea-worker.>` |
| RUNTIME | `elitea-nats-bootstrap-runtime` | `$JS.API.INFO`, `STREAM.{NAMES,LIST}`, `STREAM.{INFO,CREATE,UPDATE}` on the three route streams and `KV_ELITEA_RT_QUARANTINE`, `CONSUMER.{CREATE,INFO}` on the three route durables | `_INBOX_elitea-nats-bootstrap-runtime.>` |

The RUNTIME rows are what the runtime command bus change binds to: elitea-main
presents `elitea-main-runtime` with `ELITEA_RUNTIME_NATS_URL` and
`ELITEA_RUNTIME_NATS_TLS_{CA,CERT,KEY}_FILE`, separate from its live-update
identity, and neither the producer nor the worker may create a consumer.

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
NetworkPolicy or route TLS off; sets `no_auth_user`, `allow_non_tls` or a
top-level `authorization` block; declares accounts other than exactly `MAIN`,
`GATEWAY` and `RUNTIME`, or one without JetStream; declares an identity twice,
a non-URI, password, nkey or token user, or an account without exactly its own
bootstrap; lets a user subscribe to `_INBOX.>`/`>`/`$JS.API…`; lets anyone
delete or purge a stream or anyone but the account's bootstrap create or
update one; or widens the export/import beyond GATEWAY's soft-alert stream to
MAIN.

### Network

`templates/networkpolicy.yaml` admits 4222 only from pods labelled
`app.kubernetes.io/name` ∈ {`elitea-main`, `elitea-llm-gateway`,
`elitea-scheduler`, `nats-bootstrap`} (`networkPolicy.clients`, per-entry
namespace), 6222 only between the NATS pods (HA), and 7777 (exporter) from
`networkPolicy.metricsFrom`. **8222 (monitoring) has no rule**: the exporter
sidecar scrapes it over localhost and kubelet probes are not subject to
NetworkPolicy. KEDA (runtime command bus) will need a 8222 rule then.

## Assets (all owned by `nats-bootstrap`)

| Asset | Account | Kind | Purpose |
|-------|---------|------|---------|
| `GATEWAY_BUDGET` | GATEWAY | stream (AllowMsgCounter, allow_direct, 1 msg/subject, 12m dedup) | int64 nano-USD budget counters (`Nats-Incr`) |
| `GATEWAY_RATELIMIT` | GATEWAY | stream (AllowMsgCounter, allow_direct, MaxAge 5m) | per-minute rate-limit counters |
| `GATEWAY_BUDGET_DELTAS` | GATEWAY | stream (72h / 1 GiB / 5M, 12m dedup) | write-behind deltas, drained by `budget-writeback` |
| `GATEWAY_ALERT_COOLDOWN` | GATEWAY | KV (TTL 4h) | 80% soft-alert cooldown (`kv.Create` = SETNX-with-TTL) |
| `ELITEA_CANVAS_PRESENCE` | MAIN | KV (TTL 2m, history 1) | canvas presence rosters |
| `GATEWAY_BUDGET_DELTAS` / `budget-writeback` | GATEWAY | durable pull consumer (explicit ack, AckWait 30s, MaxDeliver 10) | the scheduler's write-back drain; it binds only |

The services **bind** and verify what their code depends on (counter flag,
direct get, dedup windows, TTLs); they create nothing, and a missing asset is a
boot error naming the bootstrap Job. Re-running the bootstrap reconciles drift
(`stream edit` / `kv edit`).

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
commands present one, and an identity sees only its own account. The GATEWAY
bootstrap identity reads the gateway's streams:

```bash
kubectl -n elitea get secret elitea-nats-bootstrap-gateway-nats-client-tls -o json \
  | jq -r '.data | to_entries[] | "\(.key) \(.value)"' \
  | while read -r k v; do echo "$v" | base64 -d > "/tmp/nats-$k"; done
kubectl -n elitea port-forward svc/elitea-nats 4222:4222 &
N="nats --server tls://elitea-nats:4222 --tlsca /tmp/nats-ca.crt --tlscert /tmp/nats-tls.crt --tlskey /tmp/nats-tls.key --inbox-prefix _INBOX_elitea-nats-bootstrap-gateway"
# (map elitea-nats to 127.0.0.1 in /etc/hosts for the server name to verify)
$N stream ls          # GATEWAY_BUDGET, GATEWAY_RATELIMIT, GATEWAY_BUDGET_DELTAS, KV_GATEWAY_ALERT_COOLDOWN
```

(`elitea-nats-bootstrap-main-nats-client-tls` the same way shows MAIN's
`KV_ELITEA_CANVAS_PRESENCE`, and nothing of GATEWAY's.)

Who is connected, as whom (from inside the pod; 8222 is not reachable from
elsewhere):

```bash
kubectl -n elitea exec elitea-nats-0 -c nats -- wget -qO- 'http://127.0.0.1:8222/connz?auth=1' \
  | jq -r '.connections[] | "\(.name)\t\(.account)\t\(.authorized_user)"'
```

Every connection must show one of the `spiffe://elitea.internal/nats/…` users,
in its plane's account. A plaintext `nats pub` or a pod outside the four clients must fail
(the first with a TLS/authorization error, the second with a connect timeout).
