# Post-merge verification of merged main `dd0bbc170` (#1084) — 2026-10-16/19

Suites ran on 2026-10-16; the browser pass ran on 2026-10-16 and 2026-10-19. Everything here is at `dd0bbc170`. Main
has moved on since (15 more commits by 2026-10-19, head `39e999c01`); they are not covered.

Scope: #1084 ("Fix Point 5 consumers and retained Code recovery", merged as `89fe71f2a`) verified on current main
`dd0bbc170`, 31 commits later (mostly toolkit, office and indexing work, #1260–#1295). No product code changed in
this PR.

## Stack

Compose project `elitea-pm1084`, `http://pm1084.localhost:19100/app/` (ports 19100–19108), production profile
(`stack.sh`), restored from `product-real-models-main-c0f2e5f9b.dump`, then every migration of this sha
(shared 166, tenants 150). `verify-profile`: **50 passed, 0 failed** (re-run after each Worker recreate and again on 2026-10-19 before the remaining journeys).

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
| JavaScript with npm on the native profile, own run (pipeline 169, `slugify` + `csv-parse`) | chat 887, exec `41b73073c363288f319ae7915a614d97` SUCCEEDED, attempt 1 | `{rows: 2, slug: "native-js-hello-world", status: PASS, total: 5}`; unchanged after reload. 70 s end to end: the execution job waited ~52 s between creation and dispatch (pipeline 140's JavaScript node took ~11 s on 2026-10-16). |
| TypeScript with npm (pipeline 140 node `typescript_packages`; pipeline 168 node `ts_npm`) | chats 883, 884; exec `0ea0…669d`, `5b81fc439808c98a48d9bd254b3dd493` FAILED | Refused with the generic `PIPELINE_CODE_FAILED` card. The profile has no TypeScript preparation profile (only python and javascript), so npm imports in TypeScript are not prepared. |
| Plain TypeScript (pipeline 168 node `ts_plain`) | chat 884 | Ran (visit recorded; the run then stopped at `ts_npm`) |
| Reload of chats 883 and 884 (2026-10-19) | | Same stored generic card (FU-229) |
| Stop in the pipeline editor Test pane (pipeline 149, 120 s JavaScript delay) | exec `546cf618873f1a36d93463199f70c187` CANCELLED, attempt 1, seq 4 | Canvas "Run is stopped", pane "Execution was cancelled."; settled 10 s after Stop; the downstream node did not run. The Test pane is empty after reload. |
| Stop in a regular chat (pipeline 149) | chat 885, exec `613c41fe1bee29d2a70618b291a820ce` CANCELLED once, attempt 2, seq 9 | Settled ~67 s after Stop. The Worker's stream ended 6 s after Stop, but delivery was held (`sandbox.stop_delivery_pending` → `retry_later`), redelivered after the 60 s AckWait and finished by output recovery (FU-235). The chat is empty after reload: Main removes a stopped turn without output together with its question (`services/elitea-main/API_CONTRACT.md:247`, `internal/db/queries/agent_cancel.sql:181-198`). The current app does the same: on `http://localhost/` (EliteaUI, chat 813) Stop during a streaming answer removed the question and the partial answer, returned the question text to the composer, and the turn was gone after reload. So the empty chat is parity; the ~67 s is the open part of FU-235. Whether the new Web returns the question to the composer was not checked. |
| Worker kill (`docker kill` + start) during Python package preparation (pipeline 142, `delay` 45) | chat 882, exec `1fa8134e8fa064fdfda7ff6c6c300ccf` SUCCEEDED, attempt 2, seq 20 | One answer after reload. `agentstate.sandbox_jobs`: one preparation job (lease re-taken, completed) and one execution job created after the restart; one Code visit, one intent. No re-run. |
| Worker kill after the execution job was created, before its runtime was prepared (pipeline 142, `delay` 25) | chat 882, exec `4438b98ad647867831e8e7e292847fa1` FAILED, attempt 2, seq 21 | Generic `PIPELINE_CODE_FAILED` card. The execution job was never dispatched and failed `sandbox.preparation_incomplete` ~2 min later: `src/sandbox/docker_supervisor.rs:603-626` (`await_prepared`) waits for the provisioning started by the dead Worker and fails at the readiness deadline instead of provisioning again. Effect-free Code ends F with a user-Code message (FU-234). |
| Worker kill after Code dispatch with `delay` 60 | chat 882, execs `23b52721a9dba547f75ff4365d4c2d29`, `958eeb4b9787e4e093a9657c29656f0d` FAILED, attempt 2 | `sandbox.deadline_exceeded` 60 s after dispatch: the input exceeded the Code deadline. The original runtime kept running through the kill (no second dispatch), so this is the input, not recovery. |
| FU-226: agent node with a sensitive tool (pipeline 167 → agent 166, MCP toolkit 100, `reverse` in `guardrails.sensitive_tools`) | chat 880; paused `d3aa4cbb0a2ef2dba50efb27ed0546d8` SUCCEEDED seq 15 → resumed `3409988984823ef594153bff8d085914` FAILED seq 7; debug repeat: paused `2854b098b02e04d0c465502b8a82eff8` → resumed `0577bea5382457c0af0d1b8d1fe0d6dc` FAILED seq 7 | Card shown and survives reload; Approve → "The runtime operation failed." |
| Same agent approved at the root (no pipeline) | chat 881; paused `4935a0abde7bdbb7ab5007eccba68d25` → `676c8a1c3e5b8eec09c200ad5a94625d` FAILED seq 1 | Typed, readable refusal "Configuration type is not supported." (`native_agent.unsupported_capability`) |

Every settlement `error_code` column was empty, including FAILED and CANCELLED (FU-217 pattern).

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

## Recovery guarantees observed (component × phase)

| Component × phase | Class | Evidence |
|---|---|---|
| Worker × Code package preparation | R | exec `1fa8134e…`: preparation lease re-taken, one execution job, one result, attempt 2 |
| Worker × Code execution after dispatch | R (runtime keeps running, no second dispatch) | execs `23b52721…`, `958eeb4b…`: one dispatch; the failure is the input's deadline |
| Worker × final runtime provisioning (job created, not yet dispatched) | F, mis-typed as user Code | exec `4438b98a…`, `sandbox.preparation_incomplete` (FU-234) |
| Main/Worker × Stop in chat | R (CANCELLED once) but slow (~67 s) | exec `613c41fe…` (FU-235) |
| Main/Worker × Stop in editor Test | R (CANCELLED once, 10 s) | exec `546cf618…` |
| Worker × HITL decision on an effectful sensitive tool, nested in a pipeline | F, generic message | FU-226 |

## Performance, durability, resilience, security

- **Performance.** No budgets are claimed by this verification. Observed: chat Stop settles in ~67 s (AckWait 60 s,
  FU-235/FU-215); editor Stop in 10 s; Worker-kill recovery ~2 min 40 s end to end (FU-215 target ≤ 20 s); one native
  JavaScript npm run spent ~52 s between job creation and dispatch.
- **Durability.** Covered by the recovery table above; no duplicate Code effects were seen in any run.
- **Resilience.** Typed failures are lost in two places: FU-226 (nested sensitive-tool refusal) and FU-234 (Worker loss
  during provisioning surfaces as a user-Code failure); FU-229 (TypeScript npm imports unprepared) also shows the generic
  card.
- **Security.** No product code changed. Evidence is redacted: no credentials, prompts or payloads; Worker debug logs
  were read locally and only error codes and source locations are cited. The model credential on the stack was updated
  by the user, not by this session.

## Fixtures (all through the UI)

Agent 166, pipelines 167, 168 and 169, chats 880–887. Pipelines 140, 142 and 149 come from the dump. The current-app
parity probe used a new chat (813) on `http://localhost/`. No database writes were made to create fixtures; the only
direct database access was reading.

## Follow-ups

- **FU-226** (cause on main): carry the typed `DirectHitlError` code through `direct_hitl_execution_error`
  (`src/agents/application_tools.rs:5120`) so a nested sensitive-tool refusal stays readable, as it is at the root; and
  decide whether approved effectful replay opens with durable effect receipts (`src/agents/direct_hitl.rs:1243-1250`).
- **FU-229**: TypeScript Code with npm imports is not prepared on the production profile (only Python and JavaScript
  have preparation profiles); add TypeScript preparation (defaults PR) and a readable "packages are not prepared for
  this language" refusal instead of the generic card.
- **FU-234**: Worker loss during final runtime provisioning ends the node F with the user-Code message; re-provision
  effect-free Code (I) and type the failure as infrastructure (with FU-214/FU-230).
- **FU-235**: chat Stop waits for the 60 s AckWait redelivery before it settles; the empty chat after reload matches
  the current app.
- **Scanners, not from #1084**: `rustybuzz` and `ttf-parser` (unmaintained) and the yanked `zip` 2.4.2 come from
  #1274/#1275 (office-core/pptx); the `rustls` advisories RUSTSEC-2026-0346/0347/0348/0349 are new advisories against a
  dependency present before #1084.
- Settlement `error_code` is empty on FAILED and CANCELLED (FU-217).
- Web: `themePreference.test.ts` fails only on Node 24 (the package requires Node ≥ 26).
- Shared machine: a Go `-race` workspace run with default parallelism plus image builds exhausted the 16 GiB Docker VM
  and restarted Docker Desktop; run it with `-p=4`.
