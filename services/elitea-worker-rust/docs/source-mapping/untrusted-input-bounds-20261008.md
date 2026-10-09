# Bounded YAML expansion, OpenAPI reference budget and path segments

Scope: Rust Worker and `elitea-agent-runtime`. Tracks SEC-02, SEC-03 and SEC-09 from the 2026-10-08 replatform
review. The details are kept privately.

## Business behaviour

- The current platform loads saved pipeline YAML and OpenAPI specifications (inline, or fetched over HTTPS) and
  turns OpenAPI operations into tools. Path parameters are filled from tool arguments.
- Kept:
  - YAML anchors and aliases still work within the budget.
  - A path value that contains `/` is still sent as one encoded segment (`Ada/Lovelace` → `Ada%2FLovelace`). This
    keeps GitLab-style `group/project` ids working (user decision 2026-10-08).
  - Response collection hints still come from response schemas.
- Not ported:
  - YAML expansion without a bound after parsing.
  - `$ref` resolution without a work budget.
  - Path values that change the selected path.

## Changed paths

`R` is `libs/rust/agent-runtime/src`. `W` is `services/elitea-worker-rust/src`.

| Path | Change |
|---|---|
| `R/bounded_yaml.rs:65` | `from_str`: a counting pre-pass (nodes, scalar bytes, depth) runs over the same deserializer before the target type is built. `:94` `from_str_as_yaml_error` serves callers whose errors carry `serde_yaml_ng::Error`. |
| `R/graph/mod.rs:20` | `PIPELINE_YAML_BUDGET`: 131,072 nodes, 1 MiB scalar bytes, depth 64. One constant shared by the compiler and every node parser in both crates. |
| `libs/rust/clippy.toml`, `services/elitea-worker-rust/clippy.toml` | Ban `serde_yaml_ng::{from_str, from_slice, from_reader}` and `serde_yaml_ng::Deserializer::{from_str, from_slice, from_reader}`. The only allowed caller is `bounded_yaml::from_str`. Test files with inline fixtures opt out with a file-level `allow` and a reason. |
| `W/agents/graph/compiler.rs:82,85,799` | The document parse uses the budget. A compile-time assertion ties the budget to `MAX_PIPELINE_YAML_BYTES` (512 KiB). A refusal is the named limit `PipelineLimit::YamlExpansion` (`:2865`, code `graph.pipeline.yaml_expansion_exceeded`, detail `yaml_expansion`). It logs `pipeline_yaml_budget_exceeded` with only the bound's name. |
| `R/graph/{hitl,printer,router,state_modifier,yaml}.rs`, `W/agents/graph/{application,code,decision,direct_tool,llm,map_yaml}.rs` | Node-fragment parsers use the same budget. |
| `W/agents/graph/data_shaping.rs:51` | SplitOut and Aggregate node YAML uses the budget; a refusal is `ShapingConfigurationError::ResourceExhausted`. |
| `R/toolkits/families/openapi/spec.rs:32,324` | YAML specification text is bounded by `SPEC_YAML_BUDGET` before the tree is built. |
| `R/toolkits/families/openapi/spec.rs:27,29,1035,1069` | `ExpansionBudget`: a `$ref` resolution may produce at most 65,536 values, and one specification at most 1,048,576 in total. |
| `R/toolkits/families/openapi/spec.rs:905,958,23` | Response schemas are walked in place, at most 4 levels deep, following `$ref` without expanding them. Each visit is charged to the budget, and at most 64 hint paths are kept. |
| `R/toolkits/families/openapi/spec.rs:574` | Template paths refuse `.` and `..` segments, also when percent-encoded. |
| `R/toolkits/families/openapi/client.rs:818,833,469,504,51` | Path values are refused when empty or when they contain dot segments (also after one percent-decode), backslashes, control characters, Unicode line separators, or characters that normalize to `/`, `\` or `.`. The assembled path is checked again before `set_path`. |
| `R/toolkits/families/openapi/source.rs:45` | A fetched specification over budget fails with `SourceError::TooLarge` (`resource_exhausted`). |

Contract changes:

- An empty path value is refused with `InvalidInput`.
- A recursive response schema is now accepted. The old path-based cycle check refused it during full expansion.
- A template with a `.` segment is refused when the specification is parsed.

## Tests

Rebased on `origin/main` at `0def77b2`, after #1157–#1165 and ADR-0029, which moved toolkits and part of the graph
nodes into `elitea-agent-runtime`.

| Command | Result |
|---|---|
| `libs/rust`: `cargo test --offline --locked -p elitea-agent-runtime --all-targets --features toolkit-sql` | 502 passed, 0 failed, 0 ignored |
| `libs/rust`: same, with `--features test-preserve-order,toolkit-sql` | 502 passed, 0 failed, 0 ignored |
| `services/elitea-worker-rust`: `cargo test --offline --locked` | 1631 passed, 0 failed, 10 ignored |

The 10 ignored Worker tests are pre-existing:

- 9 need an isolated or disposable PostgreSQL: `postgres_checkpointer_tests::direct_tool_journal::*`,
  `run_root_fencing::*`, and `sandbox_dispatch_journal_preserves_exact_pending_identity`;
- 1 is a manual timing probe: `diagnostics::failure::tests::measure_failure_capture_cost`.

28 new tests, all run in the commands above:

- `R/bounded_yaml_tests.rs` (8):
  - node, scalar-byte and depth budgets at the limit and at limit+1;
  - every reference counts its expansion;
  - keys and tags are counted;
  - parity with the unbounded parser (anchors, tags, numeric keys, JSON values);
  - malformed and multi-document input stays `Malformed`.
- `W/agents/graph/pipeline_yaml_budget_tests.rs` (5):
  - node, depth and scalar-byte refusals, each reported as `graph.pipeline.yaml_expansion_exceeded`;
  - the refusal is identical on replay and fast;
  - anchors within the budget still compile.
- `R/toolkits/openapi_bounds_tests.rs` (14):
  - YAML specification refusal, with the replay variant;
  - layered response and parameter references;
  - a single walk and a whole specification at the limit and at limit+1;
  - shared and recursive references;
  - template dot segments;
  - path values that stay in one segment;
  - path values that would change the path;
  - adjacent placeholders;
  - two placeholders;
  - query encoding, and header line-break refusal.
- `R/toolkits/families/openapi/source.rs` (1): a fetched YAML specification over budget is `TooLarge`.

TDD: the pipeline, schema-reference and path tests were written first and failed against `origin/main` (`85cabcc8`)
for the expected reason:

- the pipeline tests got `malformed_yaml` instead of a budget refusal;
- the over-budget schema was accepted;
- a path value changed the request path.

The full-size replay tests were not run against the old code, because that would exhaust the host's memory.

Checks:

- `cargo fmt --check`: clean in both workspaces.
- `cargo clippy -D warnings`, CI's variants (`agent-runtime` with and without `toolkit-sql`, and the Worker with
  `--all-features`): clean. The ban also proves that no direct call is left in production code.

## Performance

Budget: refuse an over-budget document in under 100 ms and under 50 MB RSS. Measured before the rebase, on the same
parser and resolver code, which the ADR-0029 move did not change. Release profile, Apple Silicon, three runs each; RSS
is for the whole test process. Harness baseline: 6.3–7.4 MB.

| Case | Wall time | Peak RSS |
|---|---|---|
| Pipeline YAML refusal, two refusals of a ~300 KiB document | 40–50 ms (≈20 ms each) | 31.9–32.0 MB |
| OpenAPI YAML specification refusal, two refusals | 60 ms (≈30 ms each) | 32.5–33.1 MB |
| Layered response references | 10–20 ms | 7.4–7.6 MB |
| Layered parameter references (stops at 65,536 values) | 20–30 ms | 45.3–46.7 MB |
| Specification budget at the limit and at limit+1 | 10–20 ms | 17.2–21.1 MB |

- Memory is bounded by the budget, not by the input's expansion factor. The YAML cases are dominated by the
  parser's event buffer for the input text.
- Documents within the budget are parsed twice: one counting pass, then the typed pass. The counting pass allocates
  nothing.
- Response walks visit at most 4 levels. They replace a full expansion, so the common case gets cheaper.

## Durability

- No durable write path changed.
- A pipeline compile or toolkit materialization that is over budget fails the same way on every replay: typed and
  deterministic, with no allocation past the budget.
- A crash loop on replay is replaced by a typed admission failure.

## Resilience

- Every bound is a named constant checked during parsing, before allocation:
  - `PIPELINE_YAML_BUDGET` (shared across both crates, and asserted against `MAX_PIPELINE_YAML_BYTES`);
  - `SPEC_YAML_BUDGET`;
  - `MAX_SCHEMA_EXPANSION_NODES`, `MAX_SPEC_EXPANSION_NODES`, `MAX_RESPONSE_COLLECTION_PATHS` and `MAX_REF_CHAIN`.
- Failures are typed and readable:
  - pipeline: `LimitExceeded(YamlExpansion)` → `AgentSettingsLimit` with cause `graph.pipeline.yaml_expansion_exceeded`
    and detail `yaml_expansion` (`W/agents/runtime.rs:61`), the same path as the existing readable pipeline limits;
  - SplitOut and Aggregate: `ShapingConfigurationError::ResourceExhausted`;
  - OpenAPI: `OpenApiSpecErrorCode::ResourceExhausted`, or `SourceError::TooLarge` for a fetched specification;
  - path values: `OpenApiClientErrorCode::InvalidInput`, and no request is sent.
- The release profile uses `panic = "abort"`. No new `unwrap`, `expect` or `panic` was added on request paths.

## Security

| `rules/security.md` category | Applies | How it was checked |
|---|---|---|
| Trust boundaries and identity | No | No identity or header handling changed. |
| Authorization | No | No route or RPC changed. |
| Input, parsing and amplification | Yes | YAML expansion and `$ref` work are bounded during parsing. Proven by the limit and limit+1 tests above. The `clippy.toml` bans in both workspaces keep new YAML entry points on the bounded parser. |
| Injection and construction (URL and path) | Yes | Path values are encoded per segment, and every escape form is refused. Query values stay encoded, and header line breaks are refused. Proven by `openapi_bounds_tests`. |
| Egress and SSRF | No | Host, scheme and the fetch client are unchanged. Worker egress is tracked separately. |
| Secrets | Yes | No secrets in code or tests. The new log field is a fixed bound name, and error messages are static strings. |
| Supply chain | Yes | No dependency or lockfile change. |

Reviews:

- `code-review` (high): 9 findings. 8 fixed. 1 deferred: Main does not mirror the YAML budget at save time.
- `security-review`: no findings at confidence 8 or higher. Checked: path encoding and `allowReserved`, divergence
  between the counting pass and the typed pass, merge keys, and logging.

`cargo deny --all-features check advisories` (Worker): run before the rebase; there is no dependency or lockfile
change. Pre-existing on `origin/main`: RUSTSEC-2026-0258 (`h2`), RUSTSEC-2023-0071 (`rsa`), yanked `chacha20`. They
are tracked by the dependency-advisory work. New: none.

## Recovery guarantee

| Component × phase | Before | After | Enforced by | Proven by |
|---|---|---|---|---|
| Worker × admission (pipeline compile) | L (untyped process failure on replay) | F (typed readable limit, no retry) | `W/agents/graph/compiler.rs:799`, `R/bounded_yaml.rs:65` | `pipeline_reference_expansion_refusal_is_fast_and_identical_on_replay` |
| Worker × tool call (OpenAPI materialization) | L | F | `R/toolkits/families/openapi/spec.rs:324,1035`, `source.rs:45` | `yaml_specification_expansion_refusal_is_fast_and_identical_on_replay`, `specification_budget_holds_at_limit_and_refuses_limit_plus_one`, `fetched_yaml_beyond_the_expansion_budget_is_too_large` |
| Worker × tool call (OpenAPI request build) | R | R (unchanged; a refused value sends no request) | `R/toolkits/families/openapi/client.rs:469,504` | `path_values_that_would_change_the_selected_path_are_refused` |

## Browser evidence

Pending. The stack is being rebuilt from merged `main`. The planned regression:

- a normal pipeline run, including one that uses YAML anchors, with a reload;
- an OpenAPI toolkit call with a path parameter, with a reload;
- an image identity check that confirms the deployed binary contains a string unique to this branch.

## Fixtures

All fixtures are generated in the tests. No UI or database fixtures were used.

## Open limits and follow-ups

- Main does not mirror `PIPELINE_YAML_BUDGET` at save time. An over-budget pipeline is saved, then refused at Worker
  admission with the readable `yaml_expansion` limit.
- The template dot-segment refusal and the empty path-value refusal are contract tightenings. Tell toolkit authors in
  the release notes.
