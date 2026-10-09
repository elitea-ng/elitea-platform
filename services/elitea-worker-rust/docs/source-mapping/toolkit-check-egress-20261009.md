# Toolkit connection check through the egress guard (2026-10-09)

Branch `fix/toolkit-check-egress`, stacked on `fix/main-hardening-residuals` (#1192) because it uses that PR's
per-entry private egress permits (SEC-10). Component: Go Main (`services/elitea-main`), plus the e2e compose file, a
Helm values comment and the egress operator notes. The Worker is not changed; this file lives here per the
source-mapping rule.

Hardens SEC-13 (follow-up F10). Finding details are kept out of this repository.

## Business behaviour

Kept from the current platform: the credential card's "Test connection" (single, batch and stored checks) answers the
same four reasons (`ok`, `auth_failed`, `unreachable`, `unsupported_type`) with the same fixed messages, for the same
provider families and authentication methods. `ELITEA_TOOLKIT_CHECK_ALLOWLIST` keeps its host-name grammar and its
default list of public vendor hosts.

Changed on purpose:

- The probe dials through `internal/infra/egress`: the host is resolved and classified at dial time and the checked
  IP literal is dialled, no proxy is used, redirects are not followed, response headers are capped at 64 KiB and the
  body is closed unread. Before, it used the default transport once the host name was on the list.
- A loopback or private-network address passes only when a CIDR block, IP literal or `localhost` entry of the same
  variable names it, each entry for its own range (the SEC-10 semantics). Link-local, metadata, multicast and
  reserved addresses are always refused. A self-hosted provider on a private address needs its name and its range.
- An IP-literal base URL inside a named CIDR/IP entry passes the host-name bound.
- A guard refusal answers the existing configuration message ("This provider endpoint is not permitted by the
  platform's configuration."), not "unreachable".

**Decision recorded: the bare `*` is removed, not gated.** With the dial-time guard, `*` could only lift the host-name
bound; it could never permit a private destination, and nothing in `deploy/` or the e2e stacks used it. Rather than
keep it behind a dev-only flag, the parser drops it: a value of only `*` refuses every host (fail closed), and `*`
beside other entries has no effect. No production render can enable it.

## Changed paths and enforcing code

| Item | Enforcing code |
| --- | --- |
| Guarded transport, no redirects, body unread | `services/elitea-main/internal/api/v2/configurations/toolkit_check.go:552-565`, `:663` |
| One list → host bound + private permits; `*` dropped | `toolkit_check.go:578-584` (`parseToolkitCheckAllowlist`) |
| Outer bound incl. IP literal inside a named block; empty-list guard | `toolkit_check.go:594-600` (`allowsHost`), `:625` |
| Guard refusal → configuration message | `toolkit_check.go:652` |
| Per-entry private permits, dial-time classification | `internal/infra/egress/classify.go` (`privatePermits`), `guard.go` (`permittedIPs`) — from #1192 |
| e2e stack | `deploy/docker-compose.e2e-standalone.yml` names Docker's private pools beside `elitea-main, elitea-web` |
| Operator docs | `deploy/helm/elitea/values.yaml` (variable comment), `egress-guard-hardening-20261008.md` ("Upgrade notes for operators": toolkit checks, per-entry permits) |

## Tests

| Suite | Result |
| --- | --- |
| `go test ./internal/api/v2/configurations/` (with PostgreSQL) | ok (all existing checker tests now name their loopback provider `127.0.0.1`, as an operator would) |
| `go test ./internal/api/ ./cmd/... ./tests/deployedge/ ./internal/infra/egress/` | ok |

New proving tests in `internal/api/v2/configurations/toolkit_check_egress_test.go` (5 tests, 0 skips):

- `TestToolkitCheckRefusesEveryBlockedAddressClass` — 24 cases, an allowlisted NAME resolving to: IPv4 and IPv6
  loopback, IPv4-mapped and NAT64 loopback, RFC 1918, ULA, CGNAT, benchmarking, IPv4-mapped and 6to4 RFC 1918; and,
  even with wide ranges named: unspecified, cloud metadata (plain, IPv4-mapped, NAT64, 6to4), metadata inside CGNAT and
  inside ULA, the platform endpoint, multicast, reserved, IPv6 link-local; loopback with only other private ranges
  named. Each is refused with the configuration message, decided before any connect.
- `TestToolkitCheckPrivateProviderNeedsItsRangeNamed` — name + range, name + address, name + address on this port, IP
  literal inside a named block pass and reach the provider once; name only, name + another range, name + address on
  another port, range without the name, bare `*`, `*` beside a range and the default list are refused with no request.
- `TestToolkitCheckIgnoresProxyEnvironment` — all six proxy variables set to a live proxy: the provider is reached
  directly, a refused target is not handed to the proxy, the proxy sees nothing.
- `TestToolkitCheckDoesNotFollowRedirects` — a 302 is the answer; the Location host never sees the request.
- `TestToolkitCheckBoundsTheResponse` — headers over 64 KiB are refused; a 64 MiB body is not read (answer from the
  status line in well under 2 s, the provider never finishes writing).

Mutation check: with `http.DefaultTransport` in place of the guard, four of the five fail (the redirect test is also
held by the client's existing no-follow rule).

## Performance

One guard per checker (built once at handler construction); per probe the cost is the guard's resolution (bounded to
32 answers, 5 s) and a single connect, as before. The body is never read, so a large answer costs nothing beyond the
capped header block. The probe timeout (5 s) is unchanged.

## Durability

Read-only check; no state is written. A refused check stores nothing.

## Resilience

Typed, readable answers from the existing closed vocabulary; refusals never carry an address, URL or credential.
Fail closed: an empty private list permits no private address (`Allows` is consulted only for a configured list), a
lone `*` refuses every host, an unparsable entry permits nothing.

## Security

| `rules/security.md` category | Applies | How checked |
| --- | --- | --- |
| Egress and SSRF | Yes | Blocked-class matrix incl. IPv4-mapped, NAT64, 6to4, metadata, CGNAT; no redirects; proxy variables ignored; DNS answers classified at dial time (fake resolver); per-entry private permits |
| Input, parsing and amplification | Yes | Response header cap; body unread; scheme limited to http/https; userinfo and fragment dropped (unchanged) |
| Secrets | Checked | Refusal messages carry no credential or URL; diff secret scan before commit: clean |
| Trust boundaries, authorization, injection | Not changed | Routes and their authorization are untouched |
| Supply chain | Checked | No dependency change |

`security-review` of the commit: no findings. Notes from the review, both fail-closed or documented: a host entry
carrying a port (`host:8443`) never matches the host-name bound; names and ranges are not paired (the documented
"name AND range" model).

## Recovery guarantees

| Component × phase | Class | Evidence |
| --- | --- | --- |
| Main × credential check (read-only external call) | F | Refused before connect with the configuration message; nothing written. `toolkit_check_egress_test.go` |

No L rows.

## Real-browser evidence

Own standalone stack `elitea-mh` at `mh.localhost:18420` (`STANDALONE_HOST`), real-model dump
`product-real-models-main-c0f2e5f9b.dump`, OIDC login as `admin@centry.user`. `elitea-main` built from this branch at
d5a300704 (`ghcr.io/elitea-ng/elitea-main:mh-d5a300704`, image `sha256:c49ca1304a44…`; the extracted binary contains
the guard package and this stack's other branch strings); other services merged-main `main-c0f2e5f9b-verify` images.
Main ran with `ELITEA_TOOLKIT_CHECK_ALLOWLIST` = the default public hosts plus `elitea-web, llm-mock, 172.24.0.20`
(`elitea-web`'s address only). No response mocks; the stack was torn down afterwards.

- Public: Credentials › `github_secret` (base `https://api.github.com`) › Test connection → "Connection successful"
  (`check_stored_connection/2/2` → `{"reason":"ok","success":true}`).
- Private, range not named: New credential › Confluence, base `http://llm-mock:8090` (named, its address not) › Test
  connection → 400 "This provider endpoint is not permitted by the platform's configuration." (`reason: unreachable`).
- Private, allowlisted: base `http://elitea-web/app/x` (named and its address listed) → 200 "Connection successful".
- After a reload: the stored GitHub check is `ok` again; IP-literal `http://172.24.0.20/app/x` → ok;
  `http://172.24.0.12:8090` (unlisted address) → the configuration refusal.

## Fixtures

Unit tests use httptest providers and a fake resolver. Browser: the dump's existing GitHub credential; the Confluence
credential was filled in the UI with placeholder values and never saved; nothing was written to the database directly.

## Follow-ups

- A host entry with a port (`gitlab.corp.example:8443`) does not match the host-name bound today (fails closed); the
  docs tell operators to name the host without a port.
