# Sign in from a fan-out delegated-auth card, by reference only (FU-250), 2026-11-03

- **Branch:** `fix/fu250-fanout-authorize-credential`, off `main` `9934b07c0`, with `main` `586e931b9` merged in
  (see "Merge with main" below).
- **Plan:** `handoffs/point5-wave1-20261008/fu250/PLAN.md` (rev 2 plus "Review changes"), and the orchestrator's
  `PLAN-REVIEW.md`: D1 option (A), D2 owner-only, D3 one PR, M1 (claim opt-in), M2, E1–E4, B1–B4, A1, A2.
- **Scope:** contract (proto, card schema, OpenAPI), Worker, agent-runtime, Main and Web, in one PR. No migration.
- **Commits:**
  - `edd7ccb29`: Worker and contract;
  - `6f07c61f1`: Main;
  - `ba0ee1a3e`: Main tests;
  - `90cb304d7`: Worker tests, and a redacted `FetchedDecision` `Debug`;
  - `4c8cbf07e`: Web;
  - `6896887d2`: A2 tests;
  - `318ea5f80`: E2 log captures;
  - `e6972c348`: a scripted claim row's fifteenth column;
  - `99208955d`: the browser fixture's test-only OAuth server in `deploy/mock-mcp/server.py`;
  - `ced100a47`: a requirement names its toolkit even without configured OAuth metadata (found in the browser);
  - `0fc86ed42`: Main's read-only sign-in challenge route (found in the browser);
  - `000780f8e`: the Web card asks for that challenge before opening the dialog;
  - `30bfc7795`: the retry test.

## Business behaviour

- **The run.** A fixed Parallel member reaches an MCP toolkit that needs the user's own delegated OAuth sign-in. The
  member parks and its fan-out card (`kind: delegated_auth`) offers `authorize` and `skip`.
- **Before.** Main refused `authorize` on a fan-out card with a blanket 400, so the user could only Skip. The member
  never ran the MCP call.
- **Now.**
  - The run's own user signs in from the card through the usual MCP sign-in dialog, in reference-only mode. Main
    saves the grant and returns only an `authorization_reference`.
  - The decision carries that reference. Main checks it inside the Decide transaction, and at the wake hands the
    Worker the token for that server only. The member resumes signed in and runs its call.
  - Anyone else sees Skip only, with "Only the person who started this run can sign in for it."
- **Current platform.** It asks the user to sign in and re-runs the turn from scratch. The re-run is not ported: the
  member resumes from its own pause, and its siblings keep their results.
- **Not ported:** the current platform's raw-token transport. The token never travels in a decision, a card, a frame
  or a log, and the Web never sees it.

## How it works

### Contract

- `AgentExecutionInputV1.mcp_token_references = 69` (`libs/proto/elitea/runtime/v1/agent.proto`), holding repeated
  `McpTokenReferenceV1 {resource, reference, revision, toolkit_id}`. Main fills it at wake admission. The materializer
  resolves the references and then clears the field, so a Worker never receives one.
- `ClaimCommandRequestV1.fanout_delegated_credentials = 20` (`libs/proto/elitea/runtime/v1/control.proto`): the
  Worker's opt-in (M1).
- The card's `display.toolkit_id` is optional; the schema's `display` `maxProperties` is 15. Both copies of the card
  schema and `api/openapi/v2.yaml` are updated, plus the fixture
  `libs/jsonschema/runtime/v1/fixtures/fanout-interrupt-card-v1.delegated-auth-toolkit.json`.
- New error code `agent_interrupt_credential_refused`, 409.
- New route `POST /elitea_core/mcp_authorization_challenge/prompt_lib/{project_id}/{toolkit_id}`
  (`getMcpAuthorizationChallenge`, schema `McpAuthorizationChallenge`, `additionalProperties: false`).

### Worker

- **The pause names its toolkit (M2).**
  - `with_pause_toolkit_id` (`services/elitea-worker-rust/src/agents/graph/direct_tool.rs:1839`) adds a top-level
    `toolkit_id` to a new MCP-auth pause. It reads it from `DelegatedAuthorizationRequirement::toolkit_id`
    (`libs/rust/agent-runtime/src/toolkits/delegated_auth.rs:424`). Called from the direct-tool pause and from
    `llm.rs:1425`.
  - The card copies it (`src/agents/events.rs:5264`) only when the stored pause holds it.
  - **Found in the browser and fixed (`ced100a47`).** `with_toolkit_id` used to write the id only into an existing
    `resource_metadata`. A plain MCP toolkit without a configured OAuth client has none, so its pause had no
    `toolkit_id` and its card stayed Skip-only. The requirement now holds a separate optional `toolkit_id`
    (`delegated_auth.rs`), kept out of `resource_metadata` so `authorization_tool_name()`, a hash of the configured
    metadata, does not change. `toolkit_id()` prefers it and falls back to the metadata; `valid_requirement` bounds it
    to a positive int32. Tests: `a_toolkit_without_configured_metadata_still_names_its_toolkit` and
    `an_authorization_requirement_without_an_oauth_client_names_its_toolkit` (`mcp_tests.rs`).
- **The opt-in (M1).** `build_agent_claim_request` sets `fanout_delegated_credentials` (`src/protocol/control.rs:3153`).
  Recovery claims keep it. Every other claim defaults to false (`:3173`).
- **An authorize binds exactly the card's server.**
  - `delegated_auth_server` (`src/agents/graph/parallel_cards.rs:179`) reads the server from the pause.
  - `PipelineMcpAuthorizationContinuation::fanout` (`src/agents/graph/resume.rs:905`) binds it. An authorize without a
    server is invalid.
- **No credential, no resume.**
  - `credential_unavailable` (`src/agents/graph/parallel.rs:1638`, called at `:1606` before `resume_input`) checks
    two things: the decision named a reference, and the wake's `mcp_tokens` hold the card's server
    (`PipelineNodeRuntimes::delegated_servers`, `src/agents/graph/compiler.rs:319,327`).
  - If either is missing, the branch ends `graph.fanout.credential_unavailable` (`parallel.rs:87`) with readable text
    (`:2243`), and nothing runs.
- **A reference never reaches the Worker.** `parse_agent_execution_input` refuses an input that still carries
  `mcp_token_references` (`src/agents/protocol.rs:57`).
- **`FetchedDecision` `Debug` redacts** `value` and `credential_ref`
  (`libs/rust/agent-runtime/src/graph/fanout/runner.rs:101`).

### Main

- **Decide** (`internal/infra/db/repos/execution_interrupts.go:432`, `checkDelegatedCredential` at `:857`). It runs
  in the Decide transaction after the response lock and before the CAS (E4):
  - the card must carry `display.toolkit_id` and a server (`delegatedCardBinding`,
    `execution_interrupt_wakes.go:529`, canonicalised by `mcpoauth.CanonicalResource`); otherwise
    `ErrAuthorizeUnavailable`, 400 "Signing in from this card is not available; choose Skip.";
  - the decider must equal `execution_jobs.actor_id` of the card's execution (D2);
  - `Tokens.Validate(reference, {project, actor, toolkit, resource})` must succeed, without decrypting.

  Any failure is `ErrCredentialRefused`, mapped to 409 `agent_interrupt_credential_refused`
  (`internal/api/v2/agentexecution/interrupts.go:203,206`). It is one code for every cause, so it does not enumerate.
- **List per viewer.** `viewerCard` (`execution_interrupts.go:896`) drops `authorize` for anyone but the run actor,
  and for a card without a toolkit id. No viewer ever sees a grant.
- **Wake admission (E1).**
  - `wakeDelegatedCredentials` (`execution_interrupt_wakes.go:487`) reads only DECIDED (not CONSUMED) `authorize`
    decisions of this response, one reference per card, bound to the card's toolkit and canonical resource.
  - `FanoutWakeInput` sets `McpTokenReferences` (`internal/application/agentexecution/continue_fanout_wake.go:211`).
  - `validDelegatedCredentialReferences` (`input_bundle.go:195`) allows at most 16 references. Each must be a 32-byte
    base64url reference, revision 1, a toolkit id within int32, and an https resource; duplicates and unknown fields
    are refused.
- **Claim gate (M1).**
  - The first claim query computes `wakeCredentialsSQL` (`internal/infra/db/repos/claims.go:61`, the const at
    `:1261`), so there is no extra round trip.
  - A wake whose response holds an authorize decision goes only to a claim that opted in. Any other claim gets
    `ClaimRetryLaterNoACK` (`:197`).
- **One release path.** `resolveMCPTokenReference` (`internal/infra/storage/toolkit_runtime_context.go:77`) serves
  both the toolkit-call materializer and `materializeDelegatedCredentials` (`configurations_materializer.go:319`).
  - It always runs under the live claim's project and actor. Validate re-checks the full binding and the exact
    revision before Load decrypts.
  - Resolved grants are projected into `mcp_tokens` under their resource, and the references are cleared.
  - An unresolvable grant (revoked, expired, another actor's) is dropped, so the Worker ends that branch typed (E3).
  - A malformed or duplicate entry refuses the content.
- **Wiring.** `SetDelegatedTokens` (`internal/runtimecomposition/composition.go:926`).
- **Sign-in challenge (`0fc86ed42`).** `MCPAuthorizationChallenge`
  (`internal/api/v2/eliteacore/mcp_authorization_challenge.go`, routed in `internal/api/router.go` next to
  `mcp_sync_tools`) answers the public consent metadata of one saved MCP toolkit. The card names only its toolkit.
  - **Permission.** `requireMCPConnect`, i.e. `models.applications.predict.post` through the always-on
    `projectPermission` gate: the run permission of `mcp_oauth_proxy` and `mcp_dcr_proxy`, which the same dialog
    calls next. The run's own user may be a viewer, who does not hold `tool.patch` (migration 0068, #6885); the first
    version used `tool.patch` and so refused a viewer (REVIEW-1566 R1).
  - **The endpoint comes from storage.** `ResolveDelegatedAuthToolkitSettings(project, actor, toolkit)`, the resolver
    the OAuth exchange uses, then `mcpoauth.ToolkitResource`. The request body is not read. Unknown, another
    project's and an invisible toolkit all answer the same 404 `toolkit_not_found`. Only `mcp` toolkits with an https
    endpoint are served.
  - **Outbound.** `authorizationMetadataSource()` only: the guarded dialer (`ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST`), its
    per-fetch timeout and size cap, under a 10 s total budget. The protected-resource document must name the stored
    resource (`ProtectedResource`), and its `resource` must also canonicalise to it, else
    `authorization_resource_mismatch`.
  - **Answer.** `server_url`, `resource_metadata_url` and the projected `resource_metadata` (the issuer document
    already projected by `Fetcher.project`), `Cache-Control: no-store`. Failures are neutral codes; the log line
    carries project, toolkit and code only.

### Web

- `LedgerSignIn.tsx` (`apps/elitea-web/src/widgets/chat-box/ui/`) opens the existing `McpAuthModal` with
  `authorizationReferenceOnly` for the card's `display.toolkit_id`. It reads the reference with
  `getAuthorizationReference` and decides `authorize` with `credential_ref`.
- **Authorize first reads the challenge (`000780f8e`).** `useMcpLogin` gets an injected login
  (`authConfig.onLogin`) that calls `readMcpAuthorizationChallenge(projectId, toolkitId)`
  (`features/mcps/api/mcpAuthorizationChallenge.ts`, the generated `getMcpAuthorizationChallenge`). It parses the
  answer with the generated schema and hands it to the dialog. The card's own server URL is never sent. A refusal
  shows a readable message per code and keeps Skip; a retry clears it.
- Non-actors get the card's Skip (`ChatContinue`) plus a one-line hint. No modal, no grant listing, and it stays
  within the current visual language.
- A 409 `agent_interrupt_credential_refused` becomes the readable refusal (`interruptLedgerApi.ts`,
  `useChatBoxHandlers.ledger.ts:44`). The card goes back with "The sign-in could not be used for this run…".
- `interruptLedger.buildRequest` never sends `authorize` without a reference, and never sends a reference with any
  other action.

## Changed paths

60 hand-written files, plus 10 generated ones (Go and Python proto stubs, `api.gen.go`, zod and MSW). The source is
listed in the sections above; the full list is `git diff --name-only 9934b07c0..HEAD`.

## Tests

| Layer | Test | Proves |
|---|---|---|
| Main, real PG | `TestAuthorizeAcceptsOnlyTheRunActorsOwnGrantForThisCard` (`execution_interrupts_authorize_postgres_integration_test.go:100`) | Authorization matrix: owner with own reference accepted; foreign reference, another member, another toolkit or resource, revoked, expired → the same 409 |
| Main, real PG | `TestARefusedAuthorizeLeavesTheCardDecidable` (`:152`) | E4: after a 409 the row is byte-identical and the revision unchanged; a valid authorize with the same `expected_revision` then succeeds |
| Main, real PG | `TestAnOlderCardOrNoGrantStoreRefusesAuthorize` (`:166`) | M2: a card without `toolkit_id` gets the typed "choose Skip"; with no grant store, the 409 |
| Main, real PG | `TestListOffersAuthorizeOnlyToTheRunActor` (`:191`) | D2: per-viewer `available_actions` |
| Main, real PG | `TestAWakeCarriesOnlyDecidedAuthorizeReferences` (`:229`) | E1: only DECIDED authorize cards contribute; a consumed one does not |
| Main, real PG | `TestTheWakeCredentialPredicateSeesAnAuthorizeDecision` (`:272`) | The M1 predicate on real rows |
| Main, real PG | `TestAuthorizeRefusesAnotherProjectsToolkitAndAnotherServer` (`:303`) | A2: another project's toolkit, and a project toolkit whose resource differs from the card's server → the same 409 |
| Main, real PG | `TestExecutionInterruptPublicRouteAuthorizationMatrix` (`execution_interrupts_http_postgres_integration_test.go:117`, unchanged) | The route-level rows of gate §3a for these same routes: unauthenticated, foreign project, token vs session |
| Main | `TestAWakeWithAnAuthorizeIsGrantedOnlyToAnOptedInWorker` (`claims_test.go:1410`) | M1: retry-later without the opt-in, under every disposition; granted with it |
| Main | `TestAgentWakeReferencesResolveForTheClaimActorOnly` (`toolkit_runtime_context_test.go:106`) | E1 at redemption: the claim's actor only; another actor's grant is dropped; malformed or duplicate entries are refused |
| Main | `TestAuthorizeRefusalsAreTypedAndNonEnumerating` (`interrupts_test.go:323`) | Typed codes plus an slog capture: no reference or token in logs |
| Worker | `an_authorize_without_its_credential_ends_the_branch_typed` (`parallel_cards_tests.rs`) | No reference, or no token for the server → `graph.fanout.credential_unavailable`, and the call never runs |
| Worker | `a_delegated_auth_card_resumes_only_its_child_and_an_older_pause_is_refused` | Authorize resumes only its child; no checkpoint of either child or their pipelines holds the reference; `Debug` redacts it |
| Worker | `a_delegated_auth_card_names_its_toolkit_only_when_its_pause_stored_it` | M2: an older pause re-raises byte-identical, without `toolkit_id` |
| Worker | `an_authorize_wake_logs_no_credential_reference_at_trace` | E2: a child process under a global TRACE subscriber with span events (105 lines captured) logs no reference |
| Worker | `an_input_with_unresolved_credential_references_is_refused` (`protocol.rs`) | A reference never enters a Worker |
| agent-runtime | `a_projected_token_reaches_only_the_toolset_of_its_resource` (`mcp_tests.rs:2204`) | A2: on the recording server's request log, only the toolset on resource R carries the bearer; the other server gets none |
| agent-runtime | `a_projected_token_is_never_logged_at_trace` | E2: TRACE capture (hyper's connection lines included) holds no token bytes |
| Main | `TestAuthorizationChallengeAnswersOnlyTheStoredToolkitsPublicMetadata` (`mcp_authorization_challenge_internal_test.go`) | The exact key set; a URL in the body or query never steers discovery; discovery dials only through the guarded client; no stored secret, token or client secret in the answer |
| Main | `TestAuthorizationChallengeIdentityMatrix` | Gate §3a at the handler: session and token-with-owner allowed; token without an owning user and unauthenticated 401; another user, another project's toolkit and an unknown toolkit the same 404; malformed ids 400; no refused request reaches the network |
| Main | `TestEliteaCoreRoutesRefuseAnotherProject`, `…RefuseAnUnderPrivilegedMember`, `…AdmitAnEntitledMember` (new row in `router_elitea_core_project_scope_test.go`) | Gate §3a at the router: foreign project 403, a member without `predict.post` 403, an entitled member admitted |
| Main | `TestViewerCanReachTheMCPAuthorizationProxies` (new path, `router_mcp_connect_and_project_settings_test.go`) | A viewer (`predict.post` without `tool.patch`) passes the gate; it fails with 403 against the earlier `tool.patch` gate |
| Main | `…RefusesANonMCPToolkitWithoutFetching`, `…RefusesAStoredURLThatIsNotHTTPS`, `…RefusesAGuardedDestination`, `…FailureIsNeutral`, `…RefusesMetadataForAnotherResource` | Typed refusals with no fetch; the guard refuses a private destination; no upstream body or internal URL in the answer; another resource's metadata refused |
| Main | `TestAuthorizationChallengeLogsNoMetadataOrStoredSecrets` | slog capture over success and failure: no stored header secret, upstream body, endpoint or metadata URL |
| Web | `LedgerSignIn.test.tsx` (real `useMcpLogin`, `useMcpAuthModal`, `McpAuthModal`; only MSW and the recording test socket) | One challenge request with no body and no card URL, no `test_mcp_connection` emit, the dialog's Authorize enabled; three refusals show readable text, no dialog, Skip works; a retry clears the message. All four original cases fail against the previous card |
| Web | `interruptLedgerApi.test.ts` (MSW), `interruptLedger.test.ts`, `ApplicationAnswer.approvals.test.tsx` | The 409 mapping; never authorize without a reference; the run user signs in and sends the reference; anyone else gets Skip only, with the reason |

### Suite runs

- **Worker lib, full:** 2893 passed / 0 failed / 105 ignored (`cargo test --offline --locked --all-features --lib`, at `318ea5f80`). Clippy `--all-targets --all-features -D warnings` is clean.
- **agent-runtime lib:** 1737 passed / 0 failed / 1 ignored before the E2 test; the two `projected_token` tests pass.
  Clippy is clean.
- **Main `go test ./...` with `ELITEA_TEST_DATABASE_URL` (a throwaway PostgreSQL 18):** 292 packages ok; one failure in `internal/infra/db/repos`, which was this branch's (a scripted claim row still had 14 columns), fixed in `e6972c348`. The `repos` package then ran 591 passed / 2 skipped (fixtures owned by CI: NATS service-backed, compiled-snapshot PostgreSQL) and 1 failed, all against the real database.
  - **The 1 failure predates this branch.** `TestCorpusOnlySchemaSupportsFoldersAndSelectedConversations` times out on the same local PostgreSQL 18 at the base commit `9934b07c0` too. A chat-folders list query keeps running on CPU past its 20 s deadline. FU-250 changes no folders or chat code.
- **Web unit (`test:unit`, node project):** 1859 files; 18363 passed, 6 expected-fail, 1 skipped (Node 24.19). Typecheck and lint are clean.
- **Skips:** none added. The real-PG tests run only with `ELITEA_TEST_DATABASE_URL` set, as everywhere in Main.
- **After the merge with `main` `586e931b9`:**
  - Main `go test ./...` with a throwaway PostgreSQL 18: 295 packages ok and one package failing only with
    "out of shared memory" while applying migrations (`authsession`), which passes on a server started with
    `max_locks_per_transaction=512`, as CI runs it (`.github/workflows/ci-go.yml:409`). `repos` (302 s) and
    `eliteacore` pass, the local FU-278 timeout included this time. The client-contract lock test, red before the
    merge only because the branch was behind, passes.
  - Worker `cargo test --offline --locked`: 2780 passed / 0 failed / 18 ignored (default features, all targets).
    agent-runtime lib: 1546 passed / 0 failed / 1 ignored. Clippy `--all-targets --all-features -D warnings` is clean
    on both.
  - Web: the new `LedgerSignIn.test.tsx` 5/5; `features/mcps`, `widgets/chat-box`, `features/chat-messages` and
    `pages/toolkits`: 173 files, 1767 passed, 3 expected-fail. Typecheck, lint, `check-generated-client` (984 files),
    `check-endpoint-manifest`, `check-handlers` and `check-spec-examples` pass.

### Merge with main

- `main` `586e931b9` (unattended runs, #1561) took `AgentExecutionInputV1` field 68 for `unattended_grant`. This branch's
  `mcp_token_references` moved to 69 before anything shipped; both messages are kept. Go and Python stubs were
  regenerated with the pinned toolchain; the Worker compiles the proto from source, and no fixture encodes the tag.
- `protocol.rs` keeps both `request_and_grant_from` and the credential-reference test; `api.gen.go` was regenerated
  from the merged spec.
- The browser proof ran on `000780f8e` (before the merge), because the merged `main` brings migrations the throwaway
  stack's database does not have. The merge changes no FU-250 behaviour beyond the field number; the merged suites
  above are the evidence for it.

## Performance

- **Claim.** The opt-in check is an `EXISTS` folded into the first claim query (`claims.go:61`), so the statement
  count is unchanged. The scripted claim tests pin the statement sequence and failed when this was a separate query.
- **Decide.** For `authorize` only, it adds two point reads inside the existing transaction:
  - the actor, from the `execution_jobs` primary key `(execution_id, generation)`, `LIMIT 1`;
  - the grant row (`Validate`, no decrypt).

  Other actions are unchanged.
- **List.** One JSON rewrite, only for `delegated_auth` cards shown to a non-actor.
- **Wake admission.** One query on the `(root_response_id, interrupt_key)` primary-key prefix, ordered by the index
  and bounded by the response's open cards (contract cap 16).
- **Redemption.** At most 16 references, each a Validate plus a Load (two reads and one AES-GCM decrypt).
- **Worker.** One `BTreeSet` lookup per authorize decision. No extra I/O.
- **Sign-in challenge.** Only on an Authorize click: one resolver call and at most three guarded fetches (protected
  resource, at most two issuer candidates), each under the per-fetch timeout and a 10 s total budget. Nothing is
  cached or stored.
- **Measured:** none beyond the tests. Every addition is O(cards) and bounded by the 16-card cap. No benchmark was
  added, because no hot path changes for any action other than `authorize`.

## Durability

- **Durable intent before effect.** The decision, with its reference, is stored DECIDED in the same transaction that
  checked it (E4). The wake is admitted from those rows, and the token is resolved only when the claimed Worker
  fetches content.
- **Stable identity across retries.** Grants are immutable (revision 1). A reclaim re-fetches the content through
  the materializer under its own claim and resolves the same reference again. Input is never spooled; the spool holds
  output frames only (`src/spool.rs:1-8`).
- **Fencing.** Redemption happens only in the claim-checked content listener, under the live claim's project and
  actor.
- **Crash windows:**
  - after Decide, before the wake: the wake is admitted from the DECIDED rows (I);
  - after materialization, before the member's call: the reclaim re-materializes and re-runs from the member's pause
    row (R, the existing P13 recovery; the call's node journal decides as before);
  - the grant revoked or expired in between: F typed (E3).

## Resilience

- **Bounded.** 16 references per input, a 32-byte reference, revision 1, a toolkit id within int32, https resources
  only. Malformed input is refused (`input_bundle.go:195`, `configurations_materializer.go:319`).
- **Typed, readable failures.**
  - 400 "choose Skip" for an older card;
  - 409 `agent_interrupt_credential_refused`;
  - `graph.fanout.credential_unavailable` with "The sign-in for this branch could not be used: it expired or was
    revoked before the run continued. Sign in again and run it once more."
- **Mixed versions (M1).**
  - An older Worker never claims a wake that carries an authorize: it gets retry-later until a capable Worker takes
    it.
  - An older Main still refuses `authorize`.
  - A card raised by an older Worker has no `toolkit_id`, so it stays Skip-only (M2).
- **No silent coercion.** An authorize never resumes "authorized" without its server's token.
- **The slip, and why the tests missed it (test honesty).** The first `LedgerSignIn` reused `useMcpLogin` without an
  injected login, so Authorize fell back to the `test_mcp_connection` socket check, which Main has never answered on
  this platform. In a real browser the click did nothing: no request, no dialog. The card tests rendered it through a
  mocked `renderLedgerSignIn` slot (`ApplicationAnswer.approvals.test.tsx`), so `useMcpLogin` never ran. The new test
  renders the real hook, dialog and modal and mocks only the network; its four original cases fail against the
  previous card. Other Web callers of the same socket check are FU-282.
- **Sign-in challenge failures** are typed and readable on the card (`toolkit_not_found`,
  `unsupported_toolkit_type`, `invalid_toolkit_resource`, `authorization_discovery_failed`,
  `authorization_discovery_timeout`, `authorization_resource_mismatch`, `authorization_challenge_unavailable`), and Skip
  stays available.
- **Rollback to an older Worker.** A pause written by this Worker carries a top-level `toolkit_id`, and its
  requirement a serialized `toolkit_id`. An older Worker refuses such a pause typed, never with a panic or Internal
  (all cited at base `9934b07c0`):
  - The requirement has no `deny_unknown_fields` (`libs/rust/agent-runtime/src/toolkits/delegated_auth.rs:233`), so
    its new field is ignored wherever a requirement is decoded (declined scope, error metadata).
  - The pause data is `#[serde(deny_unknown_fields)]` (`services/elitea-worker-rust/src/agents/events.rs:4440`), so
    its parse fails with `invalid_state` (`:5469`).
  - On the fan-out path, `card_parts` maps that failure to the typed `graph.parallel.pause_unproved`, "the branch
    pause cannot be shown as a card" (`services/elitea-worker-rust/src/agents/graph/parallel_cards.rs:228`).
  - On the resume path, `nested_pipeline_interrupt_identity` (`events.rs:5706`) returns `invalid_state`.
    `projection_configuration` (`services/elitea-worker-rust/src/agents/session.rs:4426`) maps it to
    `NativeAgentAssemblyErrorCode::InvalidInput`, "the invocation-local native agent input is malformed".
  - There is no fixture test of the older binary: its behaviour can't run inside this branch's suite, so the citations
    above stand in for it.

## Security

- **Identity and authorization.**
  - The decider must be the run's actor (`execution_jobs.actor_id`).
  - The grant's full binding (project, actor, toolkit, canonical resource) is validated in the Decide transaction
    (fail closed), and again at redemption under the live claim.
  - The Web check is cosmetic; Main enforces.
- **Credentials by reference.**
  - The decision, card, wake input row, events and logs carry only an opaque reference.
  - The token exists only in the materialized input in Main's content response, and in Worker memory
    (`MaterializedInput(Zeroizing<Vec<u8>>)`, no `Clone` or `Debug`, `src/transport/input_content.rs:156`).
  - The Worker presents it only to the toolset whose frozen server is its resource (A2).
- **No enumeration.** Every credential refusal is the same 409 with the same text. The slog capture proves no
  reference or token is logged.
- **Redaction.**
  - `FetchedDecision` `Debug` redacts the reference.
  - The Worker and runtime TRACE captures (E2) and the checkpoint scans hold no reference and no token bytes.
- **D1 option (A) residual (A1).** The toolkit named by the card is not checked against the run's frozen membership,
  at Decide or at redemption.
  - **Why it is bounded.** D1 exists to stop a card from steering a credential to another server. That is enforced:
    the grant is bound to the canonical resource that the sign-in exchange checked against that toolkit's own
    settings when it saved the grant (`internal/api/v2/eliteacore/mcp_delegated_tokens.go:53,75-76`). Decide and
    redemption require the card's server to canonicalise to that same resource. The Worker presents the token only
    to the frozen toolset on that resource.
  - **What remains.** A card naming a project toolkit that isn't in this run yields nothing beyond the actor's own
    grant, for that toolkit's own server, which the actor just approved.
  - **Follow-up.** Registered as FU-277: frozen membership at redemption, through a bounded child-version reader.
- **Egress.** The Worker's MCP client guard is unchanged. The new challenge route fetches only through Main's guarded
  authorization source, from a URL read from storage after authorization. The browser fixture's mock is admitted only
  in the throwaway stack's overlay, by its IP literal and port (see Fixtures).
- **The challenge answer** carries no token, client secret or stored setting (exact-key-set test); it is
  `no-store`. Existence is not enumerable beyond what the project permission already shows.
- **Audits.**
  - No dependency file changed in this branch.
  - `cargo deny --all-features check advisories` (Worker) reports pre-existing advisories only: 3 unmaintained
    (`rustybuzz`, `ttf-parser`, `unsafe-libyaml`) and vulnerabilities in bundled root certificates, certificate
    verification, a wikilinks parser and a constant-time comparison. All are in unchanged lock entries.
  - `govulncheck ./...` (Main) reports GO-2026-6809 in the Go standard library (toolchain), which is pre-existing.
  - `npm audit --omit=dev` (Web) finds 0.
- **Neutral wording.** No finding detail is in this document.
- **`/security-review` (inline, once, on the merged branch).** No finding at or above the reporting bar. Per
  `rules/security.md` category:
  - *Trust boundaries and identity:* identity only from the verified principal (`delegatedAuthIdentity`); a token
    without an owning user is refused (challenge matrix).
  - *Object-level authorization:* Decide checks the run actor and the grant's full binding in its transaction
    (`checkDelegatedCredential`); redemption takes project and actor from the claim's authorization
    (`configurations_materializer.go:114`); the challenge route takes the run permission (`predict.post`) and resolves only an
    actor-visible toolkit, with one 404 for unknown, foreign and invisible toolkits.
  - *Input and amplification:* references bounded (16, fixed sizes, int32 ids, https only); the challenge reads no
    body and its discovery is size-, count- and time-bounded by the existing fetcher.
  - *Injection:* parameterized SQL only; no templates or shell; URLs are parsed and canonicalised, never built from
    request text.
  - *Egress and SSRF:* the challenge URL comes from storage after authorization and is fetched only through the
    guarded authorization source (private ranges need an operator IP or CIDR entry; no redirects off-origin).
  - *Secrets:* tokens never leave Main's materialized input and Worker memory; references, decisions, cards, logs and
    the challenge answer hold no token (tests above; browser log check). No secret in the diff (scanned per commit).
  - *Supply chain:* no dependency change in this branch's own commits (the merge brings `main`'s).

## Recovery rows (gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Main × P08/P13: authorize decided, Main restarts before the wake | I (the wake is admitted from the DECIDED rows) | `execution_interrupt_wakes.go:487`, `continue_fanout_wake.go:211` | `TestAWakeCarriesOnlyDecidedAuthorizeReferences` |
| Worker × P08/P13: the wake is lost after redemption, before or inside the member's call | R / I / C → F, as the existing member rows (the reclaim re-materializes; the call's node journal decides) | `parallel.rs:1606`, `configurations_materializer.go:319` | Existing P13 member rows (`pipeline_fanout_effect_pg_tests.rs`); redemption per claim: `TestAgentWakeReferencesResolveForTheClaimActorOnly` |
| Main/Worker × P08: the grant was revoked or expired before redemption | F typed (`graph.fanout.credential_unavailable`, readable text); a re-raise is FU-276 | `parallel.rs:87,1638,2243` | `an_authorize_without_its_credential_ends_the_branch_typed`, `TestAgentWakeReferencesResolveForTheClaimActorOnly` |
| Worker × admission: an older Worker meets an authorize wake (M1) | I (retry-later until a capable Worker claims it) | `claims.go:197` | `TestAWakeWithAnAuthorizeIsGrantedOnlyToAnOptedInWorker` |
| Worker × P13: a delegated-auth card from an older Worker, re-raised after the upgrade (M2) | R (byte-identical, Skip-only) | `events.rs:5264` | `a_delegated_auth_card_names_its_toolkit_only_when_its_pause_stored_it`, `TestAnOlderCardOrNoGrantStoreRefusesAuthorize` |
| Main × P08 before a decision: the sign-in challenge request is lost or Main restarts | I (read-only and stateless; the next click asks again; the card stays decidable) | `mcp_authorization_challenge.go` | `LedgerSignIn.test.tsx` retry case; browser: a 502 then a 200 on the same card |

## Real-browser evidence

Throwaway stack `fu250` (production profile, pre-defaults script; `http://fu250.localhost:19300`), created from the
real-model dump, torn down after this record. verify-profile 50 passed / 0 failed. Claude in Chrome, real network, no
response mocks. Evidence notes (redacted, mode 600) are in the handoff folder, `fu250/EVIDENCE-20261103.md`.

- **Images.** Main and Web `fu250-000780f8e`, Worker `fu250-ced100a47`; every other service unchanged. Each image was
  binary-checked against its commit (`docker create` + `docker cp`): the Main binary holds the route and its
  `authorization_discovery_timeout` code (the earlier image neither); the Web bundle holds the route call and the
  refusal text (the earlier one neither); the Worker binary holds `metadata_toolkit_id` (the earlier one not).
- **Fixture.** Project 118, MCP toolkit "fu250 mcp auth" (id 1) on the mock's `/mcp-auth`, pipeline
  "FU250 parallel sign-in": a Parallel `gather` with two LLM members, each told to call `echo`. Created in the UI.
- **Chat 3, older Worker.** Both cards Skip-only: the requirement gap fixed in `ced100a47`.
- **Chat 4, fixed Worker, earlier Main and Web.** Authorize enabled, but the click did nothing: the Web slip above.
  Roman skipped both cards; both members finished, so Skip worked end to end.
- **Chat 5, all three fixed images.**
  1. Authorize called `POST …/mcp_authorization_challenge/prompt_lib/118/1`. The first answer was a typed 502 (the
     fixture's allowlist error, see Fixtures) and the card showed "The MCP server's sign-in details could not be
     read. Try again, or Skip." After the fixture fix the same click got 200.
  2. The dialog opened ("Server: https://mcp-mock:8443/mcp-auth", "Supports automatic client registration") with
     Authorize enabled. Registration through Main's DCR proxy: the mock answered `POST /register` 201.
  3. The sign-in popup opened `https://mcp-oauth.localhost:19309/authorize`, and Chrome showed a certificate warning.
     Roman checked the leaf's SHA-256 fingerprint and accepted the local fixture warning in the browser himself. OS
     trust was unchanged: nothing was added to the keychain.
  4. Over three runs (the first and two regenerates) six cards were authorized. All six are `CONSUMED`, each decision
     `{action: authorize, credential_ref: <43-character opaque reference>}`; no decision holds a bearer.
  5. The mock: 6 authorization codes and 6 tokens issued, 16 authenticated `POST /mcp-auth` answered 200, 3
     unauthenticated 401 (the pauses), 0 OAuth refusals.
  6. Results: the second run's members returned "The echo tool returned "FU250-ONE"" and "…"FU250-TWO"". In the other
     runs a member saw the `echo` tool but asked before calling it; that is the model's choice, not the sign-in.
  7. After a reload the chat shows the same stored result, with no Authorize or Skip left.
- **No token in logs.** Over the run's 30 minutes no Main, Worker, Web or gateway log line holds a mock bearer
  (`test-only-` prefix). The mock's own log shows only the public `client_id` in the authorize URL.

## Fixtures

- The real-PG tests seed the cards through the production raise path and decide through the real ACK path. The
  delegated grants are saved through `mcpoauth.Tokens.Save` with a test key.
- The Worker tests use the in-process test ledger (`fanout_test_ledger.rs` `decide_with_credential`).
- Every credential string in a test carries a `test-only-` prefix.
- **Browser fixture (`99208955d`).** `deploy/mock-mcp/server.py` gained a test-only OAuth authorization server for
  `/mcp-auth` (protected-resource and authorization-server metadata, DCR, `/authorize` with S256 PKCE, `/token`); codes
  are single-use and bearers carry `test-only-`. Its browser-facing `/authorize` is served on a separate port only
  when `MOCK_MCP_PUBLIC_*` is set.
- **Certificate.** A dedicated name-constrained test CA signed only the leaf for `mcp-oauth.localhost` (valid to
  2026-11-05). The CA key was deleted after signing; the leaf key lived only in the stack's volume and was deleted
  after the proof. Nothing was installed into the OS trust store.
- **Stack deviations, all on the throwaway database or overlay:**
  - migration 0206 fails on a restored legacy dump (FU-279), so the five missing `audit_events` columns were added by
    hand before `up`;
  - toolkit create fails in a legacy-built tenant (FU-280), so the fixture lives in project 118;
  - the overlay first allowed `mcp-mock:8443` in `ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST`. A name entry never permits a
    private address (`libs/go/egresslib/addrcheck/classify.go:221-246`), so the guard refused the mock and the first
    challenge answered 502. The overlay now names the mock's IP literal and port, `192.168.80.11:8443`.

## Follow-ups

- **FU-276:** re-raise the card instead of ending F when the grant expired before redemption.
- **FU-277:** frozen membership at redemption (child-version reader).
- **FU-278:** a local-only timeout in `TestCorpusOnlySchemaSupportsFoldersAndSelectedConversations` (CI passes).
- **FU-279:** migration 0206 fails on a restored legacy dump.
- **FU-280:** toolkit create returns 500 in a legacy-built tenant (no `elitea_tools.owner_id`).
- **FU-281:** a card named its toolkit by the `mcp_authorize` hash on an older Worker; chat 5 shows the name.
- **FU-282:** other Web callers of the unanswered `test_mcp_connection` socket check.
- **Minor, seen in chat 5:** after a refused first click, the message stayed visible under the other card while the
  dialog was open; the unit test shows a retry on the same card clears it. Re-check after merge; the two cards'
  order also differs between chats.
- **Pre-existing, unchanged here:** the single-agent path sends raw session tokens in the encrypted bundle (out of
  scope per the review's Rev 2, item 1).
