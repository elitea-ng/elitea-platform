# Main egress guard hardening

Date: 2026-10-08. Track: Point 5 Wave 1, M1
(`handoffs/point5-wave1-20261008/T-M1-egress-hardening.md`). Base: `origin/main` `fcf86c31`.
Branch: `fix/main-egress-guard-hardening`. The change is Main-only. No Worker, Web, contract, CI or dependency file changes.

## Problem and behavior

Main's DNS-aware destination guard (#876) already re-resolved the host on every dial and dialled the checked IP
literal, which closes DNS rebinding. It still had four gaps (Point 5 expert finding F5):

1. Some address classes were not refused: CGNAT (which includes the Alibaba metadata address 100.100.100.200),
   `0.0.0.0/8`, `192.0.0.0/24`, `198.18.0.0/15`, `240.0.0.0/4`, and NAT64 `64:ff9b::/96`.
2. The transport inherited `ProxyFromEnvironment`. With `HTTP(S)_PROXY` set, the guard would check and dial the
   proxy's address, and the tenant's target would never be checked.
3. Callers could wrap the transport in a redirect-following `http.Client`. Webhook delivery did exactly that:
   a 307 or 308 re-sent the signed body to an unregistered `Location`.
4. The guard lived in `internal/api/webhook`, so `internal/infra/storage` kept its own inline copy with the same
   address gaps.

After this change, the guard in `internal/infra/egress` is the one dial-time gate for every tenant-chosen
destination in Main:

- **Forbidden whatever the allowlist says:** unspecified, link-local, multicast, `0.0.0.0/8`, `192.0.0.0/24`,
  `240.0.0.0/4` (including broadcast), local-use NAT64 `64:ff9b:1::/48`, discard-only `100::/64`, Teredo
  `2001::/32`, IPv4-compatible `::/96`, and the cloud metadata endpoints 100.100.100.200 (Alibaba),
  168.63.129.16 (Azure) and `fd00:ec2::254` (AWS IPv6).
- **Private, so liftable only when the allowlist declares private egress:** loopback, RFC 1918, ULA, CGNAT,
  benchmarking and deprecated site-local `fec0::/10`.
- **IPv4 reached through IPv6** is classified as the IPv4 address it reaches. This covers IPv4-mapped forms,
  NAT64 `64:ff9b::/96` and 6to4 `2002::/16`.
- **Transport:**
  - `Proxy = nil`;
  - `MaxResponseHeaderBytes = 64 KiB`;
  - at most 32 DNS answers;
  - zoned answers dropped;
  - TCP only.
- **TLS** is verified against the URL host, which is net/http's default. The pinned IP literal is only the TCP
  address.
- **`RoundTripper()`** is the transport-only constructor for new egress paths. A 3xx goes back to the caller and
  is never followed. The optional `RequireHTTPS()` policy refuses `http` in `Validate` and in the transport itself:
  net/http consults `Proxy` before every dial, so an HTTPS-only guard installs a `Proxy` function that checks the
  scheme and never returns a proxy. A caller holding the concrete `*http.Transport` cannot bypass the policy.
- **Refusals** carry at most the host. They never echo userinfo, path or query.

## Upgrade notes for operators

Three behaviour changes can reach a running deployment after upgrading past #1149. None is configured from the
Admin UI: the Admin **Egress Allowlist** governs only the LLM gateway's provider endpoints. Main's outbound paths
are configured in the chart or the Main config.

| Outbound path | Setting |
|---|---|
| Webhook delivery | `ELITEA_WEBHOOK_EGRESS_ALLOWLIST` (Main environment) |
| MCP Load Tools and metadata reads | `ELITEA_MCP_EGRESS_ALLOWLIST` (`deploy/helm/elitea/values.yaml`) |
| MCP OAuth and DCR proxies | `ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST` (`deploy/helm/elitea/values.yaml`) |
| Code workspace GitHub reader | `egress_allowlist` in the Code workspace config (exact `host:port` entries) |

### CGNAT and benchmarking addresses are now private

**What changed.** `100.64.0.0/10` (CGNAT) and `198.18.0.0/15` (benchmarking) used to be treated as public. They are
now private, like RFC 1918. Tailscale, some EKS custom-networking setups and some lab networks use these ranges.

**Who is affected.** A deployment whose webhook receiver, MCP server, identity provider or GitHub Enterprise host
resolves into one of these ranges, and whose allowlist for that path is empty or names only public hosts. If the
allowlist already names any private range (for example `10.0.0.0/8`), nothing changes: private egress is already
declared for that path.

**Symptom.** Creating the webhook, loading MCP tools or reading the repository fails with
`does not resolve to a permitted destination (loopback, private-network, link-local, multicast and reserved
addresses are refused)`. Webhooks that already exist fail at delivery with the same refusal.

**Fix.**
1. Add the narrowest block that covers the target to the setting for that path: for example `100.100.4.0/24`, or
   the whole `100.64.0.0/10` for Tailscale. For the Code workspace reader, add the server's address as an extra
   exact `IP:port` entry. A host name alone does not declare private egress, because the decision is made on
   resolved addresses.
2. Restart Main. These settings are read at startup.

**Know before you add the entry.** A private entry currently switches private egress on for the whole path: once
any private block is named, every private class (RFC 1918, loopback, ULA, CGNAT, benchmarking) is permitted for
that path, not only the named block. So add it only for paths that need it, and keep Main's network policy
default-deny. Metadata endpoints, link-local, multicast and reserved addresses stay refused whatever the entry
says. Narrowing a private entry to its own block is listed under follow-ups.

### Webhooks no longer follow redirects

A receiver that answers 3xx (for example an `http://` URL that redirects to `https://`, or a trailing-slash
redirect) used to be followed and counted as delivered. Delivery now fails after one attempt. **Fix:** register
the receiver at its final URL.

### No outbound proxy on these paths

Main ignores `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` for the four paths above, and dials the destination
directly. The shipped charts set no proxy, so they are not affected. A site where every outbound connection
**must** go through a forward proxy cannot reach public webhook receivers or MCP servers from Main until guarded
proxy support exists (listed under follow-ups).

## Business behavior and what was not ported

The webhook, MCP and Code workspace behaviours that users see are unchanged for permitted public destinations:

- registration;
- signed POST delivery;
- three attempts with backoff;
- the delivery log;
- MCP Load Tools, OAuth and DCR proxies;
- GitHub reads.

The current platform (`centry/pylon_main/plugins/elitea_core/utils/mcp_oauth.py:110,180,258`) sends outbound
requests with plain `requests.post`. That client:
- follows redirects;
- honours environment proxies;
- performs no address check.

That is the "per-actor ad-hoc egress" defect, and none of it was ported.

Two deliberate behaviour changes affect users:

| Change | Before | After | Reason |
| --- | --- | --- | --- |
| Webhook delivery and a 3xx | Followed up to 10 redirects. A 3xx counted as success. | Not followed. Only 2xx is success. A 3xx is logged as `failed` with its code after one attempt: it answers the same way on every attempt, so it is terminal like a refusal. A receiver that redirects (for example an http→https 301) must be re-registered at its final URL. | Following a 307/308 re-sends the signed body to a Location the tenant never registered. |
| CGNAT, benchmarking and site-local destinations | Allowed by default. | Refused unless the allowlist declares private egress. An entry naming a CGNAT or benchmarking address or block (for example `100.64.0.0/10`) now counts as that declaration. | They are internal ranges (Tailscale, carrier NAT, labs), and CGNAT contains a metadata endpoint. |

The webhook refusal text keeps its existing prefix, `webhook destination refused: "<host>" does not resolve to a
permitted destination (…)`. The list in parentheses now reads "loopback, private-network, link-local, multicast
and reserved addresses are refused".

## Changed paths

Paths are relative to `services/elitea-main/`.

| Path | Change |
| --- | --- |
| `internal/infra/egress/guard.go` | Moved from `internal/api/webhook/ssrf.go` (commit `037ab73e`, a pure move). Adds `Proxy = nil`, the HTTPS-only `Proxy` scheme check, the header cap, and a fallback when another package has wrapped `http.DefaultTransport` (`Transport`, `:157-184`), `RoundTripper` (`:190`), the TCP-only dial (`:206-211`), the 32-answer bound and the zone drop (`:268-294`), `checkScheme` (`:296`), and redaction of unparseable URLs (`parseDestinationURL`, `:313`). |
| `internal/infra/egress/classify.go` | New. `forbiddenBlocks` (`:24`), `privateBlocks` (`:41`), `classify` (`:76`), NAT64/6to4 `embeddedIPv4` (`:98`), `declaresPrivateEgress` (`:117`). |
| `internal/api/webhook/ssrf.go` | Thin aliases (`DestinationGuard = egress.Guard`, the same sentinel). Every caller is unchanged. |
| `internal/api/webhook/dispatcher.go` | `CheckRedirect` returns `http.ErrUseLastResponse` (`:211`). Success is 2xx only (`:337`). A 3xx is terminal after one attempt (`:347`). |
| `internal/api/v2/eliteacore/mcp_oauth_egress.go` | The cloned OAuth/DCR transport drops `Proxy` and caps headers (`:111-112`). |
| `internal/infra/storage/code_workspace_github.go` | The inline dialler is replaced by `egress.New(allowed).Transport()` (`:73`). |
| `deploy/helm/elitea/values.yaml` | The MCP allowlist comments now describe the private and forbidden classes and the no-proxy rule. |

### Callers found (task 4)

Every tenant-chosen destination that reaches the guard:

| Caller | Allowlist (outer bound) | Guard path | Redirects |
| --- | --- | --- | --- |
| Webhook create/update `handler.go` `validateDestination` | `ELITEA_WEBHOOK_EGRESS_ALLOWLIST` | `Validate` (400 on refusal) | n/a |
| Webhook delivery `dispatcher.go` | same | `Transport()` dial | refused (new) |
| MCP Load Tools, metadata, discovery `eliteacore/handler.go:255`, `:5248` | `ELITEA_MCP_EGRESS_ALLOWLIST` | `Transport()` dial | same origin only (unchanged) |
| MCP OAuth/DCR proxies `eliteacore/mcp_oauth_egress.go` | `ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST` | `Validate` plus `DialContext` on a cloned transport | same origin only (unchanged) |
| Code workspace GitHub `infra/storage/code_workspace_github.go` | Code workspace exact-host allowlist (`runtimecomposition/code_consumers_config.go:136`) | `Transport()` dial (new) | refused (unchanged) |

Other `egresslib` users only parse or validate entries:
- `api/gateway/governance.go:296`;
- `runtimecomposition/code_consumers_config.go`;
- the LLM gateway service, which dials through bifrost's own SSRF-safe dialer and is out of scope.

`ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST` does not exist yet. HTTP actions are not wired in production; the only
constructor is `infra/storage/runtime_http_action.go:55`, called only by tests. Wave 2 (design D5) wires them
through `RoundTripper()` and adds that variable as their outer bound.

## Tests

Every new test was written first and failed for the stated reason before the fix. The red output was captured
during the session:
- the class table failed for 36 of its 49 cases;
- "guarded transport has a Proxy function";
- `MaxResponseHeaderBytes = 0`;
- `Validate(http) = <nil>`;
- "33 answers = <nil>";
- the webhook redirect target "received 1 deliveries";
- the MCP transport "keeps a Proxy function";
- Code workspace "dial 100.100.100.200 = code workspace content is unavailable" (it dialled);
- the refusal echoed `s3cr3t-token`.

| Test | Proves |
| --- | --- |
| `egress/hardening_test.go` `TestGuardAddressClasses` | 49 addresses × {no allowlist, private allowlist}: every class as IPv4, IPv4-mapped and NAT64, with the first and last address of each range and the neighbours just outside it (limit and limit+1). |
| `TestGuardAllowlistNamingANewPrivateClassDeclaresPrivateEgress` | A CGNAT or benchmarking entry lifts the class. A public-only allowlist does not. An entry naming metadata or reserved space grants nothing. |
| `TestGuardTransportIgnoresProxyEnvironment` | With `HTTP_PROXY`/`HTTPS_PROXY` set, the proxy gets 0 hits and the target 1. |
| `TestGuardRoundTripperReturnsRedirectUnfollowed` | A 307 is returned and the Location gets 0 hits. |
| `TestGuardTransportCapsResponseHeaders` | A 32 KiB header passes; a 128 KiB header is refused. |
| `TestGuardRequireHTTPSRefusesPlainHTTP` | `Validate` and `RoundTrip` refuse `http` before any connection. The default policy keeps `http`. |
| `TestGuardRequireHTTPSHoldsOnTheRawTransport` | `Transport().RoundTrip` on an HTTPS-only guard refuses `http`, with 0 target hits and 0 proxy hits while `HTTP(S)_PROXY` is set. |
| `TestGuardRoundTripperRefusesANilURL` | A request with a nil URL returns an error instead of panicking. |
| `TestGuardTransportSurvivesAReplacedDefaultTransport` | With `http.DefaultTransport` wrapped by another package, construction does not panic and the transport stays hardened. |
| `TestGuardVerifiesTLSAgainstTheURLHost` | A certificate for `example.com` passes; `wrong.example`, dialled to the same IP, fails with `x509.HostnameError`. |
| `TestGuardBoundsResolution` | 32 answers pass and 33 are refused. Zoned answers are refused. A `udp` dial is refused. |
| `TestGuardRefusalsNeverEchoURLSecrets` | Five refusal paths never contain URL userinfo, path or query secrets. |
| `TestGuardFilterBudget`, `BenchmarkGuardPermittedIPs` | `classify` allocates 0 times. Filtering 32 answers allocates once. |
| `TestGuardRoundTripperReleasesIdleConnections` | `http.Client.CloseIdleConnections` reaches the guarded transport. |
| `webhook/dispatcher_test.go` `TestDispatcherDoesNotFollowRedirects` | The signed body is not re-sent, and a 307 is logged as failed with code 307 after exactly one attempt. |
| `eliteacore/mcp_oauth_egress_test.go` | A base transport with `ProxyFromEnvironment` loses its proxy and gains the header cap. |
| `storage/code_workspace_github_egress_test.go` | `0.0.0.1`, `192.0.0.8`, `240.0.0.1`, `100.100.100.200` and `64:ff9b::a9fe:a9fe` are refused before connect. No proxy, and the cap is set. |

The tests that moved from the old webhook `ssrf_test.go` (4) still pass in `egress/guard_test.go`, and every
webhook, MCP and Code workspace test is unchanged.

Command results:

- `go test -race -count=1 -v` with real PostgreSQL 18 (a throwaway `postgres:18` container,
  `ELITEA_TEST_DATABASE_URL`):

  | Package | Top-level tests passed | Skipped |
  | --- | --- | --- |
  | `internal/infra/egress` | 18 | 0 |
  | `internal/api/webhook` | 24 (including the PostgreSQL tests `TestWebhookDestinationIsRefusedAtCreate` and `TestWebhookSmuggledIntoTableIsRefusedAtDialAndLoggedBlocked`) | 0 |
  | `internal/api/v2/eliteacore` | 357 | 0 |
  | `internal/infra/storage` | 280 | 0 |
  | **Total** | **679**, plus 858 subtests | **0** |

- `go test -race -count=1` passes for those packages and for `internal/api`, `cmd/...` and
  `internal/runtimecomposition` (12 packages).
- `go test -count=1 ./...` for Main: 186 packages ok, 22 without test files, 0 failures. It ran without
  `ELITEA_TEST_DATABASE_URL`, so the PostgreSQL tests outside the four packages above skip themselves as before.
- `go vet ./...` is clean.
- `gofmt -l` is clean for every changed file. Five pre-existing unformatted test files elsewhere in Main are
  untouched.

## Performance

The budget is that the guard adds no round trip, no database write and no unbounded memory to an egress
request. Per new connection it costs one DNS resolution (unchanged), one connect per permitted address (unchanged),
and filtering at most 32 answers.

- **Measured:** `BenchmarkGuardPermittedIPs` filters 32 answers in 4.4–5.6 µs/op, 896 B/op, 1 alloc/op (3 runs on
  an arm64 Mac). That is below 0.1% of a typical DNS lookup.
- **Enforced:** `TestGuardFilterBudget` keeps `classify` at 0 allocations and filtering at 1 or fewer allocations.
  `MaxResolvedAddresses = 32` bounds the dial loop (`guard.go:67`, `:284`).
- **Memory:** `MaxResponseHeaderBytes = 64 KiB` (`guard.go:64`) replaces net/http's 10 MiB default per response.
- **Webhooks:** a 3xx now costs one request and no backoff, instead of up to 10 redirect hops on each of up to
  three attempts.

## Durability

Not applicable to state: the guard keeps no durable state, creates no new crash window and changes no
transaction. A refusal is decided again from DNS on every dial.

What changes is when a webhook delivery is logged:
- a 3xx is now `failed`, with its response code, through the existing delivery log;
- a refusal is still logged as `blocked` without retry. This is proven against real PostgreSQL by
  `TestWebhookSmuggledIntoTableIsRefusedAtDialAndLoggedBlocked`.

## Resilience

| Bound | Mechanism | Proof |
| --- | --- | --- |
| DNS time | `dnsLookupTimeout` 5 s (`guard.go`, unchanged) | existing |
| DNS answers | `MaxResolvedAddresses` 32; more is a typed refusal | `TestGuardBoundsResolution` (limit / limit+1) |
| Connect time | `dialTimeout` 10 s per address (unchanged) | existing |
| Response headers | `MaxResponseHeaderBytes` 64 KiB | `TestGuardTransportCapsResponseHeaders` |
| Redirects | `RoundTripper` never follows; webhook `CheckRedirect` returns the 3xx, which is terminal | `TestGuardRoundTripperReturnsRedirectUnfollowed`, `TestDispatcherDoesNotFollowRedirects` |
| Construction | No unchecked `http.DefaultTransport` assertion; a nil request URL is an error, not a panic | `TestGuardTransportSurvivesAReplacedDefaultTransport`, `TestGuardRoundTripperRefusesANilURL` |
| Network | TCP only | `TestGuardBoundsResolution` |
| Typed failures | Every refusal wraps `egress.ErrDestinationRefused` (the same value as `webhook.ErrDestinationRefused`) | all refusal tests use `errors.Is` |

Cancellation and deadlines keep their identity: the guard passes the caller's context to the resolver and the
dialer, and never wraps `context.Canceled` as a refusal.

## Security

| Threat | Mechanism | Proof |
| --- | --- | --- |
| SSRF to metadata or reserved space | `classify` and `forbiddenBlocks` (`classify.go:24`, `:76`) | `TestGuardAddressClasses`; browser case 1 |
| IPv6 wrappers of internal IPv4 | `embeddedIPv4` (`classify.go:98`); IPv4-mapped forms are matched natively | the NAT64, 6to4 and mapped rows of the table; browser case 2 |
| DNS rebinding | Re-resolve and dial the checked IP literal (unchanged) | existing `TestDestinationGuardDialsThePinnedIPNotTheHostname`, the PostgreSQL dial-time test |
| Proxy bypass | `Proxy = nil` (`guard.go:172`, `mcp_oauth_egress.go:111`) | `TestGuardTransportIgnoresProxyEnvironment`, the MCP test |
| Redirect to an unvetted host | `RoundTripper` (`guard.go:190`); webhook `CheckRedirect` | the redirect tests |
| Downgrade to http, for policies that require HTTPS | `RequireHTTPS` / `checkScheme`, enforced by the transport's `Proxy` hook (`guard.go:98`, `:176`, `:296`) | `TestGuardRequireHTTPSRefusesPlainHTTP`, `TestGuardRequireHTTPSHoldsOnTheRawTransport` |
| TLS bypass from IP pinning | net/http `ServerName` from the URL host, with no `InsecureSkipVerify` | `TestGuardVerifiesTLSAgainstTheURLHost` |
| Secrets in logs, delivery log and errors | Refusals carry the host only (`parseDestinationURL`) | `TestGuardRefusalsNeverEchoURLSecrets` |
| Operator allowlist widened by a metadata entry | `declaresPrivateEgress` ignores entries inside forbidden space | `TestGuardAllowlistNamingANewPrivateClassDeclaresPrivateEgress` |
| Layering | The guard lives in `infra/egress`; `infra/storage` no longer keeps a copy | build; Code workspace test |

Supply chain:
- no new dependency; `go.mod` and `go.sum` are untouched;
- `libs/go/egresslib` is unchanged, so the LLM gateway's bifrost semantics are untouched.

`~/go/bin/govulncheck ./...` (Main) reports 7 called Go standard-library vulnerabilities for the local toolchain
go1.26.5:
- GO-2026-5026;
- GO-2026-5972;
- GO-2026-6088 to 6091;
- GO-2026-6218.

All are fixed in go1.26.6. **origin/main reports the identical set, so no finding is new** (rerun after the
review fixes: same set).

Authorization is unchanged: the guard runs after the existing route permission checks.

## Recovery guarantees

| Component × phase | Class | Enforcing code | Proof |
| --- | --- | --- | --- |
| Main × admission (webhook create/update) | F — typed 400, nothing stored | `webhook/handler.go` `validateDestination` → `egress.Guard.Validate` | `TestWebhookDestinationIsRefusedAtCreate` (PostgreSQL); browser cases 1–2 |
| Main × effectful outbound call (webhook delivery) | F for refusals and 3xx (typed `blocked` or `failed` row). Main loss during delivery is a pre-existing **L** gap: delivery runs on an in-memory goroutine, and recovery is the manual Redeliver. This PR leaves it unchanged. | `dispatcher.go` `attempt`/`send` | `TestDispatcherDoesNotFollowRedirects`, `TestWebhookSmuggledIntoTableIsRefusedAtDialAndLoggedBlocked` |
| Main × read-only tool call (MCP Load Tools, OAuth/DCR proxy) | I — read-only; the user retries | `eliteacore/handler.go`, `mcp_oauth_egress.go` | eliteacore suite (357) |
| Main × Code preparation (GitHub read) | I — read-only fetch, repeated on retry; refusal is the typed `ErrCodeWorkspaceUnavailable` | `code_workspace_github.go:73` | storage suite (280), Code workspace egress test |

No other component (Worker, Sandbox supervisor, NATS, PostgreSQL, LLM gateway, Web) is touched.

## Reviews

`code-review` (high effort) on the full branch diff reported 8 findings:

| Finding | Disposition |
| --- | --- |
| A webhook receiver that redirects (for example an http→https 301) now always fails | Kept, as the deliberate change recorded above. Reviewer sign-off is requested in the PR. |
| A 3xx was retried like a transient error | Fixed: a 3xx is terminal after one attempt (`dispatcher.go:347`). |
| `RequireHTTPS` could be bypassed through `Transport()` | Fixed: the transport enforces it through its `Proxy` hook (`guard.go:176`). |
| `RoundTrip` panicked on a nil `req.URL` | Fixed: the wrapper was removed and net/http returns an error. |
| The sentinel text still says "webhook" | Kept: it is the existing readable error that the webhook form shows. |
| Main keeps a second private-range table next to `egresslib` | Kept: `egresslib.privateBlocks` mirrors bifrost's dialer for the LLM gateway, and widening it would widen gateway egress. Main's extra ranges live in `classify.go`. |
| `Guard.allowlist` was dead state | Fixed: removed. |
| Unchecked `http.DefaultTransport` type assertion | Fixed: falls back to net/http's documented defaults. |

`security-review` on the branch diff found no vulnerability at confidence 8 or above. It checked and rejected 16
candidates: IPv4-mapped, NAT64, 6to4, IPv4-compatible, zone handling, numeric hosts, rebinding, allowlist
widening, the HTTPS-only `Proxy` hook, the checks removed from the Code workspace dialler, TLS, redirects, and
error-message leakage.

Its one theoretical note is that SIIT `::ffff:0:a.b.c.d` is classed public. It reaches IPv4 only through a
translator on the destination network, and the old code was the same. It is listed in the follow-ups below.

The `.claude/rules/security.md` checklist:
- no hardcoded secrets;
- the inputs that matter (URLs and resolved addresses) are validated;
- no SQL is touched;
- no HTML rendering is touched;
- error messages carry the host only.

## Real-browser evidence

- **Stack:** the local NATS candidate (`http://localhost:18094`, candidate `a5994fa609c2`), signed-in project
  "Private" (id 2), page Settings → Webhooks.
- **Image identity:**
  - Container `elitea-nats-candidate-main-current-a5994fa609c2`, image
    `elitea-main:current-nats-efa7213e803d-hotfix1140-20261008`.
  - For the run, only `/elitea-main` was replaced (`docker cp`; the user approved swap-then-restore). The binary
    was built from `efa7213e` (the candidate's source) plus `024e6881` (#1140 hotfix) plus this branch's three
    commits. The rehearsal tree head was `a7859bac`, built with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`, matching
    the original's x86-64 binary.
  - Deployed sha256: `316176d225c0f6885af1d11412e6d512ccc3f1639fc17b37cb6a33e735595b6a`.
  - Original sha256 (restored afterwards and verified, container healthy):
    `3eb74b74d1d81e67285a59f4cd9e5a3dad13723589b7aaca8714110dd46392a5`.
- **Cases**, all with real backend responses and no mocks:
  1. `http://100.100.100.200/latest/meta-data/` → `POST /api/v2/webhooks/prompt_lib/2` returned **400**. The form
     shows `webhook destination refused: "100.100.100.200" does not resolve to a permitted destination (loopback,
     private-network, link-local, multicast and reserved addresses are refused)`.
  2. `http://[64:ff9b::a9fe:a9fe]/latest/meta-data/` (NAT64 of 169.254.169.254) → **400** with the same readable
     error for host `64:ff9b::a9fe:a9fe`.
  3. A public target, `https://93.184.216.34/webhooks/elitea`, event `egress.test.never`, inactive → **201**.
     Webhook id `1912149a-82c8-4cf4-8a41-429751bb2078`, created `2026-10-08T16:01:52Z`.
     - It is inactive and subscribed to an event that never fires, so no project data was sent to a third party.
       Live delivery to a public receiver is covered by the dispatcher tests, not by the browser.
     - A public host name (`example.com`) could not be used: the candidate network has no external DNS, and the
       guard reported `could not resolve "example.com"`, which is correct behaviour for that environment.
- **Reload:** after a full page reload the list holds exactly the public webhook (`active: false`, event
  `egress.test.never`). Neither refused URL was stored.
- **Fixtures:** all three cases were created through the UI. The inactive test webhook remains in project 2.
- **Scope of the browser run:** it used the branch before the review fixes above. Those fixes do not touch the
  path the browser exercised (webhook create-time validation). The 3xx-terminal and HTTPS-only fixes are proven
  by the Go tests only.
- **Incident during evidence capture:**
  - One stray `docker exec … /elitea-main -version` started a second Main process in the candidate container.
    `-version` is not a flag.
  - The process logged two startup lines and then exited on a closed pipe.
  - Only the original process remained, and the container stayed healthy.

## Open items and follow-ups

- **HTTP actions (Wave 2, D5).**
  - Wire `httpaction.Transport` only from `egress.Guard.RoundTripper()` with `RequireHTTPS()`.
  - Add `ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST` as the outer bound.
  - Give HTTP actions their own content pool (F6).
  - Today the `Transport` port (`internal/application/httpaction/service.go:47`) still accepts any `Do`
    implementation. It is not wired in production.
- **Toolkit "test connection"** (`internal/api/v2/configurations/toolkit_check.go:530-544`) dials a tenant
  `base_url` through `http.DefaultTransport`:
  - only a host-name allowlist applies, with no dial-time address check;
  - environment proxies are honoured;
  - `ELITEA_TOOLKIT_CHECK_ALLOWLIST=*` disables even that.

  Routing it through the guard changes self-hosted private-host semantics, so it needs its own decision.
- **DeepWiki and Inventory git clone** host checks are name-only (`ELITEA_DEEPWIKI_GIT_ALLOWLIST`). The clone runs
  outside Main.
- **Webhook delivery is not durable across Main loss.** This is a pre-existing L, listed above.
- **OpenAPI text** for the webhook `url` field (`api/openapi/v2.yaml:11691`, generated `api.gen.go:9622`) still
  lists only "loopback, private, link-local or multicast". Updating it requires regenerating the API, so it is
  left for a contract pass.
- **SIIT `::ffff:0:0:0/96`** (IPv4-translated) could join `forbiddenBlocks` in a later pass.
- **Per-entry private egress.** Today a private allowlist entry switches private egress on for the whole path
  (`declaresPrivateEgress`, `internal/infra/egress/classify.go`). Permitting only the addresses inside the named
  blocks would let an operator open a Tailscale range without also opening RFC 1918 and loopback.
- **Guarded forward proxy.** An explicit proxy setting for sites that require one. The guard must still check the
  real destination before the proxy connects to it.
- **Admin-authored allowlists for Main.** The LLM gateway already combines a chart floor with Admin-authored
  entries, applied without a restart. Main's four egress allowlists are chart-only and need a restart.
- **Toolchain:** go1.26.5 → go1.26.6 clears all 7 standard-library advisories. That change belongs to the parallel
  Go-module and toolchain sessions.
