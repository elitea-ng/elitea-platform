# Governed-profile evidence matrix

ADR-0039 P6b, issue #1124. Every acceptance line of #1124 and of the C-1009
requirement (EliteaAI/elitea_issues#6754) maps to a conformance case
(`GOV-NNN`, this module), a unit test, or a deferral that names its gate.

**Status** is one of:

- **pass**: the case or test is in CI and asserts the line. Stack cases run
  in `.github/workflows/ci-governed-conformance.yml`; unit tests run in their
  own workflow (`ci-go.yml`, `ci-rust.yml`, `ci-rust-shared.yml`).
- **deferred (gate)**: it cannot be proven here. The gate is named, and the
  affected mode is excluded from any acceptance claim until the gate closes.

A stack case that skips is listed in `scripts/go/declared-skips.txt` with the
same gate. `cmd/skip-check` fails the run on an undeclared skip, or on a
declared one that ran and passed.

## How to run

```bash
deploy/scripts/governed-conformance.sh run          # stack up, five phases, canary scan, teardown
deploy/scripts/governed-conformance.sh run --keep   # leave the stack up afterwards
go test ./...                                       # in this directory: scanner, skip checker, golden pin
```

The stack cases live behind the `conformance` build tag. The script walks one
stack through five phases, each set by the overlay's two variables:

| Phase | Governance flag | Worker governed context | Test |
|---|---|---|---|
| `off` | off | off | `TestGovernedFlagOff` |
| `on` | on | on | `TestGovernedProfile` |
| `oldworker` | on | **off** | `TestGovernedOldWorker` |
| `restored` | on | on | `TestGovernedRestored` |
| `rollback` | **off** | on | `TestGovernedRollback` |

## Cases

| Id | What it asserts | Where | Status |
|---|---|---|---|
| GOV-001 | Flag off: the governed routes (admin bindings, mandates, external identities, credential references, grants, MCP protected-resource metadata) answer 404. A legacy Jira turn reaches the provider with its own token. Every flag-off golden command vector is byte-identical to its pinned SHA-256. | `TestGovernedFlagOff/GOV-001/*`, `golden.TestGOV001GoldenCommandVectorsAreByteIdentical` | pass |
| GOV-002 | Governed interactive run on an Entra delegation (OBO) reaches the SharePoint double through the broker. | `TestGovernedProfile/GOV-002` | deferred (D1/entra): see below |
| GOV-003 | A sealed project secret is migrated with `migrate-references`: the dry run writes no rows, apply stages, a second admin activates, then attach, toolkit pin and grant. The provider sees exactly that secret (SHA-256 in the mock's journal). The run is pinned governed, with one security context, a bound invocation naming the reference, and a redemption. Both backends are covered: pgvault, and OpenBao (`--copy-to vault`, KV v2 with `cas_required`, read back from OpenBao). A re-apply reports `exists`. | `TestGovernedProfile/GOV-003/{pgvault,vault}_static_secret` | pass |
| GOV-004 | A synthetic sixth toolkit works end to end with no core diff. | unit tests `a_sixth_adapter_registers_and_serves_a_governed_call`, `an_unmapped_synthetic_tool_never_redeems` (libs/rust/agent-runtime/src/toolkits/governed_synthetic_tests.rs), `TestSyntheticAdapterRegistersBesideTheBuiltins`, `TestSyntheticAdapterUsesThePublicAPIOnly` (internal/credentialadapters/synthetic) | pass (unit); stack leg deferred (synthetic-adapter: the shipped worker registers no synthetic adapter, by design) |
| GOV-005 | Each gate alone denies, end to end. Each case breaks one gate on its own agent: RBAC (a member without `models.credentials.governed.use`), binding (suspended), toolkit (pinned before the reference was attached, giving `toolkit_digest_mismatch`; re-pinning then admits), grant (revoked), operation (granted `jira.issue.read`, called `jira.project.read`), authority (no mandate, no delegation). Each case asserts the refusal code or the deny reason, no redemption, and no provider call. | `TestGovernedProfile/GOV-005/{rbac,binding,toolkit,grant,operation,authority}` | pass |
| GOV-005 | Auth source and entity access, each alone. | `TestDefaultAuthSourcesRefuseUnknownAndDevelopment`, `TestDecideEachGateAloneDenies` (internal/authzcompose) | pass (unit); stack legs deferred (auth-source, entity-access: no persona on this stack can isolate them) |
| GOV-006 | Revocation mid-run. A grant revoked, or a reference retired, during a HITL pause: the resumed call never reaches the provider and redeems nothing. Retiring keeps the generation. A binding or mandate revoked during a long stream: the run ends before the stream's end sentinel (cancelled at lease renewal). | `TestGovernedProfile/GOV-006/*`; units `TestStaleEpochsAndDigestsDeny`, `TestEpochBumpMissesAndRevocationDenies`, `TestBindingAndToolkitDriftAfterAdmissionDenies` | pass; delegation leg deferred (D1/entra) |
| GOV-007 | Old-worker safety. With the worker's governed context off, a governed turn is admitted, pinned governed, and not answered for 90 s. Once the governed worker is back, the same run completes. The run was held, not dropped or run on the legacy path. | `TestGovernedOldWorker`, `TestGovernedRestored/GOV-007/*` | pass |
| GOV-008 | Governed schedule. With no mandate the run is skipped with `skipped_mandate_required`. A mandate that does not cover the cron is refused with `MANDATE_SCOPE_MISMATCH`. A covering autonomous mandate dispatches a run pinned to the governed profile. | `TestGovernedProfile/GOV-008/*`; units `TestCheckAttach`, `TestCoversCronAndInputScopes`, `TestAdmitRefusals` | pass |
| GOV-009 | Inbound MCP. The protected-resource metadata names the exact https endpoint and its issuer. No token, a token for another audience, or a token from an untrusted key each get 401 with `resource_metadata`. A token for the exact endpoint is accepted. | `TestGovernedProfile/GOV-009/inbound` | pass |
| GOV-009 | Outbound MCP through the broker. | units `TestGovernedMCPToolCallRedeemsThroughTheBroker`, `TestAnMCPTokenForServerAIsRefusedForServerB`, `TestMissingMCPConsentIsAuthorizationRequired`, Rust `mcp_governed_tests.rs` | pass (unit); stack leg deferred (vault-key) |
| GOV-010 | Audit under concurrency. Eight concurrent probes append exactly eight probe events. `security-audit verify` passes on the outbox and on the durable store, and the durable store holds the events. | `TestGovernedProfile/GOV-010`; units `TestIntegrationTenThousandConcurrentEventsLoseNothing`, `TestIntegrationExporterOutageLosesNothing`, `TestVerifierFindsTheExactEditedSeq` | pass |
| ROLLBACK | Flag off after governed runs, Main restarted. The governed routes are gone and a legacy turn works. A run pinned governed (paused on HITL) is resumed: it never reaches the provider, redeems nothing, and its pin stays governed. | `TestGovernedRollback/ROLLBACK/*` | pass |
| CANARY | Every credential the suite creates is a unique canary (`canaries.json`). After the phases the script scans the NATS tap (every payload on the bus), `pg_dump` of every database (input bundles, security contexts, agent checkpoints, the audit outbox and events), every container log (the OTLP collector's debug export included), the outbox as JSON lines, and the mocks' journal. The scan looks for raw, hex and base64 forms at all three alignments, and any hit fails. Its negative self-test runs first, and a committed leaked fixture keeps the scanner honest in `go test`. | `cmd/canary-scan`, `canary.TestNegativeSelfTestLeakedFixtureFails`, `cmd/canary-scan.TestSelfTestDetectsThePlantedLeak` | pass |

## #1124 acceptance

| Acceptance line | Evidence | Status |
|---|---|---|
| `podman compose -f deploy/docker-compose.governed.yml up -d` starts all services healthy | The overlay extends the standalone stack, so it is composed with `docker-compose.standalone-full.yml` and the Rust worker overlay. `deploy/scripts/governed-conformance.sh up` does that (podman locally, docker in CI) and fails unless every service is healthy and every one-shot exited 0 (`standalone-stack.sh up`). | pass, as an overlay. A self-contained file would duplicate the stack. |
| All GOV-001…GOV-010 pass in CI or are skipped with a named gate in `declared-skips.txt` | Table above. Six declared skips: GOV-002, GOV-004, GOV-005/auth_source, GOV-005/entity_access, GOV-006/delegation_revoked, GOV-009/outbound. | pass |
| Canary scan reports zero hits, and a deliberately leaked canary fails it | CANARY row | pass |
| Flag-off run: existing suites green and golden vectors byte-identical | GOV-001. The existing suites run unchanged with the flag off by default (`ci-go.yml`, `ci-rust.yml`, `ci-web-e2e.yml`). | pass |
| Rollback test passes: no governed-pinned run continues on a legacy path | ROLLBACK, GOV-007 | pass |
| Scale report published | Scale fixture and report (P6b part C, separate change) | covered there |
| `migrate-references --dry-run` makes no writes | GOV-003 asserts the reference and target-binding row counts are unchanged by a dry run, and that no id is minted. The command's own tests ship with it (P6b part B). | pass |
| Evidence matrix covers every acceptance line of #6754 | Next section | pass |

## C-1009 (#6754) acceptance

| Acceptance line | Evidence | Status |
|---|---|---|
| D1–D8 have recorded outcomes or bounded, named deferrals | The gates table below | pass (as a record) |
| Two users × two agents prove the intersection; existing RBAC denial always wins | GOV-005/rbac: a user who passes every other gate is denied without the permission. GOV-005/authority: an agent with a grant but no authority for the user is denied. Every GOV case builds its own agent, and GOV-005 uses two users (U and the member). Units: `TestDecideEachGateAloneDenies`, `TestAuthorityRules`. | pass |
| Exact credential/toolkit/target/version isolation (tampering, duplicate names, private resolution, shared policy overlap, concurrent sessions) | GOV-005/toolkit (a changed toolkit digest is refused until re-pinned). Units `TestGrantForTargetADeniesTargetB`, `TestRedemptionMustNameTheFrozenReference`, `TestViewOfFreezesEveryReferenceLikeCredentialReferenceFor`, `TestBindingAndToolkitDriftAfterAdmissionDenies`, `TestPartitionKeyEveryFieldSeparatesEntries` | pass |
| Initial providers keep useful native operations with MCP disabled; generic paths cannot broaden scope | GOV-005/operation (an operation outside the grant is denied). Units `TestKnownOperationOutsideGrantScopeIsDenied`, `TestUnknownOperationIsDenied`, Rust `governed_tests.rs` (per-family operation maps) | pass |
| Connection checks work before agent creation with separate probe authority; success does not grant execution | GOV-010 (probe). Units `TestProbeNeedsTheProbeCapabilityNotUse`, `TestUseGrantDoesNotPermitOtherCapabilities`, `TestProbeResponseNeverContainsMaterial` | pass |
| Interactive, autonomous, delegated-background, API/MCP, nested and resumed paths share one security context and current rules | Interactive with delegated_background authority: GOV-003/005/006. Autonomous: GOV-008. Resumed: GOV-006 (HITL) and ROLLBACK. Inbound MCP: GOV-009. Units `TestRunCheckerReadsTheContextOnce`, `TestAdmitNamesTheMandateAndItsSubject` | pass. Entra-delegated interactive is deferred (D1). |
| No reusable secret leaks through prompts, code, payloads, checkpoints, browser storage, traces, errors, exports or SDK clients | CANARY (bus, every database, logs and OTLP export, audit). Units `TestLogsCarryNoMaterial`, `TestSecurityEventHasNoSecretLikeFields`, `TestValidateRefusesCredentialShapedValues` | pass for the server side. Browser storage is out of this stack's reach. |
| Grant withdrawal, suspension, rotation, expiry, issuer revocation and missed invalidation meet bounds without collateral revocation | GOV-005/grant, GOV-005/binding, GOV-006. Units `TestInvalidateEpochDropsOnlyOlderEntriesOfThatRecord`, `TestInvalidationDropsThePartitionAndTheDenialIsTimed`, `TestRevocationToDenyLatencyIsObservedOnce`, `TestEntryIsNeverServedWithinTheSkewOfNotAfter` | pass. Agreed numeric bounds are D7. |
| Concurrent refresh/rotation, Vault/Entra/provider outage, lost write responses recover safely | Units `TestPostgresDownDeniesRedemption`, `TestBackendFailuresAreBackendUnavailable`, `TestIdentityProviderBreakerOpens`, `TestIdentityProviderBreakerIsPerTenant`, `TestIntegrationExporterOutageLosesNothing`, `TestAFailedAppendFailsAndRollsBackTheGovernedOperation` | pass (unit). Live outage drills need D6 environments. |
| Synthetic 300-team / 60-application objectives | Scale fixture and report (P6b part C) | covered there. Objectives are D7. |
| PostgreSQL/current profile stays compatible with no new Entra/Vault requirement; corporate profile cannot silently downgrade; mixed versions and rollback exercised | GOV-001 (flag off), GOV-003/pgvault (governed on PostgreSQL alone), GOV-007 (mixed worker versions), ROLLBACK (no downgrade) | pass |
| Flag defaults off, and both states are covered; a synthetic toolkit integrates with no provider-name branches | GOV-001 and every flag-on case. GOV-004 (unit) | pass |
| Migration reconciled, runbooks and owners exist, evidence reviewed, control owner accepts | `migrate-references` and the runbook (P6b part B). This matrix. | Review and acceptance are owner actions, not test results |

## Gates named in this matrix

| Gate | Why it blocks here | Closes when |
|---|---|---|
| D1/entra | An Entra delegation is minted at an Entra sign-in and sealed with `SECRETS_MASTER_KEY`. The standalone stack signs in through its OIDC double and leaves the master key unset, because its seed writes unwrapped vault keys. | A stack with an Entra-backed sign-in and a master key. Customer gate D1 (Entra tenant). |
| vault-key | Outbound MCP and provider OAuth seal the provider grant with `SECRETS_MASTER_KEY` (same reason). | Same stack as above. |
| synthetic-adapter | The shipped worker registers no synthetic sixth adapter, by design. | Never on a shipped image: the unit tests are the evidence. |
| auth-source | Every way to start a run on this stack (PAT, session) is an allowed source. | Unit-proven. A stack leg needs a deliberately misconfigured edge. |
| entity-access | Project membership is RBAC's own precondition, so no persona passes RBAC and fails entity access alone. | Unit-proven. |
| D6, D7 | Real-tenant qualification and agreed operating objectives (#1124 Out of scope). | Customer input |
