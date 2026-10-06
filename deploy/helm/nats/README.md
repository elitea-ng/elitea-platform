# NATS JetStream — the platform's shared NATS

One NATS JetStream server (or 3-node cluster) serves three clients:

| Client | Uses |
|--------|------|
| `elitea-llm-gateway` | budget and rate-limit counters (`Nats-Incr`), write-behind deltas, the soft-alert cooldown KV, `budget.soft_alert` / ops events |
| `elitea-scheduler` | the `budget-writeback` durable consumer that drains the deltas into Postgres (from its own `SCHEDULER` account, through service imports) |
| `elitea-main` | the project SSE relay (`gateway.events.project.>`) and canvas presence (KV + `canvas.editors` rosters) |
| `elitea-main-runtime` | elitea-main's runtime plane: PRODUCES the runtime command bus (`elitea.rt.v1.<route>.d.*`, `../../../docs/runtime-command-bus.md`) and the execution-replay wake-up |
| `elitea-worker` | the Rust and Python workers: CONSUME the command bus (pull, +WPI, ack, nak-with-delay) and record dead letters |

and one owner: the `nats-bootstrap` hook Job (`../nats-bootstrap`), which
creates every asset, connecting to each plane's account as that account's own
bootstrap identity — including the command bus's streams and durable consumers
in `RUNTIME`, and its dead-letter bucket in `WORKER`.

## Security (#1076)

This chart ships ONE posture. There is no "security off" switch; the plaintext
posture is compose's (`deploy/docker-compose.yml`), and
`deploy/docker-compose.nats-secure.yml` runs this chart's config in compose.

### Identity: mTLS with `verify_and_map`

* **A separate NATS CA.** `templates/ca.yaml` renders the cert-manager chain
  `Issuer elitea-nats-ca-selfsigned → Certificate elitea-nats-ca (isCA) →
  Issuer elitea-nats-ca`, namespaced in this release's namespace. It is NOT
  `elitea-internal-ca`, so a Certificate against the platform's internal
  issuer is not a NATS identity. That is ALL the separation buys — see "Who
  can mint a NATS identity" below. `security.ca.create: false` +
  `security.issuerRef` uses an issuer you run instead (it must sign NATS
  identities only).
* **Server certificate** (`templates/server-certificate.yaml`): the client
  port only — the Service names, `server auth`. The upstream config reloader
  picks up a renewal without a restart.
* **Route identity (HA)**: cluster routes do NOT trust the client CA. A
  separate ROUTE CA (`templates/ca.yaml`: `Certificate elitea-nats-route-ca` →
  `Issuer elitea-nats-route-ca`) signs one route certificate
  (`templates/route-certificate.yaml`: `*.<release>-headless`, `server auth` +
  `client auth`), mounted at `/etc/nats-certs/cluster`, and the route TLS
  block verifies peers against that Secret's `ca.crt` (`values-ha.yaml`
  `cluster.tls.merge.ca_file`). Before this, routes verified against the
  client CA, so any service's client certificate could join the cluster as a
  peer and see every account's traffic. `TestRoutesAcceptOnlyRouteCertificates`
  (`libs/go/natsconn/natstest`) starts a node from the HA profile's own
  cluster block and proves a client-CA certificate is refused on the route
  port while a route certificate joins. With `security.ca.create: false`, set
  `security.routeIssuerRef` to an issuer of a different CA than
  `security.issuerRef` (the guards refuse the same one).
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
    `deploy/helm/nats-bootstrap`, one per account, and only for the length
    of an install: the Certificates are hooks in the Job's phase, ordered
    before it and deleted with it, and each lives one hour. Nothing renews
    them, so the identities that may create streams and consumers stop
    working an hour after each install (the Secrets cert-manager leaves
    behind hold the expired certificate; run cert-manager with
    `--enable-certificate-owner-ref` to have them removed too). They may
    create and update their account's assets and nothing else — no identity
    may delete or purge a stream.
  * The clients (`libs/go/natsconn`) re-read the files on every TLS handshake,
    so a cert-manager renewal is presented on the next reconnect. TLS 1.3 only.
* **URLs carry no credential**: `tls://elitea-nats.<ns>.svc.cluster.local:4222`
  in a ConfigMap is fine. The platform chart refuses `nats://`, and
  `user:pass@`/`token@`, in any NATS URL it renders.

Because the CA Issuer is namespaced, **install NATS, its bootstrap and the
platform in the same namespace** (the Argo CD sample uses `elitea`; the
platform chart refuses a different `nats.namespace` while its issuer is a
namespaced `Issuer`, because its client Certificates would never be issued).
To run NATS elsewhere, back a `ClusterIssuer` with a CA
used for NATS only and point `security.issuerRef`, the bootstrap's
`tls.certificate.issuerRef` and the platform's `nats.tls.issuerRef` at it.

### Who can mint a NATS identity

A NATS identity is a certificate the server's CA signed with the right URI
SAN. Three groups can get one:

1. **Anyone who may create a cert-manager `Certificate` or
   `CertificateRequest` in the NATS namespace.** The namespaced Issuer signs
   whatever such a request asks for — any service's identity, a bootstrap's
   (which may create streams), a server or route certificate. cert-manager's
   default approver approves every request.
2. **Anyone who may read Secrets in the NATS namespace.** The CA key is
   Secret `elitea-nats-ca` (and `elitea-nats-route-ca` in HA); with it a
   certificate is minted offline, and nothing in the cluster sees it.
3. **Anyone who may read a client's Secret** (`<component>-nats-client-tls`)
   holds that one identity until the certificate expires.

What narrows them:

* **RBAC.** Grant `create` on `certificates.cert-manager.io` and
  `certificaterequests.cert-manager.io`, and `get`/`list`/`watch` on
  `secrets`, in the NATS namespace to the deployer (Argo CD, Helm) and
  cluster operators only — not to workloads or to every namespace member.
  An admin role that already holds those verbs everywhere holds every NATS
  identity.
* **approver-policy** (`security.approverPolicy`, `templates/approver-policy.yaml`).
  With [cert-manager approver-policy](https://cert-manager.io/docs/policy/approval/approver-policy/)
  installed and cert-manager's built-in approver disabled
  (`--controllers=*,-certificaterequests-approver`), the chart's
  `CertificateRequestPolicy` objects pin what each issuer signs: client
  identities only with exactly one of the permission table's URI SANs, `client
  auth`, no DNS names, no CA, at most `maxClientDuration`; the server
  certificate only for the Service names; the route certificate only for the
  headless names; the self-signed issuer only the two CA certificates. It
  stops arbitrary or CA certificates; it does NOT stop a permitted identity
  being requested by someone with `create` on Certificates — cert-manager's
  service account submits every request, so the policy cannot tell who asked.
  `enabled: auto` renders the policies when the API is served.
* **A CA key outside the namespace.** `security.ca.create: false` with a
  `ClusterIssuer` (or an external issuer) whose key lives elsewhere removes
  group 2. Group 1 remains for any namespace that may reference that
  ClusterIssuer; pair it with approver-policy.

### Accounts: one per plane

`values.yaml` → `nats.config.merge.accounts`. Each plane is its own NATS
account — its own subject space and, where it owns assets, its own
JetStream — and every user is declared in exactly one. **Every account's sole
writer is its owner**: MAIN's assets are written by elitea-main alone,
GATEWAY's by the gateway alone (its bootstrap creates them and publishes no
data), RUNTIME's command streams by its producer (the workers only pull,
ack and nak them, from WORKER), WORKER's dead-letter bucket by the workers;
SCHEDULER stores nothing.

| Account | Identities | Assets |
|---|---|---|
| `MAIN` | `elitea-main`, `elitea-nats-bootstrap-main` | `ELITEA_CANVAS_PRESENCE` (KV) |
| `GATEWAY` | `elitea-llm-gateway`, `elitea-nats-bootstrap-gateway` | `GATEWAY_BUDGET`, `GATEWAY_RATELIMIT`, `GATEWAY_BUDGET_DELTAS`, `GATEWAY_ALERT_COOLDOWN` (KV) |
| `SCHEDULER` | `elitea-scheduler` | none — no JetStream, no bootstrap |
| `RUNTIME` | `elitea-main-runtime`, `elitea-nats-bootstrap-runtime` | the command bus: `ELITEA_RT_V1_{VALIDATE,AGENT,INDEX}` streams, their worker durables, the replay wake-up |
| `WORKER` | `elitea-worker`, `elitea-nats-bootstrap-worker` | `ELITEA_RT_V1_DEADLETTER` (KV) — and nothing else (`jetstream.max_streams: 1`) |

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

Why the scheduler has an account of its own: a **request's reply subject is
not checked against the requester's permissions** either. The server checks a
publish against the subject published to, and answers a permitted JetStream
API request — a pull's deliveries, a consumer's info, an ack's confirmation —
on whatever reply subject the request named (only `$JS.ACK.` and `_GR_.`
replies are refused). As a GATEWAY user, the scheduler's pull grant on
`budget-writeback` was enough to send `MSG.NEXT` with reply
`gateway.budget.delta` and have the server copy deltas back into
`GATEWAY_BUDGET_DELTAS` (double-counted spend once past the 12m dedup window;
with discard-old, a flood evicts real deltas), or steer answers into the
cooldown KV. In `SCHEDULER` the same reply subject is SCHEDULER's, where no
stream exists. `TestSecuredSchedulerCannotStoreIntoGatewayByReplySubject`
(elitea-scheduler) makes every such request as the scheduler and asserts
that no GATEWAY stream gains a message; against the previous single-account
table the same test sees `GATEWAY_BUDGET_DELTAS` go from 4 to 11 messages.

The worker has an account of its own for the same reason (#1081 review). As a
RUNTIME user, its pull and info grants let it name a command subject
(`elitea.rt.v1.<route>.d.<token>`) as the reply: `MSG.NEXT` copied a signed
command into another route's stream, and info answers filled
`ELITEA_RT_V1_AGENT` to its `MaxMsgs` — the producer was then refused
"maximum messages exceeded", and with no delete or purge grant the junk
stayed until `MaxAge` (up to 26h). In `WORKER` every such reply lands in
WORKER, whose JetStream holds the dead-letter bucket alone (which the worker
writes anyway). The bucket is WORKER's and not RUNTIME's so that the producer,
which stays in RUNTIME as the streams' writer, cannot steer one of its own
answers into `$KV.ELITEA_RT_V1_DEADLETTER.<key>` and forge a dead letter.
`TestSecuredWorkerCannotStoreIntoCommandStreamsByReplySubject`
(elitea-main, `internal/transport/commandbus`) makes every request the worker
may make with replies on every route's command subject and on WORKER's own
JetStream API, and the producer's requests with replies on the bucket, and
asserts that no stream changes; against the previous table the same attack
took `ELITEA_RT_V1_VALIDATE` from 0 to 4 messages.

**The cross-account flows**, and nothing else crosses:

* `GATEWAY` exports the stream `gateway.events.project.*.events` (the
  per-project `budget.soft_alert`) to `MAIN` only, and `MAIN` imports it, so
  the project SSE relay can forward it. `gateway.events.ops.>` (the
  operator-only loss record) is not exported.
* `GATEWAY` exports to `SCHEDULER` only, as **services**, exactly what a bound
  pull consumer uses on exactly one durable:
  `$JS.API.CONSUMER.INFO.GATEWAY_BUDGET_DELTAS.budget-writeback`,
  `$JS.API.CONSUMER.MSG.NEXT.GATEWAY_BUDGET_DELTAS.budget-writeback`
  (`response_type: stream` — one pull, a batch of deliveries) and
  `$JS.ACK.GATEWAY_BUDGET_DELTAS.budget-writeback.>`. `SCHEDULER` imports the
  two API subjects under the JetStream API prefix `JS.GATEWAY.API`
  (`natsconn.SchedulerGatewayJSAPIPrefix`; the scheduler opens JetStream with
  `jetstream.NewWithAPIPrefix`), because an account without JetStream answers
  every `$JS.API` request itself with "JetStream not enabled". The ack
  subjects keep their name: they are each delivery's reply subject. A
  service's answer goes back to the requester's reply subject in the
  requester's account.
* `RUNTIME` exports to `WORKER` only, as **services**, the same three subjects
  for each of the three worker durables (`elitea-configuration-worker-v1` on
  `ELITEA_RT_V1_VALIDATE`, `elitea-agent-worker-v1`, `elitea-index-worker-v1`):
  `CONSUMER.INFO`, `CONSUMER.MSG.NEXT` (`response_type: stream`) and
  `$JS.ACK.<stream>.<durable>.>`. `WORKER` imports the API subjects under the
  prefix `JS.RUNTIME.API` (`natsconn.WorkerRuntimeJSAPIPrefix`; the Rust and
  Python workers open their command-bus JetStream context with it when they
  present an identity, and use WORKER's own `$JS.API` for the dead-letter
  bucket). No `STREAM.INFO` is exported: the workers verify their durable,
  elitea-main verifies the streams.

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
and cannot redefine it (SCHEDULER is not even exported a consumer-create
subject), and the gateway holds no consumer grant at all.

### The permission table

| Account | Identity | May publish | May subscribe |
|---|---|---|---|
| MAIN | `elitea-main` | `elitea.events.project.*.presence` (its presence family); `$KV.ELITEA_CANVAS_PRESENCE.>`; `$JS.API.STREAM.INFO.KV_ELITEA_CANVAS_PRESENCE`; `$JS.API.CONSUMER.{CREATE.KV_ELITEA_CANVAS_PRESENCE.>,DELETE.KV_ELITEA_CANVAS_PRESENCE.*}` (the presence watcher); `$JS.FC.KV_ELITEA_CANVAS_PRESENCE.>`. Denied: `gateway.>` (it never speaks for the gateway), stream admin | `elitea.events.project.*.presence`, `gateway.events.project.*.events` (imported), `_INBOX_elitea-main.>` |
| MAIN | `elitea-nats-bootstrap-main` | `$JS.API.INFO`, `STREAM.{NAMES,LIST}`, `STREAM.{INFO,CREATE,UPDATE}.KV_ELITEA_CANVAS_PRESENCE` | `_INBOX_elitea-nats-bootstrap-main.>` |
| GATEWAY | `elitea-llm-gateway` | `gateway.budget.counter.>`, `gateway.ratelimit.counter.>`, `gateway.budget.delta`, `gateway.events.project.*.events`, `gateway.events.ops.>`, `$KV.GATEWAY_ALERT_COOLDOWN.>`; `STREAM.INFO` on its four assets; `DIRECT.GET` on `GATEWAY_BUDGET`, `GATEWAY_RATELIMIT`, `KV_GATEWAY_ALERT_COOLDOWN`. Denied: stream admin, every `$JS.API.CONSUMER.>` | `_INBOX_elitea-llm-gateway.>` |
| GATEWAY | `elitea-nats-bootstrap-gateway` | `$JS.API.INFO`, `STREAM.{NAMES,LIST}`, `STREAM.{INFO,CREATE,UPDATE}` on its four assets, `CONSUMER.{CREATE,INFO}` on `GATEWAY_BUDGET_DELTAS.budget-writeback` | `_INBOX_elitea-nats-bootstrap-gateway.>` |
| SCHEDULER | `elitea-scheduler` | `JS.GATEWAY.API.CONSUMER.{INFO,MSG.NEXT}.GATEWAY_BUDGET_DELTAS.budget-writeback`, `$JS.ACK.GATEWAY_BUDGET_DELTAS.budget-writeback.>` — the three imported services, nothing else; it binds to the consumer the GATEWAY bootstrap creates | `_INBOX_elitea-scheduler.>` |
| RUNTIME | `elitea-main-runtime` | `elitea.rt.v1.{validate,agent,index}.d.*` (commands), `elitea.rt.v1.replay.wake`; `STREAM.INFO` and `DIRECT.GET` on `ELITEA_RT_V1_{VALIDATE,AGENT,INDEX}`; `CONSUMER.INFO` on their durables. Denied: `$JS.ACK.>`, `MSG.NEXT`, `$KV.>`, stream admin, consumer create/delete | `_INBOX_elitea-main-runtime.>`, `elitea.rt.v1.replay.wake` |
| RUNTIME | `elitea-nats-bootstrap-runtime` | `$JS.API.INFO`, `STREAM.{NAMES,LIST}`, `STREAM.{INFO,CREATE,UPDATE}` on the three route streams, `CONSUMER.{CREATE,INFO}` on the three route durables | `_INBOX_elitea-nats-bootstrap-runtime.>` |
| WORKER | `elitea-worker` | `JS.RUNTIME.API.CONSUMER.{INFO,MSG.NEXT}` and `$JS.ACK` on its three durables only — the nine imported services, nothing else in RUNTIME; `$KV.ELITEA_RT_V1_DEADLETTER.>` + its `STREAM.INFO` (dead letters, WORKER's own JetStream). Denied: `elitea.rt.v1.>`, stream admin, consumer create/delete | `_INBOX_elitea-worker.>` |
| WORKER | `elitea-nats-bootstrap-worker` | `$JS.API.INFO`, `STREAM.{NAMES,LIST}`, `STREAM.{INFO,CREATE,UPDATE}` on `KV_ELITEA_RT_V1_DEADLETTER` | `_INBOX_elitea-nats-bootstrap-worker.>` |

The RUNTIME and WORKER rows are the runtime command bus (`../../../docs/runtime-command-bus.md`):
elitea-main presents `elitea-main-runtime` with `ELITEA_RUNTIME_NATS_URL` and
`ELITEA_RUNTIME_NATS_TLS_{CA,CERT,KEY}_FILE`, separate from its live-update
identity; the Rust and Python workers present `elitea-worker`
(`runtime.json` `nats_*`). Neither the producer nor the worker may create a
consumer; the replay wake-up is published and subscribed by the producer only,
inside RUNTIME.

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
NetworkPolicy or route TLS off, points route TLS at the client certificate or
the client CA, or turns route verification off; sets `no_auth_user`, `allow_non_tls` or a
top-level `authorization` block; declares accounts other than exactly `MAIN`,
`GATEWAY`, `SCHEDULER`, `RUNTIME` and `WORKER`, MAIN/GATEWAY/RUNTIME without
JetStream, SCHEDULER with it, or WORKER's without `max_streams: 1`; puts
`elitea-scheduler` anywhere but SCHEDULER, `elitea-worker` anywhere but
WORKER, or anyone else in either; declares an identity twice, a non-URI, password, nkey or
token user, or a JetStream account without exactly its own bootstrap; lets a
user subscribe to `_INBOX.>`/`>`/`$JS.API…`; lets anyone delete or purge a
stream or anyone but the account's bootstrap create or update one; or widens
the exports/imports beyond GATEWAY's soft-alert stream to MAIN, the three
budget-writeback services to SCHEDULER and RUNTIME's nine worker-durable
services to WORKER (subject, account, response type and the `JS.GATEWAY.API`
/ `JS.RUNTIME.API` mappings are all pinned).

### Network

`templates/networkpolicy.yaml` admits 4222 only from pods labelled
`app.kubernetes.io/name` ∈ {`elitea-main`, `elitea-worker-python` (the worker
pods, Rust or Python image), `elitea-llm-gateway`, `elitea-scheduler`,
`nats-bootstrap`} (`networkPolicy.clients`, per-entry namespace), 6222 only
between the NATS pods (HA), 7777 (exporter) from `networkPolicy.metricsFrom`,
and 8222 (monitoring, `/jsz`) from `networkPolicy.monitoringFrom` — the KEDA
operator, whose `nats-jetstream` scaler sizes the worker fleet on consumer lag.
The exporter sidecar reads 8222 over localhost and kubelet probes are not
subject to NetworkPolicy.

## Assets (all owned by `nats-bootstrap`)

| Asset | Account | Kind | Purpose |
|-------|---------|------|---------|
| `GATEWAY_BUDGET` | GATEWAY | stream (AllowMsgCounter, allow_direct, 1 msg/subject, 12m dedup) | int64 nano-USD budget counters (`Nats-Incr`) |
| `GATEWAY_RATELIMIT` | GATEWAY | stream (AllowMsgCounter, allow_direct, MaxAge 5m) | per-minute rate-limit counters |
| `GATEWAY_BUDGET_DELTAS` | GATEWAY | stream (72h / 1 GiB / 5M, 12m dedup) | write-behind deltas, drained by `budget-writeback` |
| `GATEWAY_ALERT_COOLDOWN` | GATEWAY | KV (TTL 4h) | 80% soft-alert cooldown (`kv.Create` = SETNX-with-TTL) |
| `ELITEA_CANVAS_PRESENCE` | MAIN | KV (TTL 2m, history 1) | canvas presence rosters |
| `GATEWAY_BUDGET_DELTAS` / `budget-writeback` | GATEWAY | durable pull consumer (explicit ack, AckWait 30s, MaxDeliver 10) | the scheduler's write-back drain; it binds only |
| `ELITEA_RT_V1_{VALIDATE,AGENT,INDEX}` | RUNTIME | WorkQueue streams (discard new + per subject, 1 msg/subject, ≤1024 msgs, 64 MiB, 64 KiB msgs, MaxAge 3h/26h/26h, 2m dedup, allow_direct, deny delete/purge) | the runtime command bus, one stream per route |
| `elitea-{configuration,agent,index}-worker-v1` | RUNTIME | durable pull consumers (AckWait 60s, MaxDeliver -1, MaxWaiting 512) | the workers' shared consumer, one per stream; they bind only |
| `ELITEA_RT_V1_DEADLETTER` | WORKER | KV (TTL 7d, history 1) | poison commands the workers recorded |

Every JetStream write is synced before it is acknowledged:
`jetstream { sync_interval: always }` (`values.yaml`, owner decision for the
command bus; it applies to every account's streams on the server).

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

3 nodes, assets at `replicas = 3`, routes over TLS with mutual verification
against the route CA (their own identity, not a client's).
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
bootstrap identity reads the gateway's streams. Its certificate lives one hour
after an install, so sync the `nats-bootstrap` release first (`helm upgrade`,
or `argocd app sync nats-bootstrap`) and read within the hour:

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

Who is connected, as whom (from inside the pod; 8222 is reachable only from
the KEDA operator):

```bash
kubectl -n elitea exec elitea-nats-0 -c nats -- wget -qO- 'http://127.0.0.1:8222/connz?auth=1' \
  | jq -r '.connections[] | "\(.name)\t\(.account)\t\(.authorized_user)"'
```

Every connection must show one of the `spiffe://elitea.internal/nats/…` users,
in its plane's account. A plaintext `nats pub` or a pod outside the five client workloads must fail
(the first with a TLS/authorization error, the second with a connect timeout).
