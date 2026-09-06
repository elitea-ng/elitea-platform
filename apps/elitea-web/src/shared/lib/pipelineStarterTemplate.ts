/**
 * The document a NEW pipeline is created with.
 *
 * Before this constant, `usePipelineEditorCreate`'s `EMPTY_CREATE_VALUES` sent
 * `instructions: ''`. A pipeline created that way holds no graph at all, so its
 * first chat turn cannot run: the compiler refuses an empty document
 * (`compiler.rs:459`, mirrored by the `document.node-count` admission rule),
 * and the user's only signal was a failed turn in another process.
 *
 * ## Why it lives in `shared/lib`
 *
 * A pipeline is created from TWO places, and they sit in different layers: the
 * chat surface's `features/pipelines/lib/usePipelineEditorCreate.ts`, and
 * `pages/pipelines/CreatePipeline.tsx` — the `/pipelines/create` route, which
 * is the flow a person takes and the one that kept storing an empty document.
 * `no-deep-slice-import` forbids the page from reaching into the feature's
 * `lib/`, and the feature's curated barrel is at its §3.5 budget of 20 exports.
 * The constant depends on nothing in either slice, so it belongs below both.
 *
 * ## Why these exact keys
 *
 * The template is the smallest document BOTH runtimes accept and run.
 *
 * * The native runtime admits it whole.
 *   `features/pipelines/lib/pipelineStarterTemplate.test.ts` runs it through
 *   `collectGraphAdmissionIssues`, the mirror of
 *   `services/elitea-worker-rust/src/agents/graph/`, and requires zero issues.
 *   That test stays in the pipelines slice because the admission helper it
 *   checks against does.
 * * The Python worker's SDK needs `system` AND `task` in `input_mapping`.
 *   `LLMNode._invoke_llm_internal` selects its pipeline flow on
 *   `'system' in func_args` (`elitea_sdk/runtime/tools/llm.py:1138`) and
 *   refuses the node when either is absent. Its OTHER branch reads
 *   `state["messages"]`, and that channel is EMPTY on a first turn: the
 *   runnable moves the user's message out of `messages` into `input`
 *   (`langraph_agent.py:1878`), so a node mapped only to `messages` raises
 *   `LLMNode requires 'messages' in state for chat-based interaction`.
 *   Mapping `task` to the `input` state variable is what puts the user's
 *   message in front of the model.
 *
 * `system`/`task` are also exactly the two fields the node panel edits
 * (`getDefaultLLMInputMapping` in `lib/flow-editor/hooks/useLLMInputMapping.ts`
 * seeds `system`, `task` and `chat_history`), so the template opens in the
 * editor as an ordinary LLM node with its prompt fields filled in.
 */

/** Node id of the single LLM node the template ships. Referenced by `entry_point`. */
export const PIPELINE_STARTER_ENTRY_NODE_ID = 'LLM_1';

/**
 * One LLM node wired to the entry point and routed to `END`.
 *
 * Authored as text rather than dumped from an object so the user reads the
 * comments and the key order the flow editor itself writes
 * (`state -> entry_point -> nodes`, `dumpYaml.helpers.ts`).
 */
export const PIPELINE_STARTER_TEMPLATE = `state:
  input: str
  messages: list
entry_point: LLM_1
nodes:
  - id: LLM_1
    type: llm
    input:
      - input
    input_mapping:
      system:
        type: fstring
        value: You are a helpful assistant.
      task:
        type: variable
        value: input
    output:
      - messages
    transition: END
`;
