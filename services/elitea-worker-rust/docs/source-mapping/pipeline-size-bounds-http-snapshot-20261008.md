# Pipeline size bounds and HTTP snapshot scope

PR elitea-ng/elitea-platform#1140, 2026-10-08. This change aligns one pipeline size bound across Main and the Worker,
and stops the HTTP-action snapshot grammar from refusing pipelines that have no `http` node.

## Business behaviour

The current platform accepts saved pipelines regardless of YAML size, legacy numeric node ids and YAML anchors, and
runs them through the SDK graph.

The new platform keeps explicit, bounded limits. It does not adopt the old platform's unbounded acceptance. One bound
applies everywhere: **512 KiB of pipeline YAML**. That value equals the Worker compiler's `MAX_PIPELINE_YAML_BYTES`
(`services/elitea-worker-rust/src/agents/graph/compiler.rs:67`). The user accepts 512 KiB for now and will revisit
the value later.

## Defects and changes

| Layer | Defect | Change |
|---|---|---|
| Main start and saved child | Since `f8a1499d` (2026-10-05), every pipeline start (`services/elitea-main/internal/application/agentexecution/start.go:413`) and every saved child (`internal/infra/storage/runtime_saved_child_scope.go:70`) ran `httpaction.FreezeSnapshot`. Its strict HTTP grammar refused YAML over 64 KiB, non-`!!str` ids and anchored ids **even without any `http` node**. | `FreezeSnapshot` first runs a structural pre-scan (`internal/application/httpaction/frozen.go`). Pipelines without an `http` node return `nil` and are validated by the Worker. The pre-scan rejects only what the Worker compiler also rejects: more than 512 KiB, more than 128 nodes, multi-document YAML, or a non-mapping root. The strict HTTP grammar, including its 64 KiB cap, still applies to HTTP-bearing pipelines. (commit `024e6881`) |
| Worker profile check | `bounded_instruction` (`services/elitea-worker-rust/src/agents/assembly.rs`) capped pipeline `instructions` at a hard-coded 64 KiB, below the compiler's 512 KiB. Pipelines between the two sizes failed with the generic "The execution input is invalid." | The profile check now uses `compiler::MAX_PIPELINE_YAML_BYTES`, so Main, the profile check and the compiler share one bound. (commit `f1334945`) |

## Tests

**Main:**
- `agentexecution/http_action_snapshot_test.go` covers the start path: 100 KiB, numeric id and anchored id. These
  failed on `main` with `ErrUnsupportedCurrentAgentStart`.
- `storage/runtime_saved_child_scope_non_http_test.go` covers the saved child; the bytes are kept unchanged. These
  failed on `main` with `scope.ErrDenied`.
- `httpaction/frozen_scope_test.go` covers the strict-grammar guards for HTTP-bearing pipelines.
- Package tests and `go vet` pass for `agentexecution`, `httpaction` and `infra/storage`. They also pass on the
  candidate revision `efa7213e` with the fix cherry-picked.

**Worker:**
- New test `assembly_tests::a_pipeline_profile_admits_yaml_up_to_the_compiler_bound`. It failed before the change:
  100 KiB was refused.
- Existing test `large_instructions_survive_agent_assembly_and_variable_rendering` now proves refusal at compiler
  bound + 1 byte, instead of at roughly 400 KB.
- Worker library: 1,832 passed, 0 failed, 2 ignored. Strict all-target, all-feature Clippy and fmt pass.

## Performance

**Budget.** No added work on the start path.

**Result.**
- The pre-scan is one YAML parse, which the previous code already paid.
- Non-HTTP pipelines now skip snapshot construction and receipt encoding entirely, so start-path work drops for every
  pipeline.
- The Worker change compares against a constant. No new allocation or I/O.

## Durability

**Threat.** Changing persisted bytes or checkpoint lineage.

**Result.**
- No state, migration, checkpoint or recovery path changes.
- Saved-child version bytes are preserved exactly; `runtime_saved_child_scope_non_http_test.go` asserts byte
  equality.

## Resilience

**Bounds.**
- One explicit bound in all three checks: 512 KiB of pipeline YAML and 128 nodes.
- Multi-document YAML and non-mapping roots are refused.
- HTTP detection errs toward strict: aliases are followed, and merge keys are treated as possible HTTP nodes.

**Remaining gap.** Refusals still surface generic text (follow-up 3).

## Security

**Threats considered.**
1. A pipeline evading the HTTP grammar by hiding an http node behind an alias or merge key. Mitigated: the pre-scan
   follows aliases and treats merge keys as possible HTTP nodes. Tests cover both.
2. Larger accepted input as a resource-exhaustion vector. Bounded at the compiler's existing 512 KiB and 128 nodes,
   so no new maximum is introduced.
3. Authorization. Unchanged: start and saved-child authority checks run as before. The pre-scan only decides
   whether the strict grammar applies.

**Known pre-existing gap (unchanged from main).** An aliased `type` value (`type: *k`) is read as the alias name by
the strict loop. Fix it separately by denying `AliasNode` on `type`.

**Evidence handling.** This record contains no secrets or credentials. Fixture YAML is synthetic.

**Security review (2026-10-08).** No vulnerability meets the >80% confidence bar. The reasons:
- The Worker has no `http` node: `parse_pipeline_node` has no `http` arm, and unknown types fall through to
  `Unsupported`.
- Main's HTTP executor is not wired in production. Even if it were, `FrozenSnapshot.Verify` fails closed
  (`ErrUnauthorized`) without a valid snapshot.
- Author-supplied `http_action_snapshot` fields are stripped before freezing, on both the start and saved-child paths.
- Saved-child authority (`CaptureSavedChildCatalog`: project/actor match, edge selection, member equality, frozen
  definition SHA) is unchanged.

Invariant for future work: if an `http` node is ever added to the Worker, its type detection must match
`declaresHTTPNode` exactly, or the Worker must refuse any `http` node that arrives without a verified snapshot.

**Dependency audits (2026-10-08).** This PR changes no dependencies. Every finding below already exists on `main`;
remediation is tracked as a separate dependency-hygiene task.

- `govulncheck` v1.1.4, run with local Go 1.26.5 on the touched Main packages:
  - 7 reachable standard-library vulnerabilities (e.g. GO-2026-5026 `net/http`, `encoding/asn1`), all fixed in Go
    1.26.6;
  - 1 imported and 3 required-module vulnerabilities, which the scan reports as not called.
  - Production images build on the floating `golang:1.25-trixie` tag (`go.mod` says `go 1.25.8`), so the effective
    exposure depends on the patch release picked up at build time.
- `cargo-deny` 0.20.2 advisories on the Worker:
  - RUSTSEC-2026-0258 (`h2` 0.4.15, through `hyper`/`bollard`);
  - RUSTSEC-2023-0071 (`rsa` 0.9.10, through `sqlx-mysql`; no fixed release exists);
  - one yanked `chacha20` 0.10.1.
- No CI changes, per the user's decision.

## Browser evidence

**Stack.** Local NATS candidate stack (`http://localhost:18094`), real backend, no response mocks, persistent chats.
- Signed in as `admin@centry.user` through the local OIDC emulator.
- Only Main and Worker were replaced, with images built from the revisions already running plus this PR's commits:
  - `elitea-main:current-nats-efa7213e803d-hotfix1140-20261008` = `efa7213e` + `024e6881`
  - `elitea-worker-rust:frozen-cargo-c53ab7d5496a-hotfix1140-20261008` = `c53ab7d5` + `f1334945`
- The original containers are kept, stopped, as `…-rollback-pre1140`.

**Fixtures.**
- Pipelines 153 and 154: created, and their YAML saved, through the editor.
- Pipelines 155 and 156: records created in the UI, and 156's child attachment (tool 98 → 155 version 181)
  selected in the UI.
  - 155's 103,598-byte YAML was written directly to `p_2.application_versions` (too large to type).
  - 156's 206-byte YAML was also written directly, because the automated editor input did not land.
- Every run and every reload went through the browser.

| Case | Pipeline (version) | Chat / execution | Result |
|---|---|---|---|
| 103,598-byte YAML, 3 `state_modifier` nodes of ~34 KiB, no http node | 155 (181) | 862 / `a00a8389…` | `third-ok`; one identical answer after reload |
| Parent agent node → saved child over 64 KiB | 156 (182) → 155 | 863 / `11be1ef1…` | `third-ok`; one identical answer after reload |
| Anchored id (`id: &tick tick`, `entry_point: *tick`) | 154 (180) | 860 / `9c177c2e…` | `anchor-id-ok`; same after reload. Main refused it before the fix. |
| Small saved child (regression) | 136 (143) | 857 / `1ec3aa31…` | `4\|2\|3\|for orders\|2\|True`; same after reload |
| Unquoted numeric id (`id: 1`) | 153 (179) | 859 / `44c5fb0d…` | Main admits it. The Worker refuses it: `native_agent.invalid_input`, "stored pipeline definition could not be admitted". |
| 480 KiB YAML in one LLM node | 72 (79) | 858 / `b3c28c2a…`, 861 / `a781df23…` | Chat 858 (Main fix only): refused by the old 64 KiB profile check. Chat 861 (both fixes): refused by the per-node 64 KiB cap, which is expected. |

## Open limits and follow-ups

1. **Unquoted numeric node ids.**
   - The Worker refuses them at deserialization: node ids are `String`, and `normalize_legacy_graph_identifiers`
     rewrites strings only.
   - The Web pipeline editor crashes on them: `TypeError: e?.replace is not a function`, "The pipeline editor could
     not be displayed".
   - Decide: either support them (normalize to strings in the Worker and Web), or refuse them with a readable message.
2. **Per-node 64 KiB caps** (`llm.rs:39,44`, `decision.rs:26,28`, `direct_tool.rs:31,35`, `application.rs:36`,
   `hitl.rs:20,24`) versus the 512 KiB pipeline bound. Revisit together with the pipeline bound.
3. **Generic error text.** These refusals show "The execution input is invalid." without naming the limit or the
   field.
4. **Main save-time limit.** Main stores pipeline versions far above 512 KiB (version 133 is about 8 MB). A save-time
   bound with a readable error would surface the problem before the run.
