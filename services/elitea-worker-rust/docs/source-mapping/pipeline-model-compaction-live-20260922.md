# Pipeline LLM-node compaction acceptance

## Functional boundary and source mapping

Compaction applies to accumulated model-local input before the next model call reaches the admitted threshold.
Short LLM calls below that threshold do not compact.
Deterministic nodes and exact graph state do not undergo summarization.
An Agent node delegates to the existing independent child-agent context mechanism.

The functional reference is `projects/elitea-sdk/elitea_sdk/runtime/tools/llm.py::LLMNode.invoke` at SDK revision `966526e8334354366dd161b606d73fe8e204b850`.
That node maps graph input into a model invocation and returns its output separately.
The legacy compaction algorithm is not the target implementation.

Rust `src/agents/pipeline.rs::NativePipelineLlmAgentFactory::build` binds the task model, dedicated summary model, and scoped context callbacks.
`src/agents/graph/llm.rs::PipelineModelScope::for_node` separates graph threads, node identities, steps, and parent scopes.
`src/agents/session.rs::assemble_pipeline_native` retains exact graph state outside model-local summaries.
`src/agents/pipeline_scope_tests.rs` covers mixed deterministic and model execution with isolated compaction.

Main `internal/infra/db/repos/agent_context.go` excludes pipeline-node usage from the ordinary root context meter.
Rust context events identify these measurements with `model_scope=pipeline_node` and the node name.
One graph-wide percentage would incorrectly combine independent model contexts.

## Deployed verification

Fresh headed Playwright submits chat 621 to pipeline 44, version 51.
Its LLM node reads fictional records twice, with indexes 1 through 12 in each pass.
A following state-modifier node copies the answer into the terminal output.
The graph also retains a fixed exact-data marker.

Execution `7160b89000837cf43360cd500736c6a5` settles as `SUCCEEDED`.
The durable ledger contains exactly 24 read calls and no tool errors.
The independent fixture log confirms exactly two ordered passes without extra physical calls.

The task model uses Full mode with a 128,000-token window and 4,000 reserved output tokens.
The safety margin is 1,280 tokens; usable input is 122,720 tokens.
The trigger is 110,448 tokens, or 90% of usable input.
The dedicated Luna summary model has an 8,192-token output limit.

| Model-local measurement | Estimated input tokens |
| --- | ---: |
| Initial call | 281 |
| First compaction input/output | 151348 / 1105 |
| Second compaction input/output | 152172 / 1057 |

Large completed tool batches cross the threshold between model calls.
Compaction handles those batches before the task model receives the next request.
These measurements are estimates, not provider usage counters.

Both saved graph checkpoints preserve `PIPELINE_STATE_MUST_REMAIN_EXACT`.
The second checkpoint contains identical `answer` and `final_text` values, each 850 characters long.
The browser answer retains the delivery code, latest color, archive status, next step, and 24-read count.
Reload returns exactly the same answer, with no browser page errors.
Visual inspection confirms a single terminal answer.

The worker image is `sha256:f216f359c9318c5dc93dc914f35189ef3c1b30fe29b9e52c675417b87eea8272`, matching source commit `6ae7d7cf`.
Local browser evidence uses the `elitea-pipeline-model-compaction` prefix.
Database checks use `pipeline-proof.sql` and `pipeline-graph-proof.sql`.

## Limits and test correction

Chat 620 fails admission because the fixture incorrectly uses `entity_type=pipeline` for its toolkit relation.
The supported relation type is `agent` for both agents and pipelines.
`apps/elitea-web/src/features/agents/lib/toolRelation.ts` documents and applies this shared relation type.
The corrected fixture produces chat 621; no product change is needed.

The original browser harness waits for a root meter that intentionally excludes pipeline nodes.
After authoritative execution success, a separate fresh browser verifies final output and reload.

This check proves repeated LLM-node compaction during one graph execution.
It does not prove whole-pipeline crash recovery or introduce graph-wide compaction.
Pipeline checkpoint inspection and recovery assembly remain a separate open boundary.
No schema, runtime source, or deployment configuration changes are part of this acceptance record.
