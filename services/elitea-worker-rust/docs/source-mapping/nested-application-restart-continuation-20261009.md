# Nested application continuation after a Worker restart (2026-10-09)

Branch `fix/nested-application-restart-continuation`, based on `origin/main` `f7c6a6028` (#1194).

A pipeline with two nested application nodes, each calling a saved agent that asks the user a question, could not
finish if the Worker process was replaced while the first card was waiting. The first answer worked. Answering the
second card failed with "The runtime operation failed." The Worker logged `event="agent_native_assembly_failed"`
`error_code="native_agent.invalid_configuration"` `failure_reason=the nested application graph is invalid`. The
same flow without a restart completed on `main`.

## Root cause

| Question | Finding |
|---|---|
| Which `invalid_configuration()` fires? | `application_resume_history` (`src/agents/application_tools.rs:1667-1671` on this branch; `:1670` on `main`), the branch that refuses one child invocation id seen under two different parent-call routes. Found two ways: from the persisted session events of the failed chat (below), and from a backtrace in the failing real-PostgreSQL test (chain `install_nested_application_resume` `:695` → `build_nested_application_resume` `:741` → `insert_resume_decision` `:1135` → `application_resume_history`). |
| Why does the id repeat? | `ApplicationToolInvocationContext::with_resume` named every agent child `elitea-child-{N}` from a process-global `AtomicU64` that started at 1. A replacement Worker starts it at 1 again. |
| What did the history look like? | Chat 880, one session. Turn 1 (execution `e303526c…`, pre-restart): the parent call `pipeline:override_call:0` under `inv-ca795762…`; its child `elitea-child-1` paused. Turn 2 (execution `191d085b…`, after `docker restart`): the pipeline re-persisted the same call under `inv-0c350159…`; the **resumed** child was again `elitea-child-1`, and node 2's child `elitea-child-2` paused. Turn 3: `application_resume_history` maps `elitea-child-1` to (turn-1 call event, call id) and to (turn-2 call event, call id), so it fails closed. |
| Why does it pass without a restart? | The counter keeps rising: original child 1, resumed child 2, node-2 child 3. Every id maps to exactly one route. |
| Why did answer 1 work? | At that point the history held only the pre-restart `elitea-child-1` events; the duplicate is written by answer 1 itself. |
| Is it only a restart problem? | No. Any turn handled by another Worker (more than one replica, or a takeover) can draw an id already persisted in the same session. |

The other two signals named in the report are unrelated and pre-existing. Both appear in successful runs without a
restart too:
- `error_code="session.invalid_scope"` on `agent.session.persist operation="get"` spans: a scoped session writer is
  asked for a session outside its scope (the derived `elitea-model-<digest>` model-scope session). `get` reports this
  to callers as `session.not_found` on purpose, so existence is not revealed (`src/state/postgres_session.rs:1001`),
  and the model-scope path then creates that scoped session (`src/agents/model_scope.rs:307`). The span still records
  the internal code with `outcome="failed"` at INFO. This is noisy telemetry, not a failure.
- `event="agent_session_terminal_completion_unavailable"` (`src/agents/session.rs:1460`): it fires for a terminal model
  turn that holds only a tool call (here the `ask_user` call events), which has no streamed text to add. It is a
  noisy WARN, not a failure.

## Business behaviour

| Topic | Current platform (reference only) | New platform after this change |
|---|---|---|
| Two nested agent calls that each ask the user, with a Worker restart while paused | LangGraph replays from its checkpoint. A worker crash mid-run is "lost, run again" (not ported). | Both cards resolve independently. The continuation after any number of process replacements completes from PostgreSQL, and completed node 1 is not re-run. |
| Nested agent child identity | Python run ids (random) | `elitea-child-<uuid v4>`, the same scheme ADK uses for root `inv-<uuid v4>` invocations. Unique for every activation, including each resume of the same parent call. |
| A history that already holds one child id under two parent calls (written by an older Worker) | n/a | Fails closed with its own typed cause `nested_application.ambiguous_child_invocation`, "a nested application invocation is bound to two parent calls". It is never guessed or merged. |

## Changed paths

| Path | Change |
|---|---|
| `services/elitea-worker-rust/src/agents/application_tools.rs:4860-4864` | The child invocation id is `elitea-child-<uuid v4>`; the per-process `NEXT_INVOCATION` counter is removed. |
| `services/elitea-worker-rust/src/agents/application_tools.rs:1670,5222-5234` | The route-conflict refusal returns `ambiguous_child_invocation()`, still `InvalidConfiguration`, with cause code `nested_application.ambiguous_child_invocation`. The lifecycle logs it as `cause_code` (`src/execution/native_agent_lifecycle.rs:292`). The browser message is unchanged. |
| `services/elitea-worker-rust/Cargo.toml:81-83`, `Cargo.lock` | Direct dependency `uuid = "=1.24.0"` (`v4`). It was already locked through `adk-runner` with `v4` enabled, so no new crate enters the graph. The only lockfile change is `uuid` in the Worker's own dependency list. |
| `services/elitea-worker-rust/src/agents/pipeline_restart_pg_tests.rs` (new) | The real-PostgreSQL process-replacement proof. |
| `services/elitea-worker-rust/src/agents/pipeline_tests.rs:54-55` | Registers it as a child module, so it reuses the pipeline fixtures. |
| `services/elitea-worker-rust/src/protocol/control.rs:1535-1551` | `#[cfg(test)]` helper `test_session_authority_for_claim`: a later execution's own claim on the same session. |

The branch ordinal (`nested_application_branch`, `application_tools.rs:1083`) is also per process and repeats after a
restart. It was left unchanged on purpose. It only makes a child branch a strict descendant of its parent, and
nothing groups events by it: every use compares parent and child tiers (`application_tools.rs:1008,1063-1067,3964`).

## Crate ownership (ADR-0027 layout)

| Mechanism | Owner | Changed here? |
|---|---|---|
| Nested application tool, child invocation context, resume reconstruction | Worker (`src/agents/application_tools.rs`) | **Yes** |
| `NativeAgentAssemblyError` and its cause | `libs/rust/agent-runtime` (`src/assembly_error.rs`) | No, used as is |
| Session and checkpoint stores | Worker (`src/state/`) | No |

`libs/rust` is unchanged, so its suites were not re-run for this branch.

## Tests

The real-PostgreSQL proof was written first and failed on `main` in phase `answer_second` with
`NativeAgentAssemblyError { code: InvalidConfiguration }` from `application_tools.rs:1670`. Phases `start` and
`answer_first` passed.

| Test | Proves |
|---|---|
| `pipeline_restart_pg_tests.rs:150` `postgres_two_nested_agent_pauses_resume_across_process_replacement` (child `:229`) | Three separate OS processes of the test binary, one per turn, against one isolated database. Each phase goes through the production `PipelineNativeAgentAssembler::postgres` and `activate_pipeline_postgres`, as a new execution with its own claim on the same session, as the browser does. `start`: node `override_call`'s child pauses (1 model call). `answer_first`: in a new process, card 1 is answered, node 1 completes, node `defaults_call`'s child pauses with a different card (exactly 2 model calls; node 1 is not re-run). `answer_second`: in a new process, card 2 is answered and the pipeline completes (exactly 1 model call). A checkpoint holds both child answers, and the conversation session holds four distinct child invocation ids: the original and resumed child of each node. Each phase process is bounded at two minutes and killed on expiry, so a stuck turn fails the proof instead of hanging CI. |
| `application_tools.rs:5689` `a_child_invocation_id_under_two_parent_calls_fails_closed_with_its_reason` | Distinct child ids each keep their own route. A history with one id under two persisted parent calls (as an older Worker wrote it) fails closed as `InvalidConfiguration` with cause `nested_application.ambiguous_child_invocation`. |

Counts (local, `--offline --locked`, `ELITEA_TEST_DATABASE_URL` on the loopback PG 18.2 of this branch's own
disposable stack, with `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` and `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`):
- Worker `cargo test --all-targets --all-features`: 1846 passed, 0 failed, 71 ignored. The ignored set is the same
  Docker- and Kubernetes-gated tests as on `main`. No PostgreSQL test skipped.
- `cargo fmt --all -- --check` and `cargo clippy --all-targets --all-features -- -D warnings` are clean.
- Without the receipt guard variables, the two `graph_receipts` PostgreSQL proofs refuse to run against a database
  URL by design. They are not a regression.

## Performance

| Rule / budget | Mechanism | Measured |
|---|---|---|
| No new I/O | The id is generated in memory. No database statement, round trip or event is added. | The PG proof's model-call counts per turn (1, 2, 1) are unchanged from `main` behaviour, and node 1 is never re-run. |
| O(1) per child activation | One `Uuid::new_v4()` (one OS random read) replaces one atomic increment. Children per turn are already bounded (`MAX_PARALLEL_APPLICATION_CALLS`, `MAX_AGENT_TIERS`). | No measurable change. The three-process PG proof runs in 2.0 s. |
| Bytes | The id grows from about 14 to 49 bytes. It appears in each persisted child event's `invocation_id` and in model event ids (`<invocation>_llm_<n>`). | About +70 bytes per persisted child event, well inside the existing session event and identity bounds (identity validators allow 256-512 bytes; root ids are already 40 bytes). |

## Durability

| Rule | Mechanism | Proving test |
|---|---|---|
| Stable, unique identity across retries and restarts | A child activation id never repeats in a session, whichever process or Worker writes it (`application_tools.rs:4864`). | `pipeline_restart_pg_tests.rs:150`: four distinct ids across three processes. |
| Completed work is never re-run | Node 1 completes from its checkpoint; only the paused child resumes. | Same test: model-call counts 1, 2, 1. |
| Fail closed on an ambiguous history | A collided history is refused with a typed cause, never merged (`application_tools.rs:1670`). | `application_tools.rs:5689` |

Crash windows touched: (1) Worker replaced while a nested agent card is pending (proved, R); (2) Worker replaced
between a resumed child finishing and the next node pausing — the next turn starts from the PostgreSQL checkpoint and
the same id rule applies (covered by the same proof's third process).

## Resilience

- Every failure stays typed: `InvalidConfiguration` keeps its browser mapping, and the new cause code says which rule
  refused the history.
- No silent coercion: a history with an ambiguous child id is never re-joined by guesswork.
- Nothing new is unbounded; no new task, channel, lock or retry.

## Security

| Category (`rules/security.md`) | Applies? | How checked |
|---|---|---|
| Trust boundaries / identity | No change | The child id is a correlation id built inside the Worker. It is not an authorization input; authority still comes from the claim (`activate_pipeline_postgres` checks tenant, projects, execution and generation). |
| Object-level authorization | No change | No new route, RPC or read. |
| Input / amplification | No change | No new parser. Existing identity bounds hold with the longer id. |
| Injection / construction | No | No SQL, URL, path, template or shell is built from the id. |
| Egress | No | No outbound call. |
| Secrets | Yes | The new cause and message are `&'static str` with no data. The diff secret scan was clean. Evidence in this file holds ids only, no prompts or answers. |
| Supply chain | Yes | `uuid` was already in the locked graph through `adk-runner` with `v4` enabled; it is now an exact direct pin, so no new crate is compiled. `cargo deny --all-features check advisories`: only pre-existing RUSTSEC-2023-0071 (`rsa` via `sqlx-mysql`, recorded unreachable in `dependency-advisories-20261008.md`). `check licenses` fails on all 473 crates on `main` as well, because the Worker has no license allowlist; that is pre-existing and not the gate. |

## Recovery guarantee rows

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × fan-out child (P12), nested agent child paused on `ask_user`, process replaced, then answered | R | Unique child id (`application_tools.rs:4864`); resume from session events and the pipeline checkpoint (`session.rs:1837`, `application_tools.rs:689`) | `pipeline_restart_pg_tests.rs:150`; browser runs below |
| Worker × HITL decision (P07) for a later node after a replacement | R | Same | Same test, third process; browser card 2 |
| Worker × admission of a continuation whose history an older Worker already collided | F (typed cause) | `application_tools.rs:1670,5228` | `application_tools.rs:5689` |
| PostgreSQL, Main, NATS, LLM gateway, sandbox supervisor, Web | Not touched | — | — |

No new L. The platform matrix (`docs/recovery-guarantees.md`, Worker × P12) is owned by #1084 until it merges; the
row above is to be added there afterwards.

## Real-browser evidence

Own standalone stack `elitea-nestedapp` (`http://nestedapp.localhost:18500/app/`, `STANDALONE_HOST=nestedapp.localhost`,
ports 18500-18507 and 19600), restored from `product-real-models-main-c0f2e5f9b.dump` (shared migrations 158, tenant
148). Mock OIDC login as `admin@centry.user`. Real model behind the gateway. No response mocks. Every restart is
`docker restart elitea-nestedapp-elitea-worker-1` while a card is pending, followed by a full page reload.

Images: Main, Web, gateway, scheduler and sub-app host `main-c0f2e5f9b-verify`.
- Worker baseline `elitea-worker-rust:main-c0f2e5f9b-verify` (image `sha256:500d9904cc77…`, binary sha256 prefix
  `8fc66e61e8c149ed`).
- Worker fix `elitea-worker-rust:nestedapp-restart-fix` (image `sha256:ae9ac53196a5…`, binary sha256 prefix
  `90c3b8d04bb7e8f4`), built from this branch at `c044c2468`, which contains #1160 (`524165ed09`). It was tagged
  separately so the shared `main-c0f2e5f9b-verify` tag is untouched. Binary check: `docker create` + `docker cp` of
  `/usr/local/bin/elitea-worker-rust`. The fix binary contains `nested_application.ambiguous_child_invocation` once
  and "bound to two parent calls" once; the baseline binary contains neither.

| # | Worker | Chat | Executions (turn) | Result |
|---|---|---|---|---|
| 1 | baseline | 880 | `e303526c1c781ec8b98257b178441f34` (message → card 1), restart, reload; `191d085b0b6a19bd033dfc94c6088f9f` (answer 1 → card 2); `d9a7a615052faa08a522c4850bc7cfd0` (answer 2) | Reproduced: card 1 and card 2 appear, answering card 2 shows "The runtime operation failed." Worker: `native_agent.invalid_configuration`, "the nested application graph is invalid". The session holds `elitea-child-1` under both turn 1 and turn 2 (the restart reset the counter); node 2's child is `elitea-child-2`. |
| 2 | fix | 882 | `ef3a26450cb23452331a85680626f3f5` (message → card 1, SUCCEEDED), restart, reload; `80b4be4ad9a75e5b701a70a4d1bc3bb0` (answer 1 → card 2, SUCCEEDED); `b964f8dfd9ed553a80f5441711a84891` (answer 2, SUCCEEDED) | Card 1 survives the restart and reload. Answering it completes `override_call` and raises card 2 with different options. Answering card 2 completes the pipeline: the reply shows an "OVERRIDE CALL" and a "DEFAULT CALL" section, each answered with its own variable bindings (the override binding for the first call, the saved defaults for the second). It is unchanged after a full reload. Child invocation ids in the session, in order: `elitea-child-81c2b8fd…` (turn 1), `elitea-child-9ae7b135…` and `elitea-child-6802cf84…` (turn 2), `elitea-child-d35e574a…` (turn 3), all distinct. No `agent_native_assembly_failed` and no ERROR in the Worker log. |

An earlier attempt on the fix image (chat 881; executions `d432e10769ca8585731c253889acf2e8` and
`cb509089c6c99a260448d1e3b7020571`) did not reach card 2. The answer-1 turn stopped after node 2's child received its
`ask_user` call and waited until the Worker was restarted again, after which Main settled it `FAILED` (the existing
F class for a pipeline application node interrupted mid-run). PostgreSQL showed a lock wait inside the Worker's own
session writes, not an assembly failure:
- one connection was `idle in transaction`, holding the root and child model-scope writer rows, with its
  `merge_session_state` reply unread;
- a second connection waited on the root writer row.

The mechanism is independent of the invocation-id change. It is a cancellation hazard in the `select!` stream
wrappers, which can stop polling a child's in-flight model-scope append while the Runner appends a forwarded event
to the root session. It is recorded as follow-up 5 below. The later run (row 2) on the same image completed.

## Fixtures

All fixtures come from the real-model dump `product-real-models-main-c0f2e5f9b.dump`: pipeline 138 "Gate5 agent
variable parent 20260930" (version 145, project 2 "Private") with nested application nodes `override_call` and
`defaults_call`, both calling agent 137 "Gate5 variable agent 20260930" (version 144). Nothing was created or edited
through the database. Chats were started from the pipeline's **Chat** button in the UI. The test fixtures are
in-process: the scripted model gateway and runtime-context RPC fixtures of `pipeline_tests.rs` over a real
PostgreSQL database.

## Review

`code-review` (high) on #1199 gave five findings:
- The phase processes had no deadline. **Fixed**: 2-minute bound with kill.
- The distinct-id assertion read model-scope sessions too. **Fixed**: it reads only the conversation session.
- `Uuid::new_v4()` panics if the OS random source fails. **Kept**: ADK's runner already makes the same call for
  every root invocation, so this adds no new failure class.
- `LLM_INVOCATION_SEQUENCE` and the branch ordinal are per-process counters. **Not changed here**: see follow-up 3
  and the branch note under Changed paths.

`security-review` on the branch diff: no findings. The child invocation id is a join key and a digest input, never
an authorization or scoping credential (session authority is the claim). The new cause and message carry no data.
The id stays inside the existing identity bounds. The dependency change adds no crate or source.

## Follow-ups

1. Add the Worker × P12 row above to `docs/recovery-guarantees.md` once #1084 merges.
2. Conversations that crossed a Worker restart on an older image and still have an unanswered later card now fail
   with the typed `nested_application.ambiguous_child_invocation` cause instead of a generic one. They cannot be
   repaired safely, because the persisted events of two activations share one id. A new message starts a clean turn.
3. `LLM_INVOCATION_SEQUENCE` (`src/agents/graph/llm.rs:61`) is another per-process counter. Pipeline LLM-node
   invocation ids and the call ids synthesized for providers that omit them (`llm.rs:1507-1516`) repeat after a
   restart. No failure was seen; it should get the same review.
4. Telemetry noise: record the expected out-of-scope `get` as a miss rather than `outcome="failed"`, and skip the
   `terminal_completion_unavailable` warning for tool-call-only model turns.
5. Session-lock wait in the stream wrappers (seen once in the browser, chat 881). `graph/node_events.rs:415-470`
   runs a `biased` `select!` between forwarded node events and `root_events.next()`. The graph, including a nested
   child's model-scope append (`model_scope.rs:547-588`), which takes a share lock on the root writer row
   (`state/postgres_session.rs:455-466`), runs only while `root_events` is polled. If a forwarded event wins while
   that append waits on PostgreSQL, the Runner's root-session append waits on the suspended append's lock, and
   nothing polls it again. The same shape exists in `application_tools.rs:4373`, `application_pipeline.rs:1563` and
   `pipeline/scoped_runtime.rs:245`. There is no lock or statement deadline, so the turn waits until the process
   ends. This needs a deterministic real-PostgreSQL reproduction and a cancellation-safe wrapper; it is filed as its
   own task.
