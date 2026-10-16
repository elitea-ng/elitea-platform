# elitea-egress-proxy

The egress boundary for cloud-executor sandboxes whose project restricts the
network to a host allowlist. The design is ADR-0034 decision 9, "Host
allowlist: the egress proxy", and ADR-0011, "Provider, MCP and tool egress".

A NetworkPolicy cannot enforce host names. In allowlist mode, a workspace run
therefore reaches the internet only through this proxy. The sandbox gets one
setting:

```
HTTPS_PROXY=http://run:<token>@elitea-egress-proxy.<namespace>.svc:3128
HTTP_PROXY=http://run:<token>@elitea-egress-proxy.<namespace>.svc:3128
```

elitea-main signs the token at claim time
(`services/elitea-main/internal/application/egresstoken`). The token format
and the allowlist grammar are in `libs/go/egresslib/runtoken`. The address
checks are in `libs/go/egresslib/addrcheck`, which elitea-main's egress guard
also uses.

## What the proxy does per request

1. Read the token from `Proxy-Authorization: Basic`. The user is `run`, and
   the password is the token.
2. Verify the token with the public key set: the signature, the key id, the
   validity window and the size.
3. Match host and port against the allowlist in the token.
4. Resolve the host. Refuse the request when any resolved address is not
   public, or is in `ELITEA_EGRESS_PROXY_DENY_CIDRS`.
5. For a `CONNECT`, answer 200, then read the client's TLS ClientHello and
   check its server name (SNI) against the `CONNECT` host. See "The
   ClientHello check" below.
6. Dial the checked IP literal. DNS rebinding cannot change the address
   between the check and the connect.
7. Relay the bytes: a `CONNECT` tunnel, or one absolute-form `http://`
   request. The bytes that the check read go to the destination first,
   unchanged.

### The ClientHello check

The proxy does not decrypt a tunnel. The first TLS handshake message, the
ClientHello, is plaintext in every TLS version, so the proxy reads it before
it opens the upstream connection:

- The `server_name` must equal the `CONNECT` host. The comparison ignores
  ASCII case and one trailing dot on either side. Otherwise the reason is
  `sni-mismatch`.
- A ClientHello without `server_name` is refused (`sni-missing`) when the
  `CONNECT` host is a name. It is allowed when the host is an IP literal. A
  `server_name` on an IP-literal `CONNECT` must be that literal.
- A first byte that is not a TLS handshake record (0x16) is refused on port
  443 (`not-tls`). On every other allowed port, non-TLS bytes pass through
  unchanged.
- A ClientHello can span several TLS records and several TCP reads. The proxy
  reassembles it. A ClientHello that does not parse is refused
  (`client-hello-malformed`). The parser is strict on purpose, so that a
  parser difference with the destination cannot hide a name.
- The peek is bounded. The client has 10 seconds after the 200 to send the
  whole ClientHello (`sni-timeout`). The ClientHello, record headers
  included, can take at most 16 KiB (`client-hello-too-large`). A real
  ClientHello is under 2 KiB. The client must speak first on every port: a
  protocol in which the server speaks first (SMTP, for example) gets no byte
  through and is closed after the 10 seconds (`sni-timeout`). Passing such a
  silent tunnel through would let a client wait out the check and then send
  a fronted ClientHello.
- With Encrypted Client Hello (ECH), the ClientHello on the wire is the outer
  one. Its `server_name` is the public name of the ECH configuration, and
  that is the name the proxy checks. An ECH client whose public name is not
  the `CONNECT` host is refused.

The order is 200, then the check, then the dial. A client sends its
ClientHello only after the 200, and a refused tunnel never opens an upstream
connection. A refused ClientHello gets a TLS `access_denied` alert, then the
tunnel closes. Because the 200 comes before the dial, a `CONNECT` whose dial
fails closes after the 200 instead of answering 502. Its log line has outcome
`upstream-error` and reason `dial-failed`.

## Protocol support

| Supported | Not supported |
| --- | --- |
| `CONNECT host:port` for TLS. The sandbox verifies the certificate of the destination end to end. | TLS interception. The proxy never sees plaintext inside a tunnel. |
| Absolute-form `http://` requests (`GET http://host/path`). | `https://` in absolute form. Clients use `CONNECT` for https. |
| | Redirects. The proxy relays a 3xx response and never follows it. |
| | SSH, including `git` over SSH, and every other non-HTTP protocol. Use `git` over https. A refusal on port 22 says so. |
| | HTTP `Upgrade` on a forwarded request. WebSocket over `wss://` works through `CONNECT`. |

The proxy strips hop-by-hop headers in both directions, including the
headers that a `Connection` header names. `Proxy-Authorization` is one of
them, so the token never leaves the proxy.

## Allowlist grammar

| Entry | Matches |
| --- | --- |
| `example.com` | `example.com` on port 443 or 80 |
| `example.com:8443` | `example.com` on port 8443 only |
| `*.example.com` | every name under `example.com`, not `example.com` itself, on 443 or 80 |
| `*.example.com:8443` | the same, on port 8443 only |
| `203.0.113.7`, `[2001:db8::7]:8443` | that public IP literal |

- An entry has no scheme, no path, no CIDR block and no spaces.
- A wildcard needs a suffix of at least two labels. `*.com` is refused.
- A name whose last label is numeric is refused. Some resolvers read it as
  an IPv4 address.
- A request for an IP literal matches only an entry that names that address.
- A private, loopback, link-local, metadata or reserved address can never be
  an entry. A name that resolves to one is refused at request time.

## Threat model

- **A stolen token** allows egress within the allowlist of that one run,
  until the token expires. It allows nothing else. The token carries the
  execution, the project, the allowlist, the expiry and a nonce. Main sets
  the expiry to the wall time of the turn. A verifier refuses a validity
  window longer than 24 hours.
- **The sandbox cannot widen its allowlist.** The allowlist is inside the
  signed claims. The proxy verifies the signature over the received bytes
  before it decodes the claims.
- **The sandbox cannot reach internal addresses.** The proxy refuses
  loopback, RFC 1918, ULA, CGNAT, link-local, cloud metadata, multicast and
  reserved addresses, whatever the allowlist says. The chart adds the pod and
  service ranges of the cluster. The NetworkPolicy of the proxy excepts the
  same ranges from its egress, so a CNI that does not enforce policy still
  leaves the check in the proxy.
- **DNS rebinding does not work.** The proxy resolves each request once,
  checks every answer, and dials the checked literal.
- **The token stays out of logs.** No log line, error, response or forwarded
  header holds the token. The tests capture the logs and search them for
  token material.
- **The proxy does not hold secrets.** It has only public keys. It has no
  Kubernetes API access and no service-account token.

The proxy is not a content filter. A run can send any data to a host that its
allowlist names.

The proxy checks the TLS server name (SNI) of each `CONNECT` tunnel against
the `CONNECT` host, so classic domain fronting through the SNI is closed. Two
gaps remain:

- **The HTTP `Host` inside the tunnel.** It is encrypted, and the proxy does
  not intercept TLS. A shared front end (a CDN that serves many tenants on one
  address) can still route a request by its inner `Host` header to a name
  that the allowlist does not hold.
- **ECH.** With Encrypted Client Hello the proxy checks the outer SNI, which
  is the public name of the ECH configuration. The real destination is in the
  encrypted inner ClientHello. A client can reach any name behind a front end
  whose ECH public name is the allowed host.

Do not allow a shared front-end name when that matters. On a forwarded
`http://` request, the `Host` that the destination receives is the host that
the proxy checked.

## Limits

Each replica enforces these limits. A run that uses two replicas gets the
limits on each replica.

| Limit | Variable | Default | On breach |
| --- | --- | --- | --- |
| Connections per run | `ELITEA_EGRESS_PROXY_RUN_MAX_CONNECTIONS` | 64 | 429, `run-connection-limit` |
| Bytes per run, up and down together | `ELITEA_EGRESS_PROXY_RUN_MAX_BYTES` | 2 GiB | The connection closes; later requests get 403, `run-byte-limit` |
| Idle time of one connection | `ELITEA_EGRESS_PROXY_IDLE_TIMEOUT` | 5m | The connection closes (`idle-timeout`) |
| Lifetime of one connection | `ELITEA_EGRESS_PROXY_MAX_CONNECTION_LIFETIME` | 1h | The connection closes (`lifetime`) |
| Token expiry | in the token | turn wall time | The connection closes (`token-expired`); later requests get 407 |
| Client connections per replica | `ELITEA_EGRESS_PROXY_MAX_CONNECTIONS` | 4096 | 503, `proxy-connection-limit` |
| Request header block | `ELITEA_EGRESS_PROXY_MAX_HEADER_BYTES` | 32 KiB | 431; an upstream response over it gets 502 |
| Time to send the ClientHello after the 200 | fixed | 10s | The tunnel closes, `sni-timeout` |
| Size of the ClientHello, record headers included | fixed | 16 KiB | The tunnel closes, `client-hello-too-large` |

## Refusals

A refusal before the 200 has a status and the `X-Elitea-Egress-Refused`
header. The header value is one of a fixed set. The body is one short line of
fixed text.

| Status | Reason |
| --- | --- |
| 407 | `token-missing`, `token-invalid`, `token-expired` |
| 403 | `host-not-allowed`, `port-not-allowed`, `ip-literal-not-allowed`, `address-not-public`, `run-byte-limit`, `protocol-not-supported` |
| 400 | `bad-target`, `protocol-not-supported` (a request that is not a proxy request) |
| 429 | `run-connection-limit` |
| 502 | `resolve-failed`; a failed dial of an `http://` request has no reason header (a `CONNECT` closes after the 200) |
| 503 | `proxy-connection-limit`, `draining` |
| none: after the 200, the tunnel closes | `sni-mismatch`, `sni-missing`, `not-tls`, `sni-timeout`, `client-hello-too-large`, `client-hello-malformed` |

A ClientHello refusal comes after the 200, so it has no status and no header.
The client gets a TLS `access_denied` alert (none for `not-tls`), then the
tunnel closes. The reason is in the connection log line, in
`egress_proxy_refusals_total{reason}` and in the run summary's
`hosts_refused`.

## Configuration

Every variable has the prefix `ELITEA_EGRESS_PROXY_`. A value that does not
parse stops the start. The proxy never falls back to a default for it.

| Variable | Default | Meaning |
| --- | --- | --- |
| `PUBLIC_KEYS` | required | A file, or a directory of files, of PEM `PUBLIC KEY` blocks (Ed25519) |
| `KEYS_RELOAD_INTERVAL` | `30s` | How often the key set is read again. `0s` turns the reload off |
| `LISTEN_ADDR` | `:3128` | The proxy listener |
| `ADMIN_ADDR` | `:9090` | `/healthz`, `/readyz`, `/metrics` |
| `DENY_CIDRS` | empty | More ranges to refuse, comma-separated. The chart sets the cluster ranges |
| `MAX_CONNECTIONS`, `MAX_HEADER_BYTES`, `READ_HEADER_TIMEOUT`, `DIAL_TIMEOUT` | 4096, 32768, `10s`, `10s` | Global limits |
| `RUN_MAX_CONNECTIONS`, `RUN_MAX_BYTES`, `IDLE_TIMEOUT`, `MAX_CONNECTION_LIFETIME` | 64, 2147483648, `5m`, `1h` | Per-run and per-connection limits |
| `SUMMARY_INTERVAL` | `1m` | How often an active run's summary is logged |
| `DRAIN_DELAY`, `SHUTDOWN_TIMEOUT` | `5s`, `60s` | The SIGTERM drain |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

In the Helm chart, set `egressProxy.*` in `deploy/helm/elitea/values.yaml`.
It is off by default.

## Logs and metrics

Logs are JSON on stdout. Two messages are stable names:

- `egress_proxy.connection`: one line for each request. It holds `mode`,
  `execution_id`, `project_id`, `host`, `port`, `bytes_up`, `bytes_down`,
  `duration_ms`, `outcome` and, for a refusal, `reason`.
- `egress_proxy.run_summary`: the hosts that one run reached and the hosts
  that the proxy refused for it, with counts and bytes. The proxy logs it on
  `SUMMARY_INTERVAL` while the run is active, and with `final: true` after the
  run's token expires. At shutdown it logs one with `replica_shutdown: true`
  for each run that changed since its last summary.
  Each replica logs its own summary; a consumer merges them by
  `execution_id`. Phase B forwards these lines to Main's audit log.

`/metrics` on the admin port serves Prometheus text:
`egress_proxy_connections_total{outcome}`, `egress_proxy_refusals_total{reason}`,
`egress_proxy_bytes_total{direction}`, `egress_proxy_client_connections`,
`egress_proxy_active_upstreams`, `egress_proxy_runs`, `egress_proxy_token_keys`
and `egress_proxy_key_reload_errors_total`.

## Operating notes

### Creating the key pair

The run tokens are signed by elitea-main with a dedicated Ed25519 key. The
private half goes to elitea-main only; the public half goes to the proxy.
They are two Secrets, so the proxy never holds the private key.

```bash
# OpenSSL 3 (macOS's /usr/bin/openssl is LibreSSL and has no ed25519 genpkey:
# use Homebrew's openssl@3).
openssl genpkey -algorithm ed25519 -out signing.pem
openssl pkey -in signing.pem -pubout -out signing.pub.pem

kubectl -n <release namespace> create secret generic elitea-egress-token-signing \
  --from-file=signing.pem=signing.pem
kubectl -n <release namespace> create secret generic elitea-egress-proxy-keys \
  --from-file=2026-10.pem=signing.pub.pem
shred -u signing.pem   # keep no copy outside the Secret
```

Then set, in the chart values:

```yaml
egressProxy:
  enabled: true
  signingKey:
    existingSecret: elitea-egress-token-signing   # mounted into elitea-main only
  publicKeys:
    existingSecret: elitea-egress-proxy-keys       # mounted into the proxy
```

(or paste the public PEM into `egressProxy.publicKeys.keys`). The chart
refuses `egressProxy.enabled` without a signing key Secret, a signing key
without the proxy, and the same Secret for both. At start, elitea-main logs
`workspace egress proxy enabled` with the `key_id`; it must be one of the
`key_ids` the proxy logs in `egress_proxy.started`.

elitea-main reads `ELITEA_EGRESS_PROXY_URL` (the chart sets
`http://elitea-egress-proxy.<namespace>.svc.cluster.local:3128`) and
`ELITEA_EGRESS_TOKEN_SIGNING_KEY_FILE`. With either missing, a cloud turn
whose project has a host allowlist is refused with
`workspace_run.egress_proxy_unavailable` (reason
`egress_proxy_not_enabled` or `egress_token_signing_key_not_configured`);
it is never widened to direct egress.

### Key rotation

The key id is derived from the public key, so no id is configured. To rotate:

1. Make a new key pair: `openssl genpkey -algorithm ed25519 -out next.pem`,
   then `openssl pkey -in next.pem -pubout -out next.pub.pem`.
2. Add `next.pub.pem` to the proxy key set (`egressProxy.publicKeys`). Keep
   the old public key. Each replica reads the Secret again within
   `KEYS_RELOAD_INTERVAL` and logs `egress_proxy.keys_changed`. Check that
   `egress_proxy_token_keys` is 2 on every replica.
3. Replace the `signing.pem` data of elitea-main's signing-key Secret
   (`egressProxy.signingKey.existingSecret`) with `next.pem` and roll
   elitea-main.
4. Wait for the longest token that the old key signed to expire: the longest
   turn wall time, and never more than 24 hours.
5. Remove the old public key from the set.

A key reload that fails keeps the last good set and increments
`egress_proxy_key_reload_errors_total`. Alert on that counter.

### Drain

On SIGTERM, `/readyz` fails at once, and the proxy keeps accepting for
`DRAIN_DELAY`. Then it stops accepting and waits up to `SHUTDOWN_TIMEOUT` for
open tunnels. It closes the rest and logs their outcome as `shutdown`. The
chart refuses a `terminationGracePeriodSeconds` shorter than the drain.

### Network shape

- The sandbox in allowlist mode reaches only the proxy port. It has no direct
  egress and no DNS (phase B ships that sandbox policy).
- The proxy accepts on its proxy port only from the workspace-run pods in the
  execution namespace. It accepts on the admin port only from
  `egressProxy.networkPolicy.adminFrom`.
- The proxy reaches the public internet minus the deny ranges, and DNS at the
  cluster resolver.

## Tests

```bash
go test -race ./services/elitea-egress-proxy/... ./libs/go/egresslib/...
bash deploy/helm/tests/render-egress-proxy.sh
```

The integration tests use real listeners, a fake resolver, and a dialer that
routes test addresses to local servers and records every dial.
