# Post-merge verification of merged main `c0f2e5f9b` — 2026-10-09

This is the third batch of 2026-10-09. It covers:
- #1179: Go 1.26.9 floor, `x/net` v0.60.0 and `x/crypto`;
- #1180: one admission check on create and update, plus readable refusals;
- #1181: picker paging and discovery on in standalone stacks;
- #1182: social feedback moves to the shared table, with migration 0158;
- #1183: docs;
- #1184: the Helm master-key render test;
- #1185: `STANDALONE_HOST`;
- #1186: the editor YAML contract for toolkit nodes.

No product code changed in this PR. The earlier batches are recorded in
[0def77b22](post-merge-verification-0def77b22-20261009.md) and [1ab920dde](post-merge-verification-1ab920dde-20261009.md).

## Stack

The verification stack is compose project `elitea-verify-0def77b2`, upgraded in place.

**Images.** Tag `main-c0f2e5f9b-verify`. All Go binaries are now built with go1.26.9.

| Image | ID | Note |
|---|---|---|
| Main | `3a2ac118e53b` | also used by both migrate jobs |
| Web | `3f30846074d8` | |
| Worker | `500d9904cc77` | |
| LLM gateway | `cd261a0c5453` | |
| Scheduler | `cc435c9d2cb3` | |
| subapp-host | `32056836d299` | |

**Binary identity.** Each string below is present in the new image and absent from `main-1ab920dde-verify`:

| Image | String | PR |
|---|---|---|
| Worker | `repetition limit exceeded` | #1180 |
| Main | `social_feedbacks_list failed` | #1182 |
| Main | `PIPELINE_NODE_TYPE_NOT_AVAILABLE` / `PIPELINE_TOOLKIT_NOT_ATTACHED` | #1180 |
| Web | `has no YAML form` | #1186 |
| Web | `toolkit discovery unavailable` | #1181 |

**Migrations:** shared at 158 (`social_feedbacks_project`); tenants 1, 2, 118 and 119 at 148.

**API smoke:**
- 5 real models, and a real completion;
- `GET /api/v2/social/feedbacks/default/2` returns 200 (it returned 500 before #1182);
- `toolkit_available_tools` returns `echo` and `reverse`.

## Local suites on `c0f2e5f9b`

Go ran in a `golang:1.26.9-bookworm` container with `GOPROXY=off`, because the host Go is 1.26.5.

| Suite | Result |
|---|---|
| Go `vet`, 13 modules | Pass |
| Go `coverage --race`, CI env, real PostgreSQL 18, secured NATS (natscli v0.4.0 and the configs rendered by `render-secure-conf.sh`), `ELITEA_REQUIRE_DECLARED_SKIPS=1` | 17,125 tests ran, 32 skipped. The one `authattempt` failure was a load flake; it passed 3/3 alone. `tests/system` `TestIndexV2PreflightShippedBinary` needs a docker CLI in the container (environmental). The undeclared skips are the 2 Azure/GCS conformance suites, whose emulators were not pulled. |
| Editor lifecycle (13 scenarios), compiled snapshots, `tests/integration`, `sqlc` (no diff), contract static checks (18/18) | Pass |
| `services/elitea-llm-gateway` (`GOWORK=off`) | `vet` ok; 1,130 tests passed |
| Worker `cargo test --all-targets --all-features`, real-PostgreSQL receipts | 1,833 passed, 0 failed, 71 ignored |
| Worker `--ignored` CI suites (TLS fixtures regenerated) | deadline 6, preparation 17, hydration 12: all pass |
| Worker `fmt` and `clippy -D warnings` | Clean |
| `libs/rust` workspace | 1,007 passed. `toolkit-sql`: 510. `test-preserve-order,toolkit-sql`: 510. `fmt` and `clippy` clean. |
| Code runner | 136 passed; `fmt` and `clippy` clean |
| Web: typecheck, `lint --deny-warnings` | Pass |
| Web: vitest | 16,847 passed. 2 timing flakes under load, both passing when run alone. |
| Helm: `lint` on 3 charts, 18/18 render tests (including `render-main-master-key`) | Pass |

**Scanners:**
- **`govulncheck`:** 0 called vulnerabilities in any module, and `x/net` is clean.
- **Not called:** one finding in a required module, GO-2026-5932 in `x/crypto` v0.57.0, which has no fix yet. It is confirmed for the gateway; main and the scheduler report one uncalled module finding each, and I did not check that it is the same ID.
- **`cargo deny`:** only the accepted RUSTSEC-2023-0071.
- **`npm audit --omit=dev`:** 2 low findings (katex/mermaid), unchanged.

## Real-browser evidence

Built-in Chromium, no response mocks, user `admin@centry.user`, Private project.

| PR | Check | Result |
|---|---|---|
| #1186 | Pipeline 165 (two direct `toolkit` nodes, `dict` outputs, `input_mapping: {}`), YAML tab | Loads the stored YAML; saving is not blocked. This used to fall back to an empty graph. |
| #1180 | `/app/pipelines/create` with a `type: split_out` node on a production Worker build | The editor refuses before save: *"Node split_items: type "split_out" is not available on this deployment — the whole pipeline is refused."* Save stores nothing; no row in `p_2.applications`. |
| #1181 | Pipeline editor, Tools → MCP picker | All 8 project MCPs are listed without searching, including the newest (`verifyauthmcp`). The MCP tool list loads. |
| #1182 | `GET /api/v2/social/feedbacks/default/2`; Like on a chat answer | 200 `{"total":0,"rows":[]}`. The Like is stored through `message_feedback` (200). |
| #1185 | A session created before the Main redeploy | Still valid after it. The stack keeps `localhost:18120`, with no other stack on that host. |
| Direct artifact toolkit node (pipeline 165 run) | Run on this head | Still refused, with the generic "The execution input is invalid." The Worker's reason is a direct tool outside its frozen scope. Fixed by #1188 (open), which was proven on its own stack. |

## Follow-ups

- **#1188:** the artifact toolkit in root direct nodes.
- **Its own follow-ups:** nested positions, log visibility of skipped toolkits, and a false "unsaved changes" prompt after a toolkit is attached. These are tracked in the local follow-up list.
- **Not run locally:**
  - Azure and GCS conformance;
  - `TestIndexV2PreflightShippedBinary` (needs a docker CLI in the test container);
  - the Worker's live secured-NATS test (needs `natstest-serve` reachable from the host);
  - golangci-lint, `cargo doc` and release builds.
