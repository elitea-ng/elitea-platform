# Provider conformance (ADR-0022 phase P0, ADR-0012 phase P1.0)

The published contract every provider service is admitted against, plus the
golden fixtures each provider is verified with.

**This directory moved in P1.0**, from `services/elitea-deepwiki/conformance/`.
It moved because a contract that lives inside one implementation's test
directory is indistinguishable from that implementation's notes — you cannot
generalise a runner against it, and the second provider has nowhere to put its
recordings. Fixtures are now grouped per provider under `fixtures/<provider>/`,
and the protocol-generic half is `spi/contract.json`, which belongs to none of
them.

The original subject is unchanged: porting the legacy `deepwiki_plugin` into
`services/elitea-deepwiki`, per
ADR-0022 (`elitea-docs`: `docs/internal/03-architecture/adrs/adr-0022-deepwiki-provider-service-port.mdx`)
("DeepWiki Ports as a Standalone Provider Service on PostgreSQL", Approved).

P0 ships **fixtures only**. No engine code moves here (that is P1), and nothing
in this directory touches elitea-main, `v2.yaml` or any migration.

## What is here

```
conformance/provider/
├── pyproject.toml            no runtime deps; [record] extras only to re-record
├── spi/contract.json         THE CONTRACT: the SPI path set, provider-agnostic
├── tools/                    the recorders (run against the legacy checkout)
├── tests/test_fixtures.py    invariants over the committed fixtures (stdlib + pytest)
└── fixtures/
    └── deepwiki/             one profile per provider
        ├── descriptor/legacy-v0/ the golden provider descriptor + inventory + digests
        ├── spi/              recorded request/response shapes for every SPI operation
        ├── generation/       the composed generate_wiki artifact set
        ├── wiki_query/       the four wiki_query tools' answers, and the
        │                     legacy delete_wiki defect, recorded as such
        └── retrieval/sample-repo/ dense / BM25 / FTS / fused rankings, with scores
```

Two artefacts sit outside this directory and belong to the same contract:

| Path | What it is |
| --- | --- |
| `libs/provider/legacy/v0/` | the vendored v0 schema documents, byte-pinned by `fixtures/deepwiki/descriptor/legacy-v0/bundle.manifest.json` |
| `scripts/provider/verify-legacy-bundle.sh` | the gate that keeps them byte-identical |

`spi/contract.json` is spelled in three places, and
`services/elitea-main/internal/api/v2/deepwiki/spi_contract_parity_test.go`
fails when any one of them drifts: this file, the path builders in `proxy.go`,
and the facade routes in `v2.yaml`.

Every fixture carries a `_source` block pinning the legacy repository revision
and the SHA-256 of each source file it was derived from.

### Recorded, not transcribed

The recorders import the legacy sources verbatim (with a stubbed
`pylon.core.tools` and, where needed, a stubbed `flask`) and **execute** them:

| Fixture | Executed legacy code |
| --- | --- |
| `descriptor/legacy-v0/provider_descriptor.json` | `methods/descriptor.py::provider_descriptor` |
| `spi/health.get.json`, `slots.get.json`, `invoke.post.json`, `invocations.*.json` | the matching `routes/*.py` handlers |
| `spi/errors.json` | `methods/invoke.py::_create_error_response`, per exception class |
| `spi/toolkit_aliases.json` | the alias lists read out of `perform_invoke_request`'s AST |
| `retrieval/sample-repo/queries/*.json` | `unified_db.search_fts5` / `search_vec` / `search_hybrid`, `bm25_disk.BM25SqliteIndex.search` |
| `generation/composed_result.json` | `methods/invoke.py::perform_invoke_request` with only `self.generate_wiki` stubbed |
| `generation/wiki_structure.json`, `page_names.json`, `registry_entry.json` | `ArtifactExporter._export_json_format`, `_create_safe_filename`, `rebase_artifact_name`, `WikiRegistryManager.register_wiki` |

Two things are restated rather than executed, and say so in the fixture:

* `generation/wiki_manifest.json` — the manifest is built inline in
  `wiki_subprocess_worker.py`, not by a callable. The identity fields
  (wiki id, page paths, analysis key) still come from the legacy helpers, and
  the worker file is pinned by digest so a restatement cannot drift silently.
* `spi/slots.get.json` jobs-mode body — recording it needs a Kubernetes API.

### Deliberate scope limits

* **No LLM anywhere.** Retrieval fixtures use `StubEmbedder`, a seeded
  hash-bucketed bag-of-tokens projection recorded in
  `retrieval/sample-repo/embedding_model.json`; the generation fixtures supply
  the page bodies directly.
* **Chunker parity is not covered.** `tools/record_retrieval.py::chunk_corpus`
  is a trivial `def`/`class` splitter, *not* the legacy tree-sitter pipeline.
  Its output is committed as `nodes.json`, so the retrieval fixtures stand on a
  frozen input either way. Parser/chunk parity is separate work.
* **The FAISS ensemble path is not recorded.** `UNIFIED_RETRIEVER_ENABLED` is
  `True` in the legacy source, so `unified_db` is the live retrieval path; the
  older `retrievers.WikiRetrieverStack` (FAISS + `EnsembleRetriever`) is not
  exercised.
* Behaviours listed in the spec's DeepWiki compatibility inventory that need a
  live repository, cluster or credential (multi-provider clone, K8s job mode,
  slot accounting under load, cancellation mid-generation) are **not** covered
  by P0 and remain open for the P1 container tests.

## Running

Replaying the committed fixtures needs only pytest:

```bash
cd conformance/provider && python -m pytest -q
```

Re-recording needs a read-only legacy checkout and the `record` extras:

```bash
cd conformance/provider && DEEPWIKI_LEGACY_ROOT=~/projects/eliteaai/legacy/plugins/deepwiki_plugin python tools/record_spi.py --check
```

`DEEPWIKI_LEGACY_ROOT` is optional when the checkout sits at the usual
workspace path. Every recorder takes `--check`, which fails on drift instead of
rewriting — that is how the legacy plugin is watched while it is still alive.

## How each fixture set is replayed in P1

**1. Descriptor → the legacy-v0 conversion pipeline.**
`provider_descriptor.json` is the verbatim legacy-v0 input for
spec-provider-service's (`elitea-docs`: `docs/internal/03-architecture/cloud-native-migration/spec-provider-service.mdx`)
`legacy descriptor -> v0 validator -> policy overlay ->
ProviderPublishedManifestV1 -> AdmittedProviderRevisionV1` chain. P1 asserts
that the ported service's `GET /descriptor` is byte-identical to this file
modulo `service_location_url`, and that conversion yields 3 toolkits, 9 tool
declarations and 7 unique tool names with the argument schemas in
`descriptor.inventory.json`. `bundle.manifest.json` supplies the digests for
`libs/provider/legacy/v0/bundle.manifest.json`.

**2. SPI → a black-box HTTP test against the ported container.**
*Landed:* `services/elitea-subapp-host/internal/spi/conformance_test.go` replays these fixtures against the Go host (the Python shell's `tests/conformance/test_spi.py` went with the shell, ADR-0023 H4b), so the descriptor, health, slots, invoke,
poll, cancel and error-contract cases are a live gate rather than a plan. What
is still ahead is running the same cases over mTLS through the elitea-main
facade (P2) and against the engine-carrying image.
`routes.json` drives the operation list; for each, the recorded case files are
the expected responses. The ported ASGI app is started with the same fake
worker seam, and each case is issued over mTLS through the elitea-main facade
(and directly, to prove the service refuses non-mTLS). The cases that must not
regress: async-only invoke returning `{invocation_id, status: "Started"}`; the
`pending→Started` / `running→InProgress` projection; drain-on-read
`custom_events`; HTTP 204 on cancel and 404 for unknown ids; and the *two*
error shapes (HTTP 200 + `status: "Error"` for tool failures, the
`errorCode/message/details` envelope for transport failures). The recorded
`error_category` table is what a P1 error classifier is diffed against.

**3. Retrieval → the gate on the PostgreSQL storage port.**
`nodes.json` is loaded into the new schema, `embedding_model.json` supplies the
vectors (no model call), and the same seven queries are run against
pgvector + `tsvector`. Each branch is compared as a *ranked list with scores*,
not top-1:

* **dense** — pgvector HNSW ordering must match the recorded `vec_distance`
  ordering; the L2 distances themselves should match to fixture precision.
* **fts** — `tsvector` + GIN must reproduce the recorded set. Note the recorded
  conjunctive semantics: FTS5 `MATCH` ANDs bare terms, so
  `plainto_tsquery` is the match, not `websearch_to_tsquery`.
* **bm25** — the standalone index is the one branch ADR-0022 expects to change
  (term-statistics tables or a `ts_rank` substitution). The fixture is the
  evidence any ranking drift must be argued against.
* **fused** — `test_fused_scores_are_reproducible_from_the_component_rankings`
  recomputes the weighted RRF from the component lists, so a P1 implementation
  that has correct component lists is held to exactly this fusion. The fixture
  also pins that BM25 is *not* an input to the legacy fusion, so "improving" it
  into a three-way RRF cannot be sold as parity.

**4. Generation → a `generate_wiki` replay with a stubbed engine.**
The P1 service runs `generate_wiki` with its page-generation step stubbed to
the fixture's page bodies; the composed result must equal
`composed_result.json` in object order, `object_type`, target, extension,
encoding and artifact names. `page_names.json` gates `normalize_wiki_id` /
`_create_safe_filename` / `rebase_artifact_name` — the naming rules that decide
where every artifact lands in object storage — and `registry_entry.json` is the
row shape the new `wikis` table must be able to produce, which is also how the
ADR's "two concurrent generations corrupt neither registry row" check is
expressed.

## Findings worth carrying into P1

* **The invocation path is `/tools/{toolkit}/{tool}/invocations/{id}`.**
  ADR-0022 decision 2 abbreviates it as `/invocations/{id}`; the wire path and
  the legacy SPI OpenAPI both carry the toolkit and tool segments. The port
  must serve the long form.
* **Every tool advertises `sync_invocation_supported: true`, and the service
  never honours it** — `routes/invoke.py` returns immediately, unconditionally.
* **The descriptor advertises `result_bucket: "wiki"`; invoke emits
  `"wiki-artifacts"`.** They disagree today and the invoke-time value wins.
* **`custom_events` are process-local and read-once.** A missed poll or a
  restart loses them, which is exactly what the spec's durable-operation-state
  requirement forbids.
* **A jobs-mode failure in `/slots` returns HTTP 200 subprocess numbers**, so a
  Kubernetes outage reads as healthy capacity.
* **`include_traceback=True` ships a full Python stack trace** to the caller for
  unknown toolkits/tools and unhandled exceptions.
* **The registry is a read-modify-write on one JSON blob with no locking** —
  the race ADR-0022 decision 3 replaces with a table.
