# Document and memory library assessment

Date: 2026-09-23.
Status: evaluated candidates. No dependency or runtime integration is approved by this record.

## Decision and scope

Native Rust toolkits can call the document engines directly.
MCP transport is not required for this design.
A hidden MCP subprocess does not satisfy customers who reject MCP.
Prefer engine crates or extracted engine modules over server handlers.
Keep MCP adapters optional for customers who explicitly enable them.

Retain all eight projects as evaluated candidates.
Prioritize Xberg for extraction evaluation and the Zavora engines for document authoring evaluation.
Evaluate YantrikDB separately against existing long-term memory behavior.
Treat Forms as a small prototype reference, not a workflow trigger or data-table backend.
Do not implement a new document engine when an existing engine meets the required contract.

This review reads pinned source snapshots. It does not execute third-party code or install dependencies.
Build compatibility, output fidelity, benchmarks, and deployed acceptance remain unverified.
Repository versions and test files indicate implementation scope. They do not establish production maturity.

## Pinned upstream sources

Each source path below is relative to its repository at the listed commit.
The links identify immutable source trees.

| Project | Version in manifest | Reviewed source |
| --- | --- | --- |
| DOCX | 2.2.1 | [docx-mcp](https://github.com/zavora-ai/docx-mcp/tree/632efdaeda0d021c28128556bce5fa119eeabe0b) |
| PDF | 3.1.1 | [mcp-pdf](https://github.com/zavora-ai/mcp-pdf/tree/08501ea9776a2d85650ef6c1c18a868997c4d8e9) |
| Slides | 0.1.1 | [mcp_slides](https://github.com/zavora-ai/mcp_slides/tree/9c922c583ae17fe6ebaeaa7902c4795400dd3aef) |
| Excel | 0.2.2 | [excel-mcp-server](https://github.com/zavora-ai/excel-mcp-server/tree/867a03b239cfc99485182833e01cd8dea99670bc) |
| Flowcharts | 0.1.0 | [flowchart-mcp](https://github.com/zavora-ai/flowchart-mcp/tree/3872e18cb20b9f0ea340f6ddde82e1fcb2dca736) |
| Forms | 1.1.0 | [mcp-forms](https://github.com/zavora-ai/mcp-forms/tree/22d672f85ff14199a912ebb3f3da971f21c37d87) |
| Xberg | 1.2.8 | [xberg](https://github.com/xberg-io/xberg/tree/7a6f084a0ec27e4dae65a8e3ee69cc1878aaf64f) |
| YantrikDB | 0.23.1 | [yantrikdb](https://github.com/yantrikos/yantrikdb/tree/9aa589921da244ba07358674986f0bf30f87a198) |

## Engine assessment

| Candidate | Verified source and direct integration boundary | Limits and recommendation |
| --- | --- | --- |
| DOCX | `src/lib.rs` exports `engine`. `src/engine.rs` exposes document stores and template builders. `src/server.rs` uses `zavora_docx::Document`. | The public engine module mainly supplies templates and an in-memory store. Prefer `zavora-docx` for document operations. Audit that dependency separately. |
| PDF | `src/lib.rs` exports `tools`. `src/tools/{extract,generate,forms,security,convert}.rs` contain directly callable functions. Dependencies include `lopdf`, `pdf-extract`, `pdf_oxide`, and `zavora-printpdf`. | Reuse selected engine functions after validation. Path-based functions and string errors need a typed adapter. Do not publish all advertised operations without individual proof. |
| Slides | `src/lib.rs` exports stores, types, templates, and server. `src/server.rs` invokes `zavora_slide::Presentation`; the manifest also selects `zavora-slide-oxml`. | Prefer the underlying presentation crates. The server is not a separate engine abstraction. Generated and opened presentations have different operation limits. |
| Excel | `src/lib.rs` exports engines, tools, and store. `src/engines/zavora.rs` and `src/tools/` use `zavora_xlsx::Workbook`. | Direct engine reuse is plausible. The manifest references missing sibling path `../../zavora-xlsx`. The isolated checkout cannot resolve that dependency as written. |
| Flowcharts | `src/lib.rs` exports `engine`, `sequence`, and templates. `src/engine/{mod,layout,import,pdf}.rs` implement model, layout, Mermaid import, and PDF output. | This is a useful direct module candidate. Export formats include draw.io, Mermaid, DOT, SVG, and JSON. Diagram authoring does not provide executable workflow semantics. |
| Forms | `src/main.rs` declares private `domain` and `server` modules. `src/server.rs` implements definitions, submissions, validation, and summaries. | There is no library target. Native reuse requires extraction or upstream restructuring. The implementation lacks the durable product boundary needed for triggers and tables. |

### Concrete fidelity and durability findings

PDF `src/tools/extract.rs::extract_page_text` extracts whole-document text, then divides characters by page count.
This does not preserve actual page boundaries. It cannot support reliable page citations.
`extract_tables` splits lines on double spaces and treats the first row as headers.
This is a text heuristic, not proof of table structure or scanned-document extraction.
These findings support Xberg evaluation for extraction, while PDF authoring remains a separate candidate.

PDF generation functions contain `File::create(...).unwrap()` calls in `src/tools/generate.rs`.
A failed output-file creation can panic before a typed failure reaches the caller.
Review each admitted operation before executing it inside a shared worker process.
Security operations need semantic proof, including removal of recoverable redacted content.
An advertised security tool is not evidence of safe output.

DOCX stores documents in a `HashMap` in `src/engine.rs`.
Slides and flowcharts also use process-local stores.
Slides evicts handles by capacity and inactivity in `src/store.rs`.
Excel stores ten workbooks by default and expires them after thirty minutes in `src/store.rs`.
These handles are not durable artifact identifiers or authorization boundaries.
A replacement worker cannot recover unsaved changes from these maps.

Slides `set_slide_layout` documents one structural layout for generated decks.
Many editing methods explicitly require an opened deck.
Test creation, reopening, editing, and export separately.
Do not infer PowerPoint fidelity from a successful ZIP package write.

Forms starts through `FormsServer::seeded()` with example forms and submissions.
Its state uses `Arc<RwLock<Vec<...>>>` without persistent storage.
`run_validation` checks required values, an email `@`, and integer ratings from one through five.
Stored field types and options do not establish comprehensive type validation.
`submit_form` does not check the published flag before accepting a submission.
The inspected implementation does not supply durable webhook delivery, tenant authorization, table upserts, or workflow occurrence identity.

## Xberg: extraction and indexing

`crates/xberg/src/core/extract/mod.rs` exposes `extract(ExtractInput, &ExtractionConfig)` and `extract_batch`.
The functions delegate to a process-global default `Engine`.
`crates/xberg/src/engine/mod.rs` also exposes an explicit `Engine` and builder.
Prefer explicit construction where Elitea needs controlled dependencies and lifecycle.
`ExtractInput` and `ExtractionResult` are defined in `core/config/extraction/types.rs`.
Bytes input permits artifact retrieval through Elitea before parsing.
Do not expose unrestricted URI loading as an artifact-access shortcut.

The manifest separates PDF, Excel, Office, OCR, chunking, embedding, and other optional capabilities.
Defaults enable Tokio and SIMD UTF-8 support; they do not enable every document format.
`pdf` selects the native PDF engine. `pdf-pdfium` selects a separate FFI dependency.
OCR backends and model artifacts need their own deployment inventory.
Choose the smallest feature set that satisfies the admitted formats.
Validate offline startup and model availability before enabling any download-dependent capability.

Xberg provides extraction and enrichment candidates, not Elitea indexing ownership.
Source discovery, grants, collection identity, embedding policy, vector writes, cancellation, and durable progress remain platform responsibilities.
Keep parser output provenance through chunking and structured extraction.
Schema-validated business records need a separate extraction contract and evaluation corpus.
Parsing text alone does not prove invoice, contract, or customer-record accuracy.

`SecurityLimits` and `cache_namespace` exist in the inspected source.
Their presence does not prove tenant isolation or bounded resource use under hostile inputs.
Verify cache keys, temporary files, model caches, archive expansion, and memory limits for the selected configuration.

## YantrikDB: temporal and cognitive memory

`crates/yantrikdb-core` is a directly usable Rust crate.
`src/lib.rs` exports `YantrikDB`, `TenantManager`, recall types, temporal operations, graph operations, and provenance types.
`engine/record.rs` provides record and idempotency operations.
`engine/recall.rs` provides recall, explanations, and sequence-aware recall.
`engine/temporal.rs` provides episode ordering and temporal relevance.
These are useful candidates for cross-conversation memory evaluation.
They do not establish better recall quality than Elitea's existing store.

The core uses bundled SQLite through `rusqlite`.
`engine/mod.rs` documents separate read connections and a serialized write path.
`engine/tenant.rs` opens a separate database file per tenant and validates tenant path components.
These mechanisms do not authorize Elitea users, projects, or individual memory operations.
Always derive the storage scope from admitted platform identity.
Never accept a model-selected namespace as authorization.

The README describes a separate `yantrikdb-server` using OpenRaft for cluster deployment.
That server is outside this assessment.
Core replication primitives do not establish a complete managed cluster in Elitea.
An embedded database inside a replaceable worker needs explicit storage ownership and recovery design.
Do not assume a shared filesystem provides safe replication or failover.
Evaluate a dedicated storage owner if the core requires stable local files.
Keep that service protocol independent from MCP.

The default feature includes a bundled embedder.
`embedder-download` adds an optional runtime download path; it is disabled by default.
A slim build can inject an embedder through the engine API.
Verify model licenses, embedding dimensions, replacement policy, and offline behavior.
Do not adopt the crate's credential vault as a replacement for Elitea credential ownership.

Gate 7b already requires an audit of Main memory CRUD and recall.
Compare YantrikDB with that implementation before adding a second authoritative memory store.
A derived retrieval index may be appropriate, but its repair and deletion contracts need explicit design.
Keep memory separate from execution compaction, trusted instructions, and permission grants.

## Licensing, build, and maturity boundaries

All six Zavora manifests declare Apache-2.0.
The inspected DOCX tree has no root `LICENSE` file, although its manifest declares that license.
Confirm release-package notices and engine dependency licenses before redistribution.
The server declarations do not establish the licenses of all transitive engines.

Five Zavora manifests declare Rust 1.94.1; Excel declares edition 2024 without a Rust-version field.
DOCX, PDF, Slides, Flowcharts, and Forms pin `adk-mcp-sdk` to a Git revision.
These dependencies remain in their manifests even when callers use public modules directly.
For an MCP-free dependency graph, select engine crates or separate the engine into a transport-free crate.
Disabling server startup alone does not remove MCP dependencies.
Resolve Excel's sibling dependency through a verified release or an explicit pinned source before build evaluation.

Xberg's root manifest and license declare MIT.
Its `ATTRIBUTIONS.md` records additional licenses for vendored libraries, models, and test fixtures.
That file also retains an Elastic License 2.0 compatibility statement.
Resolve this inconsistent statement against the selected release and distributed files before approval.
Do not classify every optional Xberg component as MIT from the root license alone.
The manifest declares Rust 1.92 and includes native-library options with separate build requirements.

YantrikDB declares Apache-2.0 and bundles SQLite.
Model artifacts and other dependencies still require a distribution inventory.
Its SQLite dependency differs from Xberg's inspected SQLite dependency.
Check native `links` compatibility if both optional stacks enter one binary.
No combined Cargo resolution or compilation occurs in this assessment.

The Zavora snapshots include varying amounts of inline and integration tests.
None of the six inspected snapshots contains `.github/workflows` files.
Xberg and YantrikDB contain CI workflows and larger implementation surfaces.
These observations guide validation effort; they do not prove maintenance guarantees or passing CI.
No independent defect rate, security audit, benchmark, or support commitment is verified here.

## Native toolkit contract

Register static tool schemas through the existing native toolkit catalogue.
Bind toolkit identity and selected tools before engine invocation.
Resolve input artifacts with the existing admission and object-grant contracts.
Pass bytes or controlled temporary paths to the engine.
Return typed results, safe errors, immutable artifact references, and source provenance.
Keep raw filesystem paths outside the model-visible contract.

Persist completed artifact revisions and effect receipts before acknowledging success.
Reconstruct engine state from persisted inputs after worker replacement.
Use bounded CPU execution and explicit byte, page, archive, time, and output limits.
Process isolation can contain parser crashes without introducing MCP.
Apply gate 6 effect handling to external writes and published outputs.
Do not reuse upstream process-local handles as durable workflow state.

## Current-to-new mapping and gates

The existing ledgers retain the detailed current-platform source map.
This document adds candidates; it does not replace existing behavior with upstream server semantics.

| Required behavior | Current source or existing ledger | Proposed owner and gate |
| --- | --- | --- |
| Document upload, transformation, and structured extraction | [Customer workflow assessment](customer-workflow-migration-20260923.md), `WF-DOC-01` | Native engine adapters with artifact authority; gates 7 and 7a. |
| Attachment and analysis module behavior | [Built-in module mapping](builtin-runtime-modules.md) | Shared artifact and processing implementation; gate 7a. No second artifact store. |
| Parser and indexing lifecycle | [Indexing mapping](indexing.md), SDK loader/parser/splitter and vector-store references | Xberg parser candidate within gate 8. Indexing remains last. |
| Memory CRUD and recall | [Long-term memory mapping](long-term-memory.md), SDK `tools/memory/__init__.py`, Main `internal/infra/db/repos/memories.go` | Existing Main storage and authorized recall; YantrikDB comparison within gate 7b. |
| Form-triggered execution | `WF-TRIGGER-01` in the customer workflow assessment | Main trigger admission, durable occurrence identity, delivery, and browser controls. Forms supplies reference concepts only. |
| Native data tables | `WF-TABLE-01` in the customer workflow assessment | Main-owned project tables, schemas, keys, filters, upserts, quotas, and authorization. Forms submissions do not satisfy this contract. |

## Required validation before selection

1. Resolve pinned dependencies and notices in a clean, offline-capable build environment.
2. Prove direct engine invocation without MCP transport or an MCP subprocess.
3. Verify the selected toolkit schemas and exact artifact authority through Main and Rust.
4. Compare real customer documents against explicit expected text, tables, citations, formulas, and formatting.
5. Render generated documents and inspect layout in independent applications.
6. Test malformed archives, oversized inputs, path traversal, external links, timeouts, cancellation, and parser crashes.
7. Replace the worker after artifact publication and before acknowledgement; verify one durable result.
8. Verify cross-user and cross-project isolation, including caches, temporary files, and stale handles.
9. Compare memory recall across conversations, corrections, conflicts, expiry, and deletion using a fixed corpus.
10. Verify memory backup, restoration, replay, migration, and replacement without the original worker's local state.
11. Verify form duplicate delivery and table concurrent upserts under their separate product contracts.
12. Report each operation independently. A successful sample does not close a capability family or migration gate.

## Implementation history

2026-09-23: inspect eight pinned source snapshots and record direct Rust engine candidates.
Record PDF page-provenance defects, Forms prototype limits, and Excel dependency resolution gaps.
Map extraction to gates 7, 7a, and 8; map persistent memory to gate 7b.
Keep `WF-DOC-01`, `WF-TRIGGER-01`, and `WF-TABLE-01` separate.
No runtime code, dependency manifest, database schema, deployment, or gate status changes occur in this slice.
