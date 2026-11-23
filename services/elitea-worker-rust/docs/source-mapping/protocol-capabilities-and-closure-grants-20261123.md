# Protocol Capability Negotiation and 3-Hop Delegation Grant Closure (2026-11-23)

## Business behaviour and deliberate differences

- **Current platform reference:** The legacy runtime coupled deployment order tightly across control-plane and worker rollouts and did not enforce per-edge delegation grants across multi-hop saved-agent and pipeline compositions.
- **Replatform behavior added here:**
  1. **Zero-deployment-order two-way capability negotiation:** `ClaimCommandRequestV1.worker_capabilities` (field 22) and `ClaimReceiptV1.server_capabilities` (field 20) exchange bounded, domain-named ASCII capability identifiers alongside legacy boolean opt-ins (request fields 16-21; receipt fields 18-19). Mixed old-Main/new-Worker and new-Main/old-Worker deployments preserve every security invariant and fail closed with typed errors (`PROTOCOL_VIOLATION`, `GOVERNED_WORKER_REQUIRED`, `UNSUPPORTED_CAPABILITY`) when a list is malformed or a required capability is absent.
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
  - `services/elitea-worker-rust/src/protocol/node_recovery_inspection.rs`
  - `services/elitea-worker-rust/src/protocol/model_checkpoint_inspection.rs`
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
  - Observed: **196 passed, 3 skipped, 0 failed**.
  - Skipped (explicit prerequisite `ELITEA_TEST_DATABASE_URL`, live Postgres paused during EKG):
    - `TestEpochReadCoversEverySourceTableInOneStatement`
    - `TestMandateRevocationCancelsAtTheNextRenewLease`
    - `TestGovernedClaimWithAStaleBindingEpochIsRejectedOverPostgres`
  - Also verified `./internal/infra/db/repos/...` (`TestConsumableFromCallers|TestClaimValidation`): PASS (0 failed).
- **Go static and dependency checks:**
  - `go vet ./...` (`services/elitea-main`): PASS (0 issues).
  - `govulncheck` on touched Go packages: PASS (`0` vulnerabilities affecting code).
- **Rust targeted wire suite (`CARGO_BUILD_JOBS=4 cargo test --offline --locked -p elitea-worker-rust --lib -- protocol::capabilities protocol::control::node_recovery_inspection protocol::control::model_checkpoint_inspection protocol::control::shared_toolkit_tests`):**
  - Observed: **52 passed, 0 failed, 0 ignored** (including 6 new unit and mock-RPC wire negotiation tests in `protocol::capabilities::tests`).
  - `cargo fmt -- --check`: PASS.
- **CI hygiene:**
  - `node scripts/ci/check-upgrading-notes.mjs`: PASS.
  - Secret scan on `git diff origin/main..HEAD`: `0` hits.

## Performance / Durability / Resilience / Security

| Property | Mechanism (`path:line`) | Proving test | Measured result |
| --- | --- | --- | --- |
| Performance | Bounded capability sets (`MaxNegotiatedCapabilities = 64`, `MaxCapabilityNameBytes = 128` in `services/elitea-main/internal/domain/runtime/capabilities.go:9-17`, `services/elitea-worker-rust/src/protocol/capabilities.rs:14-17`) and batched BFS closure traversal (`MaxGrantClosureDepth = 16`, `MaxGrantClosureNodes = 256`, `MaxGrantEdges = 256` in `services/elitea-main/internal/application/workerdelegation/grants.go:29-42`) with one batched SQL query per depth level (`services/elitea-main/internal/infra/db/repos/delegation_grant_edges.go:19-74`). | `TestParseCapabilitiesBoundsAndSyntax`, `TestClosureGrantsEnforcesDepthAndNodeBounds`, `TestClosureGrantsEnforcesMaxGrantEdgesAndPropagatesEdgeErrors`, `parse_capabilities_enforces_bounds_syntax_and_uniqueness` | 0 allocations during name syntax scan; bounded BFS stops at depth 16, 256 nodes, and 256 grant edges. |
| Durability | No new durable state mutations; claim authority remains strictly fenced in PostgreSQL (`ClaimCommand` validates capabilities before calling `s.claims.Claim`, `services/elitea-main/internal/transport/runtimegrpc/control/server.go:255-285`). Worker 1-shot legacy fallback triggers only when an older Main rejects field 22 before claim admission (`services/elitea-worker-rust/src/protocol/control.rs:2950-2979`). | `TestClaimCommandGovernedAndRequiredCapabilitiesFailClosed`, `new_worker_falls_back_once_for_old_main_preserving_legacy_booleans` | Refused claims never reach `ClaimValidation`; fallback sends exact legacy booleans without creating duplicate leases. |
| Resilience | Strict limit and limit+1 enforcement on capability list length, string bytes, duplicates, BFS depth, node count, and grant edge count; composition edge store failures propagate and fail closed (`services/elitea-main/internal/application/workerdelegation/grants.go:135-138`). | `TestClaimCommandRejectsMalformedAndOversizedWorkerCapabilities`, `TestClosureGrantsHandlesCyclesWithoutLoopingOrDuplicates`, `TestClosureGrantsEnforcesMaxGrantEdgesAndPropagatesEdgeErrors`, `missing_required_server_capability_and_malformed_caps_fail_closed` | Cycles terminate in linear steps; oversized/malformed inputs and store outages return typed non-widening errors. |
| Security | Fail-closed capability validation before authentication/claim (`services/elitea-main/internal/transport/runtimegrpc/control/server.go:255-261`); governed context and mutually exclusive recovery invariants hold across both booleans and `worker_capabilities`; `closureGrants` enforces tenant schema scoping, canonical positive `project_id` checks on input tools, and `grant.ProjectID` verification (`services/elitea-main/internal/application/workerdelegation/grants.go:108-110,192-238`, `services/elitea-main/internal/infra/db/repos/delegation_grant_edges.go:33-60`). | `TestClaimCommandGovernedAndRequiredCapabilitiesFailClosed`, `TestClosureGrantsEnforcesForeignProjectBoundaries`, `new_worker_falls_back_once_for_old_main_preserving_legacy_booleans` | Foreign-project tools and grants are never traversed or emitted; legacy workers without governed opt-in are refused `GOVERNED_WORKER_REQUIRED`. |

## Recovery guarantees

| Component | Phase | Class | Enforcing code | Proving test |
| --- | --- | --- | --- | --- |
| Main + Worker | Admission (claim handshake, mixed versions) | **I — Idempotent retry** / **F — Typed failure** | `services/elitea-main/internal/transport/runtimegrpc/control/server.go:255-285`, `services/elitea-worker-rust/src/protocol/control.rs:2950-2979` | `TestClaimCommandTwoWayCapabilityNegotiationMatrix`, `TestClaimCommandGovernedAndRequiredCapabilitiesFailClosed`, `new_worker_falls_back_once_for_old_main_preserving_legacy_booleans`, `missing_required_server_capability_and_malformed_caps_fail_closed` |
| Main | Admission (multi-hop delegation grant closure) | **I — Idempotent retry** | `services/elitea-main/internal/application/workerdelegation/grants.go:93-163`, `services/elitea-main/internal/infra/db/repos/delegation_grant_edges.go:19-74` | `TestClosureGrantsWalksThreeHopGraphWithoutHop1Grant`, `TestClosureGrantsEnforcesForeignProjectBoundaries`, `TestClosureGrantsEnforcesMaxGrantEdgesAndPropagatesEdgeErrors` |

## Real-browser evidence and fixture honesty

- **Deferred:** No disposable Docker/real-model stack or browser rehearsal was started in this session because local Docker builds and stacks are paused for host EKG headroom.
- **Fixtures:** All Go and Rust tests run against in-process doubles and checked-in conformance vectors (`services/elitea-worker-rust/tests/fixtures/agent_control_vectors.txt`). Live PostgreSQL integration tests for `ReachableApplicationCallees` and `governed_epochs_postgres_integration_test.go` remain deferred until `ELITEA_TEST_DATABASE_URL` runs in CI heavy tier.

## Follow-ups

- Add a real-PostgreSQL integration test in `services/elitea-main/internal/infra/db/repos/` exercising `DelegationGrants.ReachableApplicationCallees` against live tenant schemas (`entity_tool_mapping`, `elitea_tools`, legacy `application_tools`, and foreign-project isolation) once heavy-tier DB tests run.
- Remove legacy boolean fallback once all deployed Main replicas understand `ClaimCommandRequestV1.worker_capabilities` (field 22).
