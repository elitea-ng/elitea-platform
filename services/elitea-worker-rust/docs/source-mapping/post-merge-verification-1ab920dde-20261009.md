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

**Not run:**
- the 25 secured-NATS permission tests, which need a native `nats-server` and the rendered configuration;
- the Azure and GCS conformance suites;
- llm-gateway on go1.26.6;
- clippy and rustdoc.

The secured-NATS tests are the ones that cover #1176's NATS permission changes.

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

## Regressions found

1. **Browser sessions are refused about 30–40 s after sign-in.**
   - Main logs `session_unknown` while the session row is valid and not revoked.
   - Isolation: swapping only the Main image back to `0def77b22`, with Web and the edge configuration unchanged, removes
     the symptom across idle time and navigation. A session created by the older Main stays valid after switching
     back to the new Main.
   - The cause is in Main between `0def77b22` and `1ab920dde`. A fix is in progress in a dedicated session.
   - Until it lands, this head is not suitable for interactive use.
2. **Pipeline editor round-trip.** Pipeline 165 (two direct `toolkit` nodes, `dict` outputs, `input_mapping: {}`)
   opens with "Pipeline YAML serialization changed its contract".
   - The editor then holds an empty graph, which blocks saving and attaching tools. The stored document is valid.
   - The check at `apps/elitea-web/src/features/pipelines/lib/dumpYaml.helpers.ts:133` predates this batch.
   - This also blocked a browser rerun of #1176's Worker-side artifact path on this stack. That path was proven on
     #1176's own verified-image stack.

## Follow-ups

1. Session regression (above): find the root cause in Main, fix it, prove it in a browser.
2. The readable `yaml_expansion` limit from #1174 should reach the user from the start path, not only from the
   compiler stage.
3. `deploy/helm/tests/render-main-master-key.sh` fails all 4 renders on the network-policy guard at
   `deploy/helm/elitea/templates/guards.yaml:30`. The script predates the guard's required decision, and no workflow
   runs it.
4. `golang.org/x/net` v0.58.0 → v0.60.0 across the go.work modules (5 HTTP/2 advisories).
5. The editor YAML round-trip (above).
6. Re-run in a browser on the next head: the first batch's checks (fan-in, sensitive approval with effect counts, MCP
   Skip, webhook refusals). They were not repeated here because of regression 1.
