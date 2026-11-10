# Errors and logs a person can read: the core (FU-299), 2026-11-10

Every WARN or ERROR about a failure is becoming one **fault line**:
- a plain sentence and a stable code from one registry shared by Go and Rust;
- the ids, and a server-generated support reference;
- the cause chain, made safe by type;
- for panics and unexpected errors, a bounded stack.

This PR is the core: the convention, the gates, the shared plumbing every existing line benefits from, and two worked
cases. The sweep over the existing errors is the polish pass.

Plan and reviews: `handoffs/point5-wave1-20261008/fu299/PLAN.md` and `REVIEW-PLAN.md`. The decisions were Q1 JSON
registry, Q2 reuse `execution_id` or the request's server reference, Q3 JSON lines, Q4 keep `err`, and Q5 as
modified by C3. Conditions C1–C7 and notes N1–N3 are covered below.

## Business behaviour

- **Nothing is ported.** The current platform has no error registry; this is new.
- **The "what broke" guide.** `docs/errors/README.md` is generated from the registry and drift-checked. Its operator
  section explains the support reference, the one-line-each-way outage convention, the DEBUG demotion during a
  reported outage and the suppressed counts (N3), and `ELITEA_RUST_SPAN_TIMING=stderr` for performance evidence.

**Not done here (the polish sweep):**
- migrating the other old-style codes, frozen in `libs/go/faults/legacy-codes.txt`: 463 lines, shrink-only;
- the other HTTP error writers;
- making the registry the source of the `RuntimeFailureKind` table;
- the remaining per-site `recover()` calls;
- FU-300's empty-tool-list behaviour itself;
- FU-298.

**The LLM gateway is not in the core (FU-303).** It needs Go 1.27.3, which was not available to build and verify it.

## Changed paths

**Shared convention (Go, `libs/go/faults`, new `go.work` module)**

| Path | What |
|---|---|
| `registry/{main,worker,runtime}.json` | The single registry. Per owner: Main's 9 codes, the Worker's 10, and the 53 imported runtime failure messages (byte-identical, `runtime_kind`/`wire_code` set). |
| `registry.go`, `codes_gen.go` (generated), `render.go`, `cmd/faultsgen` | Embedded registry, generated `Code` constants (72), the guide renderer. `go generate ./libs/go/faults` writes `codes_gen.go` and `docs/errors/README.md`. |
| `fault.go:25-185` | `Error`: `New`, `Unexpected` (`:40`, bounded stack), `Panic` (`:46`, the value's **type** only, never its text), and `Log` (`:76`), which writes one fault line with `code`, `action` and `stack`. `CauseChain` (`:97`) walks the chain to at most 8 elements of at most 512 bytes, and uses the service's `Describer` for known types. Stack: at most 32 frames and 8 KiB, at most one per code per second (`captureStack`, `:165`) (C2). |
| `supportref.go` | `NewSupportRef` (`sr-` plus 16 random hex digits, server-generated), and the context helpers. |
| `transitions.go` | Per-dependency outage tracker (lost and back once per outage). `Confirm` asks the owner to check now. |
| `cycle.go` | `CycleReporter` (C3): DEBUG only while a tracker the failure may belong to is lost, after confirming; otherwise `runner.cycle_failed` at ERROR at most once a minute per runner and failure class (`db`, `nats`, `other`), with `suppressed`. Suppressed failures still go to DEBUG. |
| `logx/handler.go` | The shared slog handler. `err` stays today's string (Q4). It adds `cause`, plus `code`, `action` and `stack` for a typed fault, and the context's `support_ref`. `Wrap` keeps a service's existing handler and level (C6). |
| `httperr/write.go` | The one error writer: logs the fault line, then answers `{error, code, support_ref}`. `KeepOKFalse` keeps a route's existing `ok:false`. A nil request is allowed. |
| `grpcrecover/recover.go` | Unary and stream interceptors: one `internal.panic` line, and `codes.Internal` with the sentence and code only. |
| `gate_test.go`, `legacy-codes.txt`, `multi-sentence-exemptions.txt` | The gates; see Tests. |

**Shared convention (Rust, `libs/rust/faults`, crate `elitea-faults`)**

| Path | What |
|---|---|
| `build.rs` | Reads and validates the same registry files with the Go gate's rules byte for byte (sentence and action non-empty, at most 256 bytes, ending with a period; no `{`, `%`, uuid prefix, 12+ hex id, `://`, path, source-file name, `/`, `\`, run of 5+ digits or `www.`), plus strict keys, `version` 1, no duplicates and no colliding constant names. The one-sentence rule stays in Go, so its exemption list has one owner. Generates `codes::*` and `REGISTRY`. |
| `src/lib.rs` | `Code` (only the crate constructs it), `Fault`, `SafeCause` (`io::Error` renders as its kind; an unregistered type renders as `"unregistered error type"`, never as text), `cause_chain` (a wrapper repeating its inner error's text keeps only its own part, as Go's chain does), `fault!`, and `in_operator_stream()`, a thread-local mark set while the JSON layer runs, cleared on unwind. |
| `src/operator_stream.rs` | `OperatorJsonLayer`: one JSON object per event, span fields flattened, no span events. |
| `src/logtest.rs` | The one shared child-process log capture. |

**Main**

| Path | What |
|---|---|
| `cmd/elitea-main/main.go`, `fault_logging.go` | `logx.New` with a describer for `*pgconn.PgError`/`*pgconn.ConnectError`, which renders their SQLSTATE and message without connection details. The level stays Info (C6). One `faults.Transitions` is shared by the readiness monitor (`WithOutages`) and the runtime composition (`Dependencies.Outages`). |
| `internal/api/middleware/supportref.go`, `router.go` | **C1.** Every request gets a fresh server reference, put on the context, returned in `X-Elitea-Support-Ref` (also exposed through CORS), and never taken from `X-Request-Id`. |
| `internal/api/middleware/recover.go` | A panic is `faults.Panic` plus `httperr.Write`: a JSON 500 with the sentence, code and reference, and one fault line with method, route and a bounded stack. **The old line logged the panic value itself.** |
| `internal/transport/runtimegrpc/{private_servers,platform_server}.go` | Recovery interceptors on the control, output and platform listeners. |
| `internal/api/v2/toolkitrun/response.go` | **FU-300.** `WriteRequestError(w, r, err)`: the 503 is `toolkit.settings_unresolved`; the default 500 is `internal.unexpected` with a stack. Both are logged once and keep `ok:false`. `WriteError(w, err)` stays for its existing tests. |
| `internal/api/v2/toolkits/{handler,test_tool_result}.go`, `internal/api/v2/indexing/start_handler.go` | The four handlers call `WriteRequestError`. |
| `internal/runtimecomposition/cycle_reporting.go`, `composition.go` | The 12 per-cycle callbacks go through one `CycleReporter` per runner, memoized per composition. |
| `internal/infra/depfailure/depfailure.go` | `Dependencies(err)` returns the trackers a failure may belong to: `["db"]` for PostgreSQL types, `["nats"]` for NATS sentinels, `["db","nats"]` when a transient failure is ambiguous, none otherwise. |
| `internal/api/health/readiness.go`, `cmd/elitea-main/readiness_checks.go` | `Check.Code`. `WithOutages` shares state with the reporters, and `confirm` is single-flight: concurrent callers wait for one evaluation, and evaluations run at most once a second. The loss lines are `dependency.database_unavailable` and `dependency.nats_unavailable`. |
| `internal/runtimecomposition/publishers.go`, `internal/transport/workloadauth/authorizer.go` | `runner.restarted`, `runner.stopped` and `workload.session_store_unavailable` codes on the existing lines. |
| scheduler, subapp-host, egress-proxy, `elitea-migrate`, `elitea-keda-cleanup` | `logx.Wrap` around the same handler options. The levels are unchanged: Info, Info, `cfg.LogLevel`, nil (Info) and nil (Info). |

**Worker**

| Path | What |
|---|---|
| `src/diagnostics.rs` | **Operator stream.** stdout is JSON lines (`OperatorJsonLayer`): events only, span fields flattened, `ELITEA_RUST_LOG` unchanged. Span timing goes to OTEL; `ELITEA_RUST_SPAN_TIMING=stderr` adds a stderr layer with span close events. **Panic hook.** Every panic writes one redacted stderr line (`payload redacted; source=<file>:<line>`), even with logging off. When ERROR is enabled it also logs one `internal.panic` line carrying the payload type, `file:line`, the span fields, and a backtrace of at most 8 KiB, one per second counted from the end of the last capture, else `"rate-limited"` (C2). A panic raised inside the operator stream itself, or inside the hook, writes the stderr line only and never re-enters tracing (no deadlock, no abort). |
| `src/execution/{output_delivery,agent_delivery_processor}.rs` | `Fault` for `AgentProgressPublishError` and `AgentOutputPreflightError`. The two WARNs go through `fault!`, keeping `event`, `error_code`/`result_code` and `retryable`. |
| `src/execution/{agent_delivery_processor,toolkit_delivery_processor,native_agent_lifecycle}.rs` | `support_ref` (= `execution_id`) on the execution span roots. |
| `src/protocol/output.rs` | The parity test: every registered runtime failure is in the registry byte-identically, and the other way round. |
| `src/{spool.rs,transport/output_grpc.rs,transport/output_session.rs,protocol/error.rs}` | Safe renderers for the Worker's own data-free error types. |
| `Containerfile` | Copies `libs/go/faults/registry`, which the build script reads. |

## Consumers of Worker logs (C4)

The Worker's stdout changes from compact text to JSON lines, so every reader of it was searched on the branch head. The
search covered the repository and the local handoff tooling for:
- `docker logs`, `compose logs` and `stack logs` of the Worker;
- `subprocess` readers of container output;
- text-format patterns (`event=…`, `time.busy`, `time.idle`, span `close`);
- log collectors in `deploy/` (`filelog`, fluent, promtail, loki, logging drivers);
- Rust tests that spawn the Worker binary;
- runbooks that grep Worker lines.

| Consumer | What it does with Worker output | Change |
|---|---|---|
| `apps/elitea-web/scripts/chat-stream-e2e.sh`, `deploy/scripts/{native,governed}-conformance.sh`, `deploy/scripts/handoff-live.sh` | dump `stack logs elitea-worker …` for a person when a run fails; no parsing | none needed: JSON lines are as readable in a dump |
| `scripts/runtime/probe_code_{runner,adapters}_container.py` | compare `docker logs` of a **code runner** container byte for byte | none needed: not the Worker |
| `deploy/` (Helm, compose, OTel collector) | no log-file collector; the collector takes OTLP, and `ELITEA_RUST_LOG` is unchanged | none needed; span timing still reaches OTLP |
| `production-profile*/stack.sh` and `verify-profile` (handoffs) | read only PostgreSQL's container log during restore | none needed |
| Crash-recovery suite (`handoffs/crash-recovery-suite`) | its DESIGN describes matching `crashpoint.arrived name=X` in container logs, but no crashpoint emitter or matcher exists in code yet | none now; whoever builds the suite matches the JSON `event`/fields (`recovery-guarantees.md` mentions crashpoints only as design) |
| Worker tests that read log output | `runtime_log_safety_tests.rs` and the diagnostics tests capture the subscriber in a child process through the shared `elitea_faults::logtest` | the one approved setup edit (Tests); no test spawns the Worker binary and parses its stdout |
| Operators and runbooks | no runbook in `docs/` or `deploy/` greps a Worker text line | the upgrade note tells log pipelines to parse JSON and match `code`/`event` |

## Where each gate runs (C5)

All the Go gates are tests in `libs/go/faults`. `scripts/go/workspace-run.sh` tests every `go.work` module in the
`ci-go.yml` test, coverage and lint jobs, and its path filter includes `libs/go/**`. No workflow changed.

| Gate | Test |
|---|---|
| Registry well-formed (C7) | `TestRegistryIsWellFormed`: grammar; sentence and action non-empty, at most 256 bytes, ending with a period; no `{`, `%`, uuid, hex id, URL, path or source-file name; audience, retryable and owner valid. One sentence, except the 53 imported runtime codes in `multi-sentence-exemptions.txt`, each of which must be an imported runtime failure. Mutation-checked. |
| Generated files current | `TestGeneratedCodesAreCurrent`, `TestGuideIsCurrent` |
| No hand-made code | `TestNoCodeIsMadeByHand`: an AST walk of every Go file refusing `faults.Code("…")`. Rust: `Code` has a private constructor. |
| No orphans | `TestEveryRegisteredCodeIsUsed`: every non-runtime code is named in Go or Rust source. |
| Old-style codes frozen | `TestLegacyCodesOnlyShrink`: 463 Rust `code()`/`as_str()` literals and Go `"reason"` literals. A new literal fails the test, and a line no source raises any more must be deleted. |
| Rust registry | `elitea-faults/build.rs` (compile time) plus `registry_is_well_formed_and_codes_are_unique`, run by `ci-rust-shared.yml`. The Worker parity test runs in `ci-rust.yml`. |

**Workflow change (approved by Roman, 2026-11-10).** `ci-rust.yml` and `ci-rust-shared.yml` now include
`libs/go/faults/registry/**` in their path filters. A registry-only change therefore also runs the Rust build that reads
the registry (`elitea-faults`' `build.rs` and the Worker parity test). They are the only Rust workflows whose build reads
it: only the Worker and the `libs/rust` workspace depend on `elitea-faults`. `ci-web-e2e.yml` already filters on
`libs/go/**`. No other workflow line changed.

## Tests

Counts were taken on the branch head after both review rounds. Labels name the boundary each test crosses.

| Suite | Boundary | Result |
|---|---|---|
| Go, every package of `libs/go/faults`, Main, scheduler, subapp-host and egress-proxy (`go test -json ./...`, with `ELITEA_TEST_DATABASE_URL` on a disposable PostgreSQL 17) | unit + service-integration (real PostgreSQL) | **21,835 passed, 129 skipped, 16 failed.** All 16 failures are in `authsession`/`authflow`, which this branch does not touch: `out of shared memory (SQLSTATE 53200)` while applying shared migrations in parallel on the untuned local container. CI's PostgreSQL is the reference. |
| `libs/go/faults` gates | unit | pass: registry well-formed, generated files current, no hand-made codes, no orphans, legacy codes only shrinking |
| C3 real-PG (`runtimecomposition/fault_cycle_postgres_integration_test.go`) | service-integration | pass, 3 runs: a database cut gives one loss line and 0 ERROR; a timing-out query on a healthy database gives 1 ERROR a minute with no dependency-demoted DEBUG; 12 runners failing at once give 0 ERROR |
| C1 forged identity (`middleware/supportref_test.go`) | unit (HTTP) | pass |
| FU-300 fault line (`toolkitrun/fault_line_test.go`) | unit (HTTP) | pass: one line, `support_ref` equals the body's; 503 at WARN; 500 at ERROR with a stack |
| `elitea-faults` (`cargo test --locked --all-targets --all-features`) | unit | 10/10 |
| Worker lib (`cargo test --locked --lib --all-features`) | unit + component | 3116 passed, 0 failed, 111 ignored |
| Worker / `elitea-faults` clippy (`--all-targets --all-features -D warnings`) | static | clean |

**New Worker tests:**
- the panic hook: one redacted line with logging off; re-entry from a panicking `Debug` writes stderr only and does not
  deadlock; the backtrace allowance;
- `progress_rejection_is_one_readable_fault_line`;
- the registry parity test (53 runtime texts, byte-identical in both directions).

**Edits to existing tests: one, approved.** The child setup in `runtime_log_safety_tests.rs` now also installs the span
timing layer, because the test reads span close fields and those are no longer on the operator stream.
`output_delivery_tests.rs` only gains imports and new tests. No existing assertion was changed or removed.

## Performance

- **Hot paths.** No new work on a request except one 8-byte random read for the support reference, and the handler's
  attribute pass on lines that carry an error.
- **Cause chains.** At most 8 elements of at most 512 bytes.
- **Stacks.** At most one per code per second, 32 frames, 8 KiB.
- **Monitor confirmation.** At most once a second, bounded by the 2 s readiness budget.
- **Per-cycle lines.** During a reported outage they are DEBUG; otherwise at most one per runner per minute, so a
  database outage no longer writes about 12 runners × 4 lines a second.

## Durability

Logging only: no state, no ordering and no outcome changes. The outage tracker is in memory and re-derived by the
monitor every 5 s.

## Resilience

- **Every bound is a named constant** (see Performance).
- **C3, never hidden.** A failure outside a reported outage is an ERROR, once a minute, with its count. That includes
  a query timing out every cycle on a healthy database. Tested against real PostgreSQL.
- **A permanent fault still ends Main by name,** as in FU-275.

## Security

- **C1, forged identity (gate §3a).** The support reference is generated by Main. A request carrying `X-Request-Id`
  and `X-Elitea-Support-Ref` gets a fresh reference, and the supplied values reach neither the answer nor the log
  (`TestTheSupportReferenceIsAlwaysGeneratedByTheServer`). The reference is random, not derived from anything.
- **C2, nothing internal reaches a client.**
  - Error bodies hold only the sentence, the code, the reference and an existing `ok:false`.
  - gRPC recovery statuses hold only the sentence and the code.
  - Panic values never reach a log or a client: the recover middleware used to log the panic value, and now logs
    only its type.
  - Each worked case is asserted by test.
- **Redaction by type.**
  - Go keeps today's `err` text, so nothing new is exposed. Known database types are described without connection
    details.
  - Rust renders an unregistered error type as a placeholder, never as text.
  - The runtime log-safety test still asserts that no secret reaches stdout, and stdout now carries both the operator
    stream and the span lines.
- **Not applicable:** injection, URL building, egress, authorization. No query, URL, outbound call or route
  semantics are added.
- `/security-review` and `govulncheck` / `cargo deny`: see Reviews.

## Recovery guarantee

Not applicable: no component × phase outcome changes. The FU-275 rows stand.

## Browser

The stack was disposable (`fu299a`, `fu299a.localhost:48600`). It ran the production profile through
`production-profile.pre-defaults-20261019/stack.sh`, restored the real-model dump
`product-real-models-main-ce2756fad.dump`, and passed `verify-profile` with 50 passed and 0 failed. The browser was
Chrome, signed in as `admin@centry.user`. All fixtures were created in the UI.

**Images (rules §3b)**

| Service | Image | Id | Binary check |
|---|---|---|---|
| Main | `elitea-main:fu299-1f799d714` | `sha256:574b9265…` | markers `toolkit.settings_unresolved` and `X-Elitea-Support-Ref` present |
| Worker | `elitea-worker-rust:fu299-c281c0178` | `sha256:2db8b475…` | marker `payload redacted; source=` present |
| Web | `main-ce2756fad-verify` | | |
| Supervisor and runners | `pm-778e5beb8`, `pm-2d7f7d3e8-{deno,rust}` | | |

**1. FU-300 worked case**
- Toolkit 104 is a new Custom toolkit with no tools, created in project 2 through the UI.
- Its page shows "The tool list did not load. Try again.", and still does after a reload.
- `GET …/toolkit_available_tools/prompt_lib/2/104` answers 503 with
  `{"ok":false,"error":"The toolkit's settings could not be resolved, so the request could not run.","code":"toolkit.settings_unresolved","support_ref":"sr-adfbc77e7794152b"}`.
  The response header `X-Elitea-Support-Ref` carries the same value.
- Main's log has exactly one line with `sr-adfbc77e7794152b` (N2). It is a WARN fault line: the sentence, the code,
  the action, and `cause: ["toolkit.settings_unresolved: …", "toolkit settings resolution is unavailable"]`. There is
  no SQL and no connection detail.
- Before this change the 503 left nothing in Main's log.

**2. Database outage**
- PostgreSQL was stopped for 45 s, then started.
- Main logged one WARN `dependency.database_unavailable`, with a safe cause (`SQLSTATE 08P01`, the server message, no
  host or user), and one INFO "Main reached its database again after 1m2s".
- The 12 runtime-composition runners routed through `CycleReporter` logged **no** `runner.cycle_failed` ERROR.
- **Still noisy:** Main's other background loops, which are not routed yet, logged 62 ERROR and 119 WARN lines over
  that window. This is registered as **FU-305** for the polish sweep, with the loop list.

**3. Worker operator stream**
- A new chat with `gpt-5.4-mini` and a neutral prompt ("What is the capital of Italy?") was answered "The capital of
  Italy is Rome.". Chat 888, execution `5d86336fc6899f2d89ff93be8ab8d2f9`. The answer is still there after a reload.
- The Worker wrote 81 stdout lines over the run:
  - all JSON, all INFO;
  - **0** span open or close lines, and an empty stderr;
  - all 77 lines inside the execution carry `support_ref` equal to `execution_id`, with the span fields flattened.
- Since the restart, stdout held 38 JSON lines and stderr none.
- The chat's preselected model was a leftover fixture (`vllm/CONTINUATION-REPAIR-FIXTURE`), which never starts a turn.
  That chat (887) was left waiting, and the proof used a real model. This is unrelated to this change.

**Teardown.** `stack.sh down --name fu299a --volumes`: no containers remain.

## Reviews

- **`code-review`** of the full diff found 10 Go findings and 4 Rust findings. All are fixed:
  - `127856578` (Go): per-class reporting windows, a single-flight confirm, `Dependencies` candidates, the honest
    settings sentence with 503 logged at WARN, the guide's claim, and the Go bans aligned with Rust.
  - `c281c0178` (Rust): a panic is never silent, the operator stream cannot be re-entered, the registry rules match
    Go, and a wrapper's repeated cause text is trimmed. This commit also fixes a backtrace-allowance flake found under
    load.
- **`security-review`** of the branch found no findings at confidence 8 or above. It examined 14 candidates:
  - client-supplied support references;
  - CORS exposure of the reference header;
  - HTTP and gRPC bodies;
  - panic values;
  - interceptor and middleware order against authorization;
  - Worker stdout fields compared with the previous compact format;
  - logx walking `Unwrap()` causes;
  - the PostgreSQL describer;
  - code generation from the registry.

  One class was noted without a finding: logx now walks `Unwrap()` chains that a wrapper's `Error()` used to hide. No
  secret-bearing cause was found on any path that logs. The polish sweep keeps it in view.
- **Dependency scanners.** This branch adds no third-party dependency: only the internal `elitea-faults` crate and the
  `libs/go/faults` module. egress-proxy's `genproto` stays at the oldest version already in use. Everything the
  scanners report is pre-existing on `main`.
  - `govulncheck`: `libs/go/faults` and egress-proxy are clean. Main, scheduler and subapp-host report `GO-2026-6809`
    in a required module, which their code does not call.
  - `cargo deny --all-features check advisories`: the Worker and `libs/rust` report the same advisories as `main`.

## Follow-ups

- FU-303: the LLM gateway onto the convention.
- FU-305: route Main's other background loops through `CycleReporter`. In the browser pass they logged 62 ERROR and
  119 WARN lines during a 45 s database cut.
- FU-306: Go's cause chain repeats the fault's own sentence as `cause[0]`; Worker INFO events have an empty `msg`.
- The polish sweep:
  - `legacy-codes.txt` and the other HTTP writers;
  - `RuntimeFailureKind` sourced from the registry;
  - rewriting the 53 runtime messages to one sentence each, which shrinks `multi-sentence-exemptions.txt`;
  - the per-site `recover()` calls (`scheduling/runner.go:469` drops its cause).
