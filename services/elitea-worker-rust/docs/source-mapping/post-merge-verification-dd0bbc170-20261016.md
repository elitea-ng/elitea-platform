# Post-merge verification of merged main `dd0bbc170` (#1084) — 2026-10-16 (WIP)

**Status: work in progress, paused 2026-10-16.** Browser journeys Stop, Worker kill and the native JavaScript
profile are still open.

Scope: #1084 ("Fix Point 5 consumers and retained Code recovery", merged as `89fe71f2a`) verified on current main
`dd0bbc170`, 31 commits later (mostly toolkit, office and indexing work, #1260–#1295). No product code changed in
this PR.

## Stack

Compose project `elitea-pm1084`, `http://pm1084.localhost:19100/app/` (ports 19100–19108), production profile
(`stack.sh`), restored from `product-real-models-main-c0f2e5f9b.dump`, then every migration of this sha
(shared 166, tenants 150). `verify-profile`: **50 passed, 0 failed** (re-run after each Worker recreate).

**Images** (tag `pm1084-dd0bbc170`, built from a `git archive` of this sha, arm64):

| Image | ID | Binary check (`stack.sh binary-check`) |
|---|---|---|
| Main | `75dd6531ba3b` | `now() AT TIME ZONE 'UTC'` (#1295): count 101; 0 in `code1084-9bf82d60d` |
| Worker | `ff29b553a879` | `the toolkit has no usable bucket` (#1293): count 2; 0 in `code1084-9bf82d60d` |
| Sandbox supervisor | `25a94acd8634` | `sandbox_hydration_refused` (#1084): count 1 |
| Web | `4cc1332eb200` | `skipped_capabilities` (#1287): count 2 |
| LLM gateway | `c1d441f56b4b` | |
| Scheduler | `e5a89c80242b` | |
| subapp-host | `717796d7a086` | |
| mock-mcp | (this sha) | needed: the 0def77b22 mock lacks `verify-trust.py` (#1270) |
| doc-extractor | `63eb630b0f80` (`:standalone`, built by compose from this worktree) | |

Code runner images are unchanged (`code1084-22f33b8f8-{deno,rust}`): `services/elitea-code-runner` has no change
since `22f33b8f8`. The vector service still ran the older `elitea-vector:standalone` image (2026-10-12); a this-sha
build (`elitea-vector:pm1084-dd0bbc170`) exists but was not deployed.

## Suites on `dd0bbc170`

| Suite | Result |
|---|---|
| Go `vet` (13 modules), container `golang:1.26.9-bookworm`, `GOPROXY=off` | Pass |
| OpenAPI generated server current (`oapi-codegen@v2.7.2`, no diff) | Pass |
| Contract static checks | 18/18 |
| Go `coverage --race`, CI env, pgvector PG18, NATS 2.12.0 secured profile (natscli 0.4.0), S3 emulator, rerun with `-p=4` | 18,310 tests executed, 32 skipped; all pass except `tests/system` `TestIndexV2PreflightShippedBinary` (needs a docker CLI in the test container, environmental, as on c0f2e5f9b). Undeclared skips: the 2 Azure/GCS conformance suites (emulators not run). The first run was killed by a Docker Desktop restart (VM out of memory); its one failure, `natstest.TestServicesCannotCreateStreams` (TLS i/o timeout under load), passed in the rerun. |
| Worker `fmt`, `clippy -D warnings`, `cargo doc` | Clean |
| Worker `cargo test --all-targets --all-features`, real PG18, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18` | 2,388 passed, 0 failed, 90 ignored |
| Worker ignored CI suites (TLS fixtures generated) | deadline 6, preparation 17, hydration 16: all pass |
| `libs/rust` fmt, clippy (workspace + 2 production feature sets) | Clean |
| `libs/rust` tests | 2,640 passed / 1 ignored; `toolkit-sql,toolkit-office` 1,275; `+test-preserve-order` 1,275 |
| Code runner fmt, clippy, tests | Clean; 136 passed, 2 ignored |
| Web `npm ci`, typecheck, `lint --deny-warnings` | Pass (Node 24, `--engine-strict=false`) |
| Web vitest | 18,204 of 18,251 passed in the loaded run; the 19 files with failures re-run alone: 340/341 pass. The one remaining failure (`themePreference.test.ts` "keeps the mark in memory when storage refuses the write") is deterministic on Node 24 and is an environment mismatch (the package requires Node ≥ 26), not a product failure. |
| Helm: 24 render scripts (incl. master-key, network-policies, platform-edge-identity, sandbox-images) + compiled-snapshots, `helm lint` on 3 charts | All pass (helm v4.1.0) |

The Worker's live secured-NATS test was not run (needs `natstest-serve` reachable from the host).

**Scanners**

- `govulncheck`: 0 called vulnerabilities in all 14 modules. Uncalled, pre-existing: GO-2026-5932 (`x/crypto`
  v0.57.0, no fix) in main, scheduler and gateway.
- `cargo deny --all-features check advisories` (none from #1084):
  - accepted, pre-existing: RUSTSEC-2023-0071;
  - newly published advisories on a pre-existing dependency: `rustls` 0.23.45, RUSTSEC-2026-0346/0347/0348/0349;
  - pre-existing dependencies newly flagged: `unsafe-libyaml` RUSTSEC-2024-0449, `paste` RUSTSEC-2024-0436 (libs only);
  - introduced after #1084 by #1274/#1275 (office-core/pptx): `rustybuzz` RUSTSEC-2026-0206 and `ttf-parser`
    RUSTSEC-2026-0192 (unmaintained), yanked `zip` 2.4.2.
  - Code runner: advisories ok.
- `npm audit --omit=dev`: 2 low (katex/mermaid), unchanged.

## Real-browser evidence (user's Chrome, own tab, mock OIDC `admin@centry.user`, Private project, real `gpt-5.4-mini`)

The dump's model credential was rejected by the upstream gateway at first; the user updated it on this stack and a
new shared dump (`product-real-models-main-dd0bbc170.dump`) was taken before any further fixture was created.

| Journey | Ids | Result |
|---|---|---|
| Python Code with PyPI packages (pipeline 142, `micropip` slugify/dateutil) | chat 882, exec `e982d5f9317433a3fc8eedbbb1126f98` SUCCEEDED | 2,000 rows, total 100003000, slug labels; unchanged after reload |
| JavaScript with npm (pipeline 140, node `javascript_packages`) | chat 883, exec `0ea03112ae5ce246d8263181fd07669d` | JavaScript node succeeded |
| TypeScript with npm (pipeline 140 node `typescript_packages`; pipeline 168 node `ts_npm`) | chats 883, 884; exec `0ea0…669d`, `5b81fc439808c98a48d9bd254b3dd493` FAILED | Refused with the generic `PIPELINE_CODE_FAILED` card. The profile has no TypeScript preparation profile (only python and javascript), so npm imports in TypeScript are not prepared. |
| Plain TypeScript (pipeline 168 node `ts_plain`) | chat 884 | Ran (visit recorded; the run then stopped at `ts_npm`) |
| FU-226: agent node with a sensitive tool (pipeline 167 → agent 166, MCP toolkit 100, `reverse` in `guardrails.sensitive_tools`) | chat 880; paused `d3aa4cbb0a2ef2dba50efb27ed0546d8` SUCCEEDED seq 15 → resumed `3409988984823ef594153bff8d085914` FAILED seq 7; debug repeat: paused `2854b098b02e04d0c465502b8a82eff8` → resumed `0577bea5382457c0af0d1b8d1fe0d6dc` FAILED seq 7 | Card shown and survives reload; Approve → "The runtime operation failed." |
| Same agent approved at the root (no pipeline) | chat 881; paused `4935a0abde7bdbb7ab5007eccba68d25` → `676c8a1c3e5b8eec09c200ad5a94625d` FAILED seq 1 | Typed, readable refusal "Configuration type is not supported." (`native_agent.unsupported_capability`) |

All settlements are claim attempt 1; every `error_code` column was empty (FU-217 pattern).

### FU-226 internal reason (pre-existing on main)

With `ELITEA_RUST_LOG=debug` and `ELITEA_RUST_FAILURE_DIAGNOSTICS=on` (Worker recreated, same image; restored to
info afterwards):

1. `src/agents/direct_hitl.rs:1243-1250` `into_direct_replay`: an approved call whose tool is not catalogued
   read-only is refused with `UnsupportedCapability` ("approved direct HITL replay remains closed for an effectful
   tool", `reverse`). This is the deliberate gate until durable effect ownership exists.
2. Root agent: the refusal stays typed and readable.
3. Pipeline agent node: `src/agents/application_tools.rs:5120` `direct_hitl_execution_error` discards the typed code
   and returns `elitea_nested_agent.invalid_resume` → `pipeline.application_node.failed` →
   `native_agent.event_failed` / upstream `agent.legacy` (`src/agents/runtime.rs:1052`) → generic message.

So the failure is not in a scoped-continuation check on main; the defect is that the nested path loses the typed,
readable refusal.

## Fixtures (all through the UI)

Agent 166, pipelines 167 and 168, chats 880–884. Pipelines 140 and 142 come from the dump (built 2026-10-01).

## Open at the pause

- Browser: Stop during a Code run (CANCELLED once), Worker kill during a Code run (one result, settlement attempt 2,
  no re-run), JavaScript npm on the native profile as its own run, reload checks for chats 883/884.
- PR.

## Follow-ups

- FU-226: carry the typed `DirectHitlError` code through `direct_hitl_execution_error` so a nested sensitive-tool
  refusal is readable; decide whether approved effectful MCP replay opens with durable effect receipts.
- TypeScript npm preparation (defaults PR §8.3: package preparation for every language); until then the refusal
  should say that TypeScript packages are not prepared, not a generic Code failure.
- Settlement `error_code` empty on FAILED (FU-217).
- Shared machine: a Go `-race` workspace run with default parallelism plus image builds exhausted the 16 GiB Docker VM
  and restarted Docker Desktop; run it with `-p=4`.
