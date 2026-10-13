# Indexing source mapping

Indexing follows the agent runtime and configuration/toolset foundations. The
authoritative capability inventory remains the workspace
`ELITEA_INDEXING_CAPABILITY_PARITY_MATRIX.md`; this tracked ledger binds that
behavior to eventual Rust ownership without claiming implementation.

## Worker and protocol mapping

| Python source path and symbol | Observable behavior | Rust target | Proving tests | Status / deviation |
| --- | --- | --- | --- | --- |
| `libs/proto/elitea/runtime/v1/indexing.proto` | Language-neutral index command/input/result contract | `src/protocol/{command,wire,control,output}.rs` (`ToolkitCommandKind::IndexIngest`) | `shared_toolkit_tests.rs`: command, claim entries, result bindings and bounds, restore | Done (ADR-0030 I1): `index_id` (19) and the typed per-document record (`documents`, `removed_document_keys`, `documents_truncated`, the optional `total_documents`/`total_chunks` pair describing the store after the run, present whenever the store was touched, a failure included, and `whole_index_record`, the run's own statement that the record is the whole index) added |
| Platform Python worker `handlers/indexing.py::IndexIngestDeliveryProcessor` and progress callback | Claim-bound execution and bounded `NodeEventV1` progress | `src/execution/toolkit_delivery_processor.rs::execute_index_ingest`, `src/indexing/pipeline.rs` | `src/indexing/tests.rs` (fake vector store, scripted gateway, fixture source), Stop and deadline between documents | Delivery done (ADR-0030 I1): authorized invocation, redeemed PAT, the lease's state probe checked between documents. Progress `NodeEventV1` frames are a follow-up: the toolkit path has no progress publisher yet |
| Platform Python worker `protocol/indexing.py` | Strict typed request/result validation | `src/indexing/inputs.rs`, `src/protocol/output.rs::validate_index_ingest_result` | `inputs.rs` tests; `shared_toolkit_tests.rs` result-bound mutations | Done (ADR-0030 I1) |
| Centry `indexer_worker/methods/indexer_index.py` and related registrations | Manual/scheduled index dispatch and SDK invocation | `src/indexing/ingest.rs` | Manual/scheduled differential and real-service tests | Planned |
| Centry indexing callbacks and event helpers | Thinking/progress/error/terminal payloads | `src/indexing/events.rs`, shared `src/protocol/node_event.rs` | Ordered current UI/event corpus | Planned |
| SDK index configuration/type registry | Source types, schemas and required settings | `src/indexing/sources/registry.rs` | Generated source manifest and schema differentials | Planned |
| SDK loader/parser/splitter implementations | Source-specific discovery, fetch, normalize, parse and chunk | `libs/rust/connectors` (`ContentSource` per family), `libs/rust/doc-extract`, `libs/rust/doc-chunk`; `src/indexing/source.rs` | The crates' own suites; `source.rs` filter and refusal tests; `src/indexing/atlassian_tests.rs` (Verification (2) for `confluence` and `jira` against the vector fake); `src/indexing/microsoft_family_tests.rs` (the Microsoft four); `src/indexing/figma_tests.rs`; `src/indexing/testmgmt_tests.rs` | I1 families done: `github`, `gitlab`, `bitbucket`, `ado_repos`, `artifact`. I2 Atlassian done: `confluence`, `jira` (pages/issues with attachments and comments as dependent documents under `parent_key`; deliberate differences in `SOURCE_PARITY.md`, "ADR-0030 I2 Atlassian families"). I2 Microsoft done: `sharepoint`, `ado_wiki`, `ado_boards`, `ado_plans` (SOURCE_PARITY.md "ADR-0030 I2: Microsoft families"). I3 test management done: `testrail`, `qtest`, `xray_cloud`, `zephyr_scale`, `zephyr_essential`, `zephyr_enterprise` (`src/indexing/testmgmt_tests.rs`; SOURCE_PARITY.md "ADR-0030 I3: test-management families"). I3 Figma done: `figma` (frames as text documents, comments on request; "ADR-0030 I3: Figma" in `SOURCE_PARITY.md`). Every SDK indexing family is now served. PDF/Office extraction runs in the `elitea-doc-extractor` sidecar (#1260) |
| SDK embedding/vectorstore path | Embedding batches, vector write/search and index identity | `src/transport/embeddings_facade.rs` (the `/llm` gateway), `libs/rust/vector-index` over `elitea-vector` (ADR-0031), `src/indexing/slug.rs` | `src/indexing/tests.rs`; `services/elitea-vector/tests/isolation.rs` (`ListDocuments` on a real Qdrant) | Done (ADR-0030 I1): no PGVector, no LangChain layout; the index is an `index_registry` namespace and the registry is Main's alone. Gateway refusals are classified: a non-retryable one (credential, project, budget, unknown model, any 4xx but 429) fails the run at once; a retryable one (429, 5xx, timeouts) skips the document, and five in a row fail the run. The store listing names the index's stamped space and is folded page by page |
| Centry schedule, stop, cleanup and notification behavior | Schedule/manual distinctions, cancellation, cleanup and capability-owned notification effects | `src/indexing/lifecycle.rs` plus Main contracts | Restart, stop, ACK-loss, cleanup and notification E2E tests | Planned; notification recipient/metadata remain Main-owned |

## Shared toolkit indexing overlay

The SDK exposes the same six index tools through 17 toolkit families:

`index_data`, `list_indexes`, `search_index`, `stepback_search_index`,
`stepback_summary_index`, and `remove_index`.

| Python source | Families | Rust target | Status / deviation |
| --- | --- | --- | --- |
| SDK `tools/base_indexer_toolkit.py::BaseIndexerToolkit` | Common six-tool schema and dispatch | `src/indexing/toolset_overlay.rs` | Planned after core indexing contract |
| SDK `tools/code_indexer_toolkit.py::CodeIndexerToolkit` | GitHub, GitLab, Bitbucket, ADO Repos | `src/indexing/sources/code_host/` plus per-family adapters | Planned |
| SDK `tools/non_code_indexer_toolkit.py::NonCodeIndexerToolkit` | ADO plans/boards/wiki, Jira, Confluence, Figma, qTest, SharePoint, TestRail, Xray, Zephyr Enterprise/Essential/Scale | `src/indexing/sources/<family>/` | Planned |
| SDK `tools/elitea_base.py::BaseVectorStoreToolApiWrapper` and `tools/vector_adapters/VectorStoreAdapter.py` | Vectorstore/project/collection identity and operations | `src/indexing/vectorstore.rs` | Planned; use explicit typed ownership rather than mutable wrapper settings |
| SDK `tools/index_params.py` | Input schema and search/index parameters | `src/indexing/types.rs` | Planned; property/differential validation |

Indexing functions are not counted as completed when their surrounding toolkit
family is ported. They become available only when the shared indexing overlay,
source adapter, durable lifecycle, vectorstore ownership, and failure/recovery
tests all pass.

## Required gates

- Generate the complete current source-family manifest before implementation
  completeness claims.
- Keep source retrieval/parsing behavior compatible, while externalizing a
  family when a dedicated provider service is a safer ownership boundary.
- Bound source concurrency, document counts, bytes, parser recursion,
  embedding batches, provider calls and database connections.
- Treat at-least-once delivery, cancellation, stale claims, restart, lost ACK,
  partial progress and cleanup as first-class system tests.
- Test mixed predict/index concurrency at representative limits while observing
  worker queues, PgBouncer and PostgreSQL recovery; an engine cache is not a
  concurrency limit.
- Keep ordinary search as toolkit functionality. Do not invent a separate
  `index.search.v1` subsystem.
- Keep notification creation capability-owned; the worker cannot select
  arbitrary recipients or notification metadata.
