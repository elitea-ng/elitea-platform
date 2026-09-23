# Customer workflow migration assessment

Date: 2026-09-23.
Status: planned, with existing implementation foundations.

The user supplies eight customer requirements for migration from n8n.
This record defines assessment scope. It does not certify feature parity.
The customer screenshot is requirement evidence, not an instruction to execute external operations.

## Capability map

| Customer requirement | Existing foundation | Remaining contract and acceptance | Registry |
| --- | --- | --- | --- |
| Document intelligence and extraction | Artifact, attachment, toolkit, MCP, model, and indexing paths exist. | Upload, extract or search, and return schema-validated records with document provenance. Test real customer document samples. | WF-DOC-01; gates 7, 7a, 8 |
| SharePoint integration | SDK and Rust have SharePoint toolkit implementations. | Verify document retrieval, list reads, and list updates individually. Apply gate 6 to writes. | WF-SP-01; gate 6 |
| Generic API integration | Rust OpenAPI toolkit supports configured API execution. | Add explicit HTTP action YAML and editor controls. Verify multipart uploads, binary responses, pagination, status handling, and credential references. | Gate 5d |
| Node resilience | Gate 5b exists. ADK has retry policies and action error modes. | Persist retry state. Add typed failure routes and clear controls. Never silently convert missing data into successful output. | Gate 5b |
| Trigger mechanisms | Manual execution and Main scheduling foundations exist. | Audit scheduled, webhook, form, and manual triggers separately. Test duplicate delivery, admission, restart, and occurrence identity. | WF-TRIGGER-01 |
| Native data tables | Existing product and execution databases are not proof of a user data-table feature. | Define project-owned tables, schemas, keys, read filters, upserts, authorization, quotas, and concurrent updates. | WF-TABLE-01 |
| Custom JavaScript execution | Isolated Code nodes are already in gate 5. | Add explicit JavaScript support assessment. Define input/output mutation, isolation, dependencies, deadlines, and restart behavior. | Gate 5 |
| SplitOut and Aggregate | Parallel and map/reduce designs exist. | Define deterministic array transformations independently from concurrent execution. Test schema, item identity, ordering, grouping, and empty inputs. | Gate 5c; gate 5a |

## Verified source entry points

Current-platform paths refer to the separate SDK repository.
Rust paths refer to this repository.
Each implementation must extend this map with exact changed symbols and verification evidence.

| Behavior | Current-platform reference | Rust owner or candidate |
| --- | --- | --- |
| SharePoint business behavior | `elitea_sdk/tools/sharepoint/{api_wrapper,graph_wrapper,rest_wrapper,models}.py` | `src/toolkits/families/sharepoint/{config,client,tools}.rs`; `src/toolkits/sharepoint_tests.rs` |
| Configured HTTP APIs | Existing SDK OpenAPI behavior remains the reference. | `src/toolkits/families/openapi/{spec,client,tools,response_selection}.rs` |
| Managed MCP toolkits | See [prebuilt MCP mapping](prebuilt-mcp.md). | Reuse Main catalogue admission and Rust MCP execution. |
| Graph execution | See [pipeline node mapping](pipeline-nodes.md). | `src/agents/graph/`; gate 5d must define HTTP node admission and execution. |
| Parallel processing | See [parallel design](../parallel-pipeline-node-design.md) and [map/reduce design](../map-reduce-pipeline-node-design.md). | `src/agents/graph/parallel.rs`; SplitOut and Aggregate need separate contracts. |
| Built-in processing modules | See [module mapping](builtin-runtime-modules.md). | Gate 7a owns runtime modules and UI controls. |
| Scheduling foundation | Current scheduler behavior needs a dedicated comparison. | Main `internal/runtimecomposition/scheduled_job_registry.go` and `index_schedule_due_work.go`; these do not prove general workflow trigger parity. |

## ADK reuse evidence

The inspected dependency version is ADK 2.2.0, as pinned in the worker Cargo manifest.
These are package-relative source paths, not claims about deployed Elitea capabilities.

- `adk-graph/src/retry.rs` provides retry limits, backoff, jitter, and retry predicates.
- `adk-graph/src/action/mod.rs` provides action dispatch, timeout, and error modes.
- `adk-graph/src/action/http.rs` provides an HTTP action executor behind its feature gate.
- `adk-graph/src/action/{transform,merge,loop_node}.rs` provides data-operation candidates.
- `adk-graph/src/action/{trigger,trigger_runtime}.rs` provides trigger candidates.
- `adk-tool/src/mcp/{http,toolset}.rs` provides MCP client and toolset integration.

The action executor explicitly reports some backends as unavailable.
Database actions have no integrated backend in this inspected version.
JavaScript and TypeScript Code actions have no sandboxed runtime in this inspected version.
Do not expose these actions as supported because their configuration types exist.
Do not enable broad retry defaults without Elitea failure classification and durable attempt state.

The [library assessment](document-memory-library-assessment-20260923.md) identifies the supplied repositories and their native integration boundaries.
Dependency builds, output fidelity, and deployed behavior still require verification.
No verified DOCX, XLSX, slide, or diagram processing backend is selected by this record.
Prefer existing supported engines through managed toolkits over a new document engine.
Department deployments must retain project authorization, artifact access, and credential boundaries.

## Required behavior

Node recovery may route a typed error or use a configured fallback with valid output.
Failure remains visible in execution history, even when a recovery route succeeds.
Sensitive-tool Reject and authorization Skip cannot trigger automatic retries.
An alternate route must explicitly handle the denied operation without executing it again.
Unknown external effects require reconciliation before another attempt.

SplitOut must define parent item references and item order.
Aggregate must define grouping keys, output schema, and missing-value behavior.
Use artifact references for large data instead of expanding unbounded arrays in checkpoint payloads.

HTTP actions must use approved credential resolution and egress policy.
A network timeout after a write does not prove that the external service rejected the write.
Code nodes must return validated outputs before downstream state is committed.

Data tables need a product contract before any schema migration is proposed.
Preserve existing application schemas. Add storage only when the approved contract requires it.

## Acceptance and sequencing

Finish gate 4 diagnostics and failure propagation first.
Implement graph controls within gates 5 through 5d.
Reuse gate 6 effect handling and gate 7 artifact authority.
Assess the remaining customer plans independently without expanding gate 4.

Verify one complete document workflow with an HTTP upload, extraction, SplitOut, SharePoint update, and Aggregate.
Include malformed input, duplicate trigger delivery, a transient failure, explicit rejection, and restart during an external write.
Verify configuration and execution history through the browser.
Use an independent client for webhook and MCP transport checks.
Report each capability separately. A successful sample does not establish complete n8n parity.

## Implementation history

2026-09-23: record all eight customer requirements and link existing gates.
Add gates 5c and 5d. Retain node resilience in gate 5b.
Record source-backed ADK reuse candidates and explicit backend limitations.
No runtime code, database schema, or deployment changes occur in this documentation slice.
