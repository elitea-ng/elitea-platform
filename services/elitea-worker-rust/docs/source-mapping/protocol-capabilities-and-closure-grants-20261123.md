# Protocol Capability Negotiation and 3-Hop Delegation Grant Closure (2026-11-23)

## Business behaviour and deliberate differences

- **Current platform reference:** The legacy runtime coupled deployment order tightly across control-plane and worker rollouts and did not enforce per-edge delegation grants across multi-hop saved-agent and pipeline compositions.
- **Replatform behavior added here:**
  1. **Zero-deployment-order two-way capability negotiation:** `ClaimCommandRequestV1.worker_capabilities` (field 22) and `ClaimReceiptV1.server_capabilities` (field 20) exchange bounded, domain-named ASCII capability identifiers alongside legacy boolean opt-ins (request fields 16-21; receipt fields 18-19). Mixed old-Main/new-Worker and new-Main/old-Worker deployments preserve every security invariant and fail closed with typed errors (`PROTOCOL_VIOLATION`, `GOVERNED_WORKER_REQUIRED`, `UNSUPPORTED_CAPABILITY`) when a list is malformed or a required capability is absent. When an older Main running `StrictProtoCodec` rejects field 22 before handler entry with gRPC `Code::Internal` (`strict protobuf contract violation: unknown field 22 in elitea.runtime.v1.ClaimCommandRequestV1`), `ControlGrpcClient::claim_command` classifies it as `ControlGrpcError::UnknownFieldRejected` and `AgentControlClient::send_claim_command` performs one legacy fallback preserving all legacy booleans; ambiguous statuses (`Unavailable`, `DeadlineExceeded`, generic `Internal`) never retry.
  2. **3-hop `closureGrants` graph traversal:** `workerdelegation.Resolver.closureGrants` walks reachable saved-application and pipeline composition edges (from `CompositionEdgeSource` and same-project frozen input tools) in addition to approved grant edges. Intermediate hops that need no grant of their own (because the callee's toolkits are a subset of the caller's) no longer cut off approved grants at hop 2, hop 3, or deeper.
- **Deliberately not ported:** Implicit flag coercion, unbounded graph walks, cross-project application edge traversal, and silent capability downgrade.

## Changed paths

- Protobuf contract and generated bindings:
  - `libs/proto/elitea/runtime/v1/control.proto`
  - `libs/proto/gen/go/elitea/runtime/v1/control.pb.go`
  - `libs/proto/gen/python/elitea/runtime/v1/control_pb2.py`
  - `libs/proto/gen/python/elitea/runtime/v1/control_pb2.pyi`
- Go Main capability negotiation:
  - `services/elitea-main/internal/domain/runtime/capabilities.go`
  - `services/elitea-main/internal/domain/runtime/capabilities_test.go`
  - `services/elitea-main/internal/application/execution/claims.go`
  - `services/elitea-main/internal/application/execution/claims_test.go`
  - `services/elitea-main/internal/transport/runtimegrpc/control/server.go`
  - `services/elitea-main/internal/transport/runtimegrpc/control/capabilities.go`
  - `services/elitea-main/internal/transport/runtimegrpc/control/capabilities_test.go`
- Go Main 3-hop `closureGrants` fix:
  - `services/elitea-main/internal/application/workerdelegation/policy.go`
  - `services/elitea-main/internal/application/workerdelegation/grants.go`
  - `services/elitea-main/internal/application/workerdelegation/grants_test.go`
  - `services/elitea-main/internal/infra/db/repos/delegation_grant_edges.go`
- Rust Worker wire client:
  - `services/elitea-worker-rust/src/protocol/mod.rs`
  - `services/elitea-worker-rust/src/protocol/capabilities.rs`
  - `services/elitea-worker-rust/src/protocol/control.rs`
  - `services/elitea-worker-rust/src/protocol/model_checkpoint_inspection.rs`
  - `services/elitea-worker-rust/src/protocol/node_recovery_inspection.rs`
  - `services/elitea-worker-rust/src/transport/control_grpc.rs`
  - `services/elitea-worker-rust/src/bootstrap.rs`
  - `services/elitea-worker-rust/src/agents/graph/code_workspace_failure.rs`
- Documentation:
  - `services/elitea-worker-rust/docs/source-mapping/protocol-capabilities-and-closure-grants-20261123.md`
  - `services/elitea-worker-rust/docs/source-mapping/README.md`

## Tests, counts, and skips

- **Protobuf checks:**
  - `buf format --diff --exit-code libs/proto`: PASS
  - `buf lint libs/proto`: PASS
  - `buf build libs/proto`: PASS
- **Go race suite (`GOMAXPROCS=4 go test -p 2 -count=1 -race -v`):**
  - Packages: `./internal/domain/runtime/...`, `./internal/application/execution/...`, `./internal/application/workerdelegation/...`, `./internal/transport/runtimegrpc/control/...`
  - Observed: **196 top-level tests / 453 total test+subtest cases passed, 3 skipped, 0 failed**.
  - Skipped (explicit prerequisite `ELITEA_TEST_DATABASE_URL`, live Postgres paused during EKG):
    - `TestEpochReadCoversEverySourceTableInOneStatement`
    - `TestMandateRevocationCancelsAtTheNextRenewLease`
    - `TestGovernedClaimWithAStaleBindingEpochIsRejectedOverPostgres`
  - PostgreSQL integration test binary compile check (`go test -c ./internal/infra/db/repos`): PASS.
- **Go static and dependency checks:**
  - `go vet ./...` (`services/elitea-main`): PASS (0 issues).
  - `govulncheck` on touched Go packages: PASS (`0` vulnerabilities affecting code).
- **Rust targeted wire suite (`CARGO_BUILD_JOBS=4 cargo test --offline --locked -p elitea-worker-rust --lib -- protocol::capabilities protocol::control::node_recovery_inspection protocol::control::model_checkpoint_inspection protocol::control::shared_toolkit_tests transport::control_grpc`):**
  - Observed: **62 passed, 0 failed, 0 ignored** (including 9 unit and mock-RPC wire negotiation tests in `protocol::capabilities::tests` and 6 transport tests in `transport::control_grpc::tests`).
  - `cargo clippy --offline --locked -p elitea-worker-rust --lib --tests -- -D warnings`: PASS (0 warnings, 0 errors).
  - `cargo fmt -- --check`: PASS.
  - `cargo deny --offline --all-features check advisories`: PASS (`advisories ok`; 0 new advisories, 6 pre-existing `advisory-not-detected` ignore warnings tracked by `#2313`).
- **CI hygiene:**
  - `node scripts/ci/check-upgrading-notes.mjs`: PASS.
  - Secret and AI-trailer scan on `git diff origin/main..HEAD`: `0` hits.

## Performance / Durability / Resilience / Security

| Property | Mechanism (`path:line`) | Proving test | Measured result |
| --- | --- | --- | --- |
| Performance | Bounded capability sets (`MaxNegotiatedCapabilities = 64`, `MaxCapabilityNameBytes = 128` in `services/elitea-main/internal/domain/runtime/capabilities.go:13-17`, `services/elitea-worker-rust/src/protocol/capabilities.rs:14-17`) and batched BFS closure traversal (`MaxGrantClosureDepth = 16`, `MaxGrantClosureNodes = 256`, `MaxGrantEdges = 256` in `services/elitea-main/internal/application/workerdelegation/grants.go:29-42`) with bounded SQL fan-out (`ROW_NUMBER <= 256`, `LIMIT 4096` in `services/elitea-main/internal/infra/db/repos/delegation_grant_edges.go:15,26-119`). | `TestParseCapabilitiesBoundsAndSyntax`, `TestClosureGrantsEnforcesDepthAndNodeBounds`, `TestClosureGrantsEnforcesMaxGrantEdgesAndPropagatesEdgeErrors`, `parse_capabilities_enforces_bounds_syntax_and_uniqueness` | 0 allocations during name syntax scan; bounded BFS stops at depth 16, 256 nodes, 256 grant edges, and 4096 edge rows per level. |
| Durability | Startup headroom check validates `ExtraServerCapabilities` plus all 4 built-in server capabilities in `NewServer` (`services/elitea-main/internal/transport/runtimegrpc/control/server.go:223-225`), and `ClaimCommand` computes `serverCapabilitiesForClaim` before `s.claims.Claim` (`server.go:279-282`) so no post-claim error can leak a live lease. Worker 1-shot legacy fallback fires only on unambiguous pre-handler `UnknownFieldRejected` or pre-claim `PROTOCOL_VIOLATION` (`services/elitea-worker-rust/src/transport/control_grpc.rs:894-909`, `services/elitea-worker-rust/src/protocol/control.rs:2956-2997`). | `TestNewServerRejectsMalformedCapabilityConfig`, `new_worker_falls_back_once_for_old_main_strict_codec_and_protocol_violation`, `ambiguous_grpc_statuses_never_retry_claim_command` | Refused claims never reach `ClaimValidation`; ambiguous gRPC statuses execute 1 RPC call only. |
| Resilience | Strict limit and limit+1 checks on capability count (including legacy boolean merges in `services/elitea-worker-rust/src/protocol/capabilities.rs:83-105`), string bytes, duplicates, BFS depth, node count, and grant edge count; SQL `CASE WHEN ... ~ '^[1-9][0-9]{0,17}$'` inside `JOIN ON` prevents `22P02` crashes (`services/elitea-main/internal/infra/db/repos/delegation_grant_edges.go:53-57,72-76`); composition edge errors fail closed (`services/elitea-main/internal/application/workerdelegation/grants.go:147-151`). | `TestClaimCommandRejectsMalformedAndOversizedWorkerCapabilities`, `TestClosureGrantsHandlesCyclesWithoutLoopingOrDuplicates`, `TestClosureGrantsEnforcesMaxGrantEdgesAndPropagatesEdgeErrors`, `from_receipt_enforces_max_bound_when_merging_legacy_booleans`, `non_executable_receipts_reject_server_capabilities_fail_closed` | Cycles terminate in linear steps; non-executable receipts carrying `server_capabilities` and legacy-merge overflows fail closed. |
| Security | Fail-closed capability validation before auth/claim (`services/elitea-main/internal/transport/runtimegrpc/control/server.go:250-285`); governed context and mutually exclusive recovery invariants hold across both booleans and `worker_capabilities`; `closureGrants` enforces tenant schema scoping, canonical positive `project_id` checks on input tools, consumable grant states (`approved`/`stale`), positive grant IDs, frontier membership, and `grant.ProjectID` verification (`services/elitea-main/internal/application/workerdelegation/grants.go:98-183,194-272`, `services/elitea-main/internal/infra/db/repos/delegation_grant_edges.go:26-99`). | `TestClaimCommandGovernedAndRequiredCapabilitiesFailClosed`, `TestClosureGrantsEnforcesForeignProjectBoundaries`, `TestClosureGrantsEnforcesMaxGrantEdgesAndPropagatesEdgeErrors/mixed_edge_kinds_across_3-hop_chain_and_non-consumable_states_filtered`, `missing_required_server_capability_and_malformed_caps_fail_closed` | Foreign-project tools, non-consumable states, and unsolicited callers are filtered out; legacy workers without governed opt-in are refused `GOVERNED_WORKER_REQUIRED`. |

## Security checklist (`rules/security.md`)

| Category | Status | How checked |
| --- | --- | --- |
| Trust boundaries and identity | PASS | Capability lists are validated before `AuthorizeWorkload` and `Claim`; workload identity stays bound to mTLS session verification (`server.go:250-264`). |
| Authorization (object level) | PASS | Governed security context requirement fails closed (`GOVERNED_WORKER_REQUIRED`), required capabilities fail closed (`UNSUPPORTED_CAPABILITY`), and `closureGrants` enforces project boundaries on inputs, SQL tenant schema, and grants. |
| Input, parsing and amplification | PASS | Strict `<= 64` count, `<= 128`-byte ASCII domain syntax, and uniqueness checks on both peers; bounded SQL fan-out (`ROW_NUMBER <= 256`, `LIMIT 4096`) and BFS bounds (`16` depth, `256` nodes, `256` edges). |
| Injection and construction | PASS | Parameterized SQL with validated `tenantSchema`; safe `CASE WHEN ... ~ '^[1-9][0-9]{0,17}$'` cast inside `JOIN ON` prevents `22P02` cast errors. |
| Egress and SSRF | N/A | No outbound HTTP clients, redirects, or egress policy paths modified. |
| Secrets | PASS | `0` hits on `git diff origin/main..HEAD` secret scan; no credentials or sensitive payloads added to logs, errors, or wire fields. |
| Supply chain and deployment | PASS | `0` dependency changes; `govulncheck` (`0` vulnerabilities affecting code) and `cargo deny --offline --all-features check advisories` (`advisories ok`). |
| Cross-layer differentials and version skew | PASS | Identical capability bounds/syntax in Go and Rust; old-Main `StrictProtoCodec` `Status::internal` fallback + old-Worker legacy boolean compatibility verified by unit and mock-RPC matrix tests. |
| Agentic delegation and untrusted content isolation | PASS | Monotonic narrowing preserved; `closureGrants` filters non-consumable states (`pending`/`revoked`/`superseded`), foreign `project_id`, non-positive IDs, and non-frontier callers. |
| TOCTOU, concurrency races, and effect replay | PASS | `serverCapabilitiesForClaim` runs pre-claim so no lease leaks; ambiguous gRPC statuses never retry `ClaimCommand`; Go `-race` clean (`453` cases). |
| Sandbox escape and resource isolation | N/A | No sandbox workspace, container, or package preparation paths modified. |
| AuthN, OAuth, PAT, and cryptographic hygiene | N/A | No OAuth/OIDC/PAT, token minting, or cryptographic primitives modified. |

## Recovery guarantees

| Component | Phase | Class | Enforcing code | Proving test |
| --- | --- | --- | --- | --- |
| Main + Worker | Admission (claim handshake, mixed versions) | **I — Idempotent retry** / **F — Typed failure** | `services/elitea-main/internal/transport/runtimegrpc/control/server.go:250-285`, `services/elitea-worker-rust/src/transport/control_grpc.rs:894-909`, `services/elitea-worker-rust/src/protocol/control.rs:2956-2997` | `TestClaimCommandTwoWayCapabilityNegotiationMatrix`, `TestClaimCommandGovernedAndRequiredCapabilitiesFailClosed`, `new_worker_falls_back_once_for_old_main_strict_codec_and_protocol_violation`, `ambiguous_grpc_statuses_never_retry_claim_command`, `missing_required_server_capability_and_malformed_caps_fail_closed` |
| Main | Admission (multi-hop delegation grant closure) | **I — Idempotent retry** | `services/elitea-main/internal/application/workerdelegation/grants.go:98-183`, `services/elitea-main/internal/infra/db/repos/delegation_grant_edges.go:26-119` | `TestClosureGrantsWalksThreeHopGraphWithoutHop1Grant`, `TestClosureGrantsEnforcesForeignProjectBoundaries`, `TestClosureGrantsEnforcesMaxGrantEdgesAndPropagatesEdgeErrors` |

## Real-browser evidence and fixture honesty

- **Deferred:** No disposable Docker/real-model stack or browser rehearsal was started in this session because local Docker builds and stacks are paused for host EKG headroom.
- **Fixtures:** All Go and Rust tests run against in-process doubles and checked-in conformance vectors (`services/elitea-worker-rust/tests/fixtures/agent_control_vectors.txt`). Live PostgreSQL integration tests for `ReachableApplicationCallees` and `governed_epochs_postgres_integration_test.go` remain deferred until `ELITEA_TEST_DATABASE_URL` runs in CI heavy tier.

## Follow-ups

- Add a real-PostgreSQL integration test in `services/elitea-main/internal/infra/db/repos/` exercising `DelegationGrants.ReachableApplicationCallees` against live tenant schemas (`entity_tool_mapping`, `elitea_tools`, legacy `application_tools`, non-numeric `application_id` rows, and foreign-project isolation) once heavy-tier DB tests run.
- Remove legacy boolean fallback once all deployed Main replicas understand `ClaimCommandRequestV1.worker_capabilities` (field 22).
