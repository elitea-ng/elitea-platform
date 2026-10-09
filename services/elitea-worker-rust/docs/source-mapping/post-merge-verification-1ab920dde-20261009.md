# Post-merge verification of merged main `1ab920dde` — 2026-10-09

This is the second batch of 2026-10-09. These PRs merged while the CI queue was backlogged:
- #1174: parser bounds;
- #1175: access hardening;
- #1176: edge identity propagation;
- #1177: docs;
- #1178: x/text and x/net.

They are verified here on merged `main`. The first batch is recorded in
[post-merge-verification-0def77b22-20261009.md](post-merge-verification-0def77b22-20261009.md). No product code changed.

## Stack

- **Compose project:** `elitea-verify-0def77b2`, the same stack, upgraded in place from `0def77b22`. Rebuilt images
  are tagged `main-1ab920dde-verify`, built one at a time:
  - Main `81ffed1c923e`, which also serves both migrate jobs;
  - Web `64a4c416da66`;
  - LLM gateway `6d6237f398a7`;
  - subapp-host `297143751c3f`;
  - the Rust Worker.
- **Unchanged images:** scheduler, engines and mocks. Their build contexts did not change.
- **Edge configuration:** Traefik and platform-edge were recreated to pick up the bind-mounted configuration from
  #1176.
- **Binary identity** (`docker create` + `docker cp`, then `strings`). Each string below is present in the new image
  and absent from the `0def77b22` image:
  - Worker: `the YAML document exceeds its expansion budget` (#1174).
  - Main: `elitea-main edge identity projection v1` (#1176), plus the master-key and unwrapped-vault-key refusal
    texts (#1175).
  - Web: the full Content-Security-Policy in `security-headers.conf` (#1175).
- **Migrations:** shared at 157, tenants at 148. Unchanged; this batch adds no migration files.
- **Start-up with #1175:** Main started with the stack's existing master key and logged that every project vault
  already holds a wrapped value, for 3 projects. No rewrap was needed.
- **Live CSP header on `/app/`:** `default-src 'self'; script-src 'self' <2 hashes>; style-src 'self' 'unsafe-inline';
  img-src 'self' data: blob: https:; font-src 'self' data:; connect-src 'self'; media-src 'self' blob: data:;
  worker-src 'self'; frame-src 'none'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'; form-action 'self'`.

## Local suites on `1ab920dde`

| Suite | Result |
|---|---|
| Go `vet`, 13 modules | Pass |
| Go `test --race`, CI env, real PostgreSQL 18 (legacy RBAC, prompt-context and pgvector tests required), NATS, S3 | 17,000 ran, 59 skipped. 2 load flakes, both passing in isolation: `authattempt` limiter saturation, and a `storage` attachment-queue timing race. Neither package is in this batch. |
| Editor lifecycle (13 scenarios), compiled snapshots, `tests/integration`, `sqlc` (no diff), contract static checks (18/18) | Pass. `tests/integration` needs a fresh database, which CI always provides. |
| Worker `cargo test --all-features`, real-PostgreSQL receipts | 1,831 passed, 1 failed, 71 ignored. The failure was the debug-build timing assertion in `pipeline_yaml_budget_tests` (3 s limit, under load); the module's 5 tests pass in 0.38 s alone. The `--ignored` CI suites pass alone (deadline 6, preparation 17, hydration 12); their first-run pool timeouts came from a shared database under load. |
| `libs/rust` workspace (agent-runtime, new in #1174) | 1,004 passed. `toolkit-sql`: 507 passed. `test-preserve-order,toolkit-sql`: 507 passed. `fmt` clean. |
| `services/elitea-code-runner` | Pass on re-run. One existing process-reaping flake in `native_finalization`, which this batch does not touch. |
| Web: typecheck, `lint --deny-warnings`, vitest | Pass. 16,814 tests, 1,664 files, no failures. |
| Helm: `helm lint` on 3 charts; 17 of 19 render tests | Pass, except `render-main-master-key.sh` (follow-up 3). `render-nats-security.sh` needs network and kubeconform, so it was not run. |

**Scanners.**
- **`govulncheck`:** the `x/text` finding in subapp-host (GO-2026-6629) is gone after #1178. The 5 `x/net@v0.58.0`
  findings in Main, the scheduler, observability, nativeclient and the generated proto modules remain unchanged
  (follow-up 4).
- **`cargo deny`:** only the accepted RUSTSEC-2023-0071.
- **`npm audit --omit=dev`:** 2 low findings (katex/mermaid), unchanged.

**Closed afterwards: secured NATS and llm-gateway.** These ran on `1ab920dde`, the same commit.
- **Environment:** a `golang:1.26.9-bookworm` Linux container, as CI does, with:
  - `nats-server` v2.12.0 copied out of the `nats:2.12.0` image;
  - natscli v0.4.0, pinned by SHA-256 in `scripts/nats/ci-secure-test-env.sh`;
  - configuration rendered by `scripts/nats/render-secure-conf.sh`;
  - a plaintext JetStream server and PostgreSQL 18 (pgvector);
  - `ELITEA_REQUIRE_NATS_SECURE_TEST=1` and `ELITEA_REQUIRE_DECLARED_SKIPS=1`.

| Package | Result |
|---|---|
| `libs/go/natsconn` | 5 passed |
| `libs/go/natsconn/natstest`, including HA route identity | 9 passed |
| `elitea-main/cmd/elitea-main` | 275 passed |
| `elitea-main/internal/transport/commandbus` | 53 passed |
| `elitea-main/internal/api/v2/canvaspresence`, including the NATS restart test | 37 passed |
| `elitea-main/internal/runtimecomposition` | 602 passed |
| `elitea-main/internal/infra/natsbus` | 17 passed |
| `elitea-scheduler/internal/budgetwriteback` | 69 passed |
| `services/elitea-llm-gateway` (`GOWORK=off`, go1.26.9) | `vet` clean. `test`: 28 packages, 1,632 passed, 0 failed, 6 skipped. The skips are PostgreSQL-only tests, which the gateway CI job does not provision either. `internal/infra/nats` ran secured: 63 passed. |

- **Secured-NATS result:** every previously skipped secured-NATS test ran, with 0 failures. These are the tests that cover
  #1176's NATS permission changes.

**Still not run:**
- **`tests/system` `TestIndexV2PreflightShippedBinary`:** the index-v2 cutover test. It needs a Docker CLI inside the
  container, or a native macOS `nats-server`, plus the `postgres:16-trixie` image. CI runs it.
- **Azure and GCS conformance.**
- **clippy and rustdoc.**

## Real-browser evidence

Built-in Chromium browser, no response mocks, user `admin@centry.user` (id 3) and the restricted
`rust-mcp-restricted-20260913@centry.user` (id 6, not a member of project 2).

| PR | Check | Result |
|---|---|---|
| #1176 | Same-origin `GET /api/v2/social/author` carrying client-set `X-Auth-Type`, `X-Auth-ID: 1`, `X-Auth-User-ID: 1` and `X-Auth-Signature` | Without the session cookie: 401. With it: 200 as the session user (id 3), not id 1. |
| #1175 sanitizer | A real gpt-5.4-mini reply in chat 867 containing a GFM task list, `**bold**`, `<span style onclick id>` and `<img onerror>` | The span keeps no attributes; the img keeps only `src`; both checkboxes render disabled; bold renders. |
| #1175 CSP | An injected inline script, then full loads of chat, agents, the pipeline editor, toolkits, MCPs, webhooks, artifacts and the admin console | The inline script is blocked (`script-src-elem`; the reported hash matches the injected text). The application itself raised 0 CSP violations. |
| #1175 authorization | Restricted user 6 against project 2 | `elitea_core` and `social` feedback, project settings, pin `POST` and `DELETE`, conversation read, and social authors: all 403. User 6 viewing user 3's author card: no `email`. As admin: own card has `email`, another user's card does not, an unknown id returns `{}`. |
| #1174 | Pipeline 154 (YAML anchor as the node id), and pipeline 164, created in the UI, whose state value is a six-level alias chain (~262k nodes, above `PIPELINE_YAML_BUDGET.nodes` 131,072) | 154 answers `anchor-id-ok`. 164 is refused in 2.9 ms at profile validation, so the runtime is bounded and fails closed. The user sees only the generic "The execution input is invalid." (follow-up 2). Main stored the 480-byte document without expanding it. |

## Re-run of the first batch's checks on `1ab920dde`

| PR | Check | Result |
|---|---|---|
| #1162 / #1161 | Pipeline 160, real gpt-5.4-mini, input `right` | `RIGHT BRANCH JOINED` (message 4460) |
| #1159 | Pipeline 162: sensitive `reverse` → Approve → `echo`, effects counted from the mock MCP's `tools/call` log | 0 effects while paused (3 → 3), +2 after Approve (→ 5), answer `{"output":"verify-main-downstream"}` (4463). A reload added none. |
| #1169 | Pipeline 163 → "Authorization required" → **Skip Auth** | "Pipeline stopped — authorization for verifyauthmcp (tool: echo, node: auth) was skipped." (4466). The protected tool never ran. |
| #1149 | Webhook to `https://100.64.1.10/…` | Refused inline; only the host is echoed |

## Findings

1. **Withdrawn: "browser sessions refused 30–40 s after sign-in".**
   - The first version of this document reported a Main regression. It was a test-environment artifact. Several
     standalone stacks were browsed at `localhost:<port>` from one shared browser profile. Cookies are scoped by host
     and not by port, so each stack's sign-in replaced the other's session cookie, and the other stack then logged
     `session_unknown`.
   - The image-swap isolation was confounded by sign-ins on the other stacks during the same window.
   - In a fresh browser, the `0def77b22` and `1ab920dde` Main images behave identically. A single stack on
     `1ab920dde` stays signed in through idle time, navigation and reload.
   - #1185 adds an opt-in `STANDALONE_HOST` (`<name>.localhost`) so concurrent local stacks no longer share a cookie
     jar.
   - `1ab920dde` is fine for interactive use.
2. **Pipeline editor round-trip.** Pipeline 165 (two direct `toolkit` nodes, `dict` outputs, `input_mapping: {}`)
   opens with "Pipeline YAML serialization changed its contract".
   - The editor then holds an empty graph, which blocks saving and attaching tools. The stored document is valid.
   - The check at `apps/elitea-web/src/features/pipelines/lib/dumpYaml.helpers.ts:133` predates this batch.
   - This also blocked a browser re-run of #1176's Worker-side artifact path on this stack. That path was proven on
     #1176's own verified-image stack.

## Follow-ups

1. Concurrent local stacks need distinct hosts (#1185, `STANDALONE_HOST=<name>.localhost`).
2. The readable `yaml_expansion` limit from #1174 should reach the user from the start path. #1180 fixes this: the
   parser's own alias-repetition guard is now classified as the expansion budget, and the user sees the input-limit
   message.
3. `deploy/helm/tests/render-main-master-key.sh` fails all 4 renders on the network-policy guard at
   `deploy/helm/elitea/templates/guards.yaml:30`. The script predates the guard's required decision, and no workflow
   runs it.
4. `golang.org/x/net` v0.58.0 → v0.60.0 across the go.work modules (5 HTTP/2 advisories). #1179, the Go 1.26
   move, covers it.
5. The editor YAML round-trip (above).
