import { FixedParallelNodeDefaults } from './parallel.constants';
/**
 * `InitialNodeData` — the default `data` payload seeded onto a freshly
 * created YAML node, keyed by `PipelineNodeTypes` (baseline
 * `flowEditor.constants.js:193-302`).
 *
 * Split out of `./flowEditor.constants.ts` when the per-node-type doc
 * comments below were added, purely to keep both files under the §3.5
 * 400-line budget. The dependency is one-way (this module imports
 * `PipelineNodeTypes`; `flowEditor.constants.ts` does NOT re-export from
 * here) — a re-export would make the cycle evaluate `PipelineNodeTypes`
 * before its own initializer had run.
 *
 * Heterogeneous by design (the baseline's own shape): the flow editor's
 * consumers read these defensively by node type, never exhaustively.
 *
 * **Every default here is now checked against the Rust pipeline compiler**
 * (`services/elitea-worker-rust/src/agents/graph/`). Before that pass, a
 * freshly added Agent node failed to deserialize at all (no `tool`, no
 * `input_mapping`), and Router/Decision/HITL seeded route targets — empty
 * strings — that `validate_target` refuses. See the per-factory comments.
 */
import { PipelineNodeTypes } from './flowEditor.constants';
import { GraphExtensionDefaults } from './graphExtensionDefaults.constants';


const createBaseNodeData = () => ({
  input: [] as unknown[],
  output: [] as unknown[],
});

const createTransitionNodeData = () => ({
  ...createBaseNodeData(),
  transition: PipelineNodeTypes.End as string,
});

const createStructuredOutputNodeData = () => ({
  ...createTransitionNodeData(),
  structured_output: false,
});

const createToolNodeData = () => ({
  tool: '',
  ...createStructuredOutputNodeData(),
});

const createFunctionNodeData = () => ({
  ...createToolNodeData(),
  function: undefined as unknown,
  input_mapping: {} as Record<string, unknown>,
});

/**
 * `type: toolkit` / `type: mcp` — both parse through the runtime's
 * `DirectToolNodeDefinition` (`compiler.rs:1249`).
 *
 * `toolkit_name` and `tool` are REQUIRED serde fields
 * (`direct_tool.rs:52-53`, neither carries `#[serde(default)]`), so a node
 * missing either fails to deserialize before any validation runs. Both are
 * seeded EMPTY on purpose: `valid_tool_identity` (`direct_tool.rs:1175`)
 * refuses `''`, so the node reads as "required, not yet filled" rather than
 * carrying a plausible-looking value the compiler would reject. The toolkit
 * picker (`BaseToolNode`/`McpNode`) writes both.
 *
 * Deliberately NOT layered on `createFunctionNodeData`: that shape carries a
 * `function` key, and `RawDirectToolNodeDefinition` is
 * `#[serde(deny_unknown_fields)]` (`direct_tool.rs:46`). It survives today
 * only because the value is `undefined` and js-yaml drops undefined keys —
 * an accident, not a contract.
 */
const createDirectToolNodeData = () => ({
  toolkit_name: '',
  tool: '',
  input_mapping: {} as Record<string, unknown>,
  ...createStructuredOutputNodeData(),
});

/**
 * `type: agent` — the runtime's `ApplicationNodeDefinition`
 * (`compiler.rs:1246`).
 *
 * `tool` and `input_mapping` are REQUIRED serde fields
 * (`application.rs:53-54`); a node without them fails to deserialize, which
 * is why a freshly added Agent node used to make the whole document
 * unparseable.
 *
 * `input_mapping` requires `task`; saved pipeline children can also receive
 * declared variables (`application.rs::from_raw`). The default seeds only
 * the required task. `{ type: 'fixed', value: '' }` parses
 * (`parse_task_mapping`, `application.rs:566`) and is refused only at run
 * time when the rendered task is empty (`map_task`, `application.rs:243`),
 * i.e. it is genuinely "empty and required", not a rejected seed.
 *
 * `tool` is left EMPTY on purpose. The runtime calls it a participant alias
 * (`application.rs:49`, `valid_application_alias` at `application.rs:609`
 * accepts nearly any non-empty string) and no legacy artefact pins WHICH
 * string resolves to a saved agent, so seeding a guess would bake an
 * unverified contract into every new pipeline. The Agent picker
 * (`AgentNode.tsx`'s `computeToolkitPatch`) writes the real value.
 */
const createAgentNodeData = () => ({
  tool: '',
  input_mapping: { task: { type: 'fixed', value: '' } } as Record<string, unknown>,
  ...createTransitionNodeData(),
});

const createConditionNodeData = () => ({
  condition_input: [] as unknown[],
  condition_definition: '',
  conditional_outputs: [] as string[],
  default_output: '',
});

/**
 * `type: decision` (`compiler.rs:1243`). `default_output` reaches
 * `RouteTargets::new` (`decision.rs:103`) and then `validate_target`
 * (`router.rs:330`), which admits only `END` or an id passing
 * `valid_graph_id` — `''` fails both, so the previous `default_output: ''`
 * seed made every fresh Decision node reject the document. `END` is also
 * the runtime's own serde default for this field (`decision.rs:51`).
 *
 * `nodes: []` stays: an EMPTY route list is legal (`router.rs:70`); it is an
 * empty-string ENTRY that would not be.
 */
const createDecisionNodeData = () => ({
  input: [] as unknown[],
  description: '',
  nodes: [] as string[],
  default_output: PipelineNodeTypes.End as string,
});

const createLoopNodeData = () => ({
  task: '',
  ...createToolNodeData(),
});

const createLoopFromToolNodeData = () => ({
  tool: '',
  loop_tool: '',
  variables_mapping: undefined as Record<string, unknown> | undefined,
  ...createStructuredOutputNodeData(),
});

/**
 * `type: router` (`compiler.rs:1261`). Same `default_output` rule as
 * Decision above — `RouteTargets::new` (`router.rs:180`) runs
 * `validate_target` (`router.rs:330`) on it, and `''` is neither `END` nor a
 * `valid_graph_id`. `END` matches the runtime's own serde default
 * (`router.rs:47`).
 */
const createRouterNodeData = () => ({
  default_output: PipelineNodeTypes.End as string,
  routes: [] as string[],
  input: [] as unknown[],
  condition: '',
});

const createStateModifierNodeData = () => ({
  template: '',
  variables_to_clean: [] as unknown[],
  ...createBaseNodeData(),
});

const createCodeNodeData = () => ({
  code: { type: 'fixed', value: '' },
  ...createStructuredOutputNodeData(),
});

const createPrinterNodeData = () => ({
  transition: PipelineNodeTypes.End as string,
});

/**
 * `type: hitl` (`compiler.rs:1252`).
 *
 * Every declared route target is checked by `validate_routes`
 * (`hitl.rs:464-468`): it must be `END` or pass `valid_graph_id`. The
 * original seed's `approve: ''` / `edit: ''` failed that on the first save,
 * so a freshly added HITL node rejected the whole document. `approve` and
 * `reject` now default to `END` — the same "goes nowhere yet" meaning every
 * other node type's `transition: END` seed carries — and `approve` alone
 * satisfies `has_action` (`hitl.rs:474-478`).
 *
 * `edit` is OMITTED, not seeded. Two constraints have to hold at once and
 * only omission satisfies both: the seed must be compiler-legal, and it must
 * not trip a validator the node card already runs. `edit: 'END'` is
 * compiler-legal but fails the second — `HITLNode.parts.tsx:352` computes
 * `hasConfiguredEditRoute = Boolean(routes['edit'])`, so `'END'` reads as a
 * configured edit route with no `edit_state_key` and every freshly added
 * node paints "Provide an edit state key before using the Edit route." on a
 * node the user has not touched. `edit: ''` fails the first (`validate_target`).
 * An absent key is neither: `HitlRoutes.edit` is `#[serde(default)]
 * Option<String>` (`hitl.rs:83-84`), so it deserializes to `None`, and
 * `routes.iter()` (`hitl.rs:96-100`) never yields it to `validate_target`.
 * The compiler-legality constraint is the one that ruled out the shape the
 * card would have preferred; the red-error constraint is what ruled out the
 * shape the compiler would have preferred.
 *
 * `user_message.value` was `''`, which `validate_message` (`hitl.rs:440`)
 * refuses; see the field comment below.
 *
 * `edit_state_key` is `undefined`, NOT `''`: the runtime types it
 * `Option<String>` and refuses `Some("")` outright
 * (`hitl.rs:157-165` — `!valid_output_key(key)`), whereas an absent key is
 * `None` and legal. js-yaml drops undefined values, so the key simply does
 * not appear in the stored document until the picker sets one; the reader
 * side already defaults it (`HITLNode.parts.tsx:341`, `?? ''`).
 */
const createHitlNodeData = () => ({
  input: [] as unknown[],
  // `''` is refused: `validate_message` (`hitl.rs:440`) rejects an empty
  // `user_message.value` outright. Unlike the Agent node's participant alias,
  // the right value here IS pinned by the runtime — this is `default_message`
  // (`hitl.rs:625-627`) verbatim, the text serde substitutes when the field is
  // absent. Seeding it visibly (rather than omitting the key) keeps the
  // prompt editable in the node card instead of appearing only at run time.
  user_message: { type: 'fixed', value: 'Please review and approve to continue.' },
  // No `edit` key: see this factory's doc comment. `Boolean(routes['edit'])`
  // is what the node card reads as "an edit route is configured", so any
  // seeded value at all — `'END'` included — renders a red error on a fresh
  // node, and `''` is refused by the compiler.
  routes: {
    approve: PipelineNodeTypes.End as string,
    reject: PipelineNodeTypes.End as string,
  },
  edit_state_key: undefined as string | undefined,
});

/**
 * Default `data` payload seeded onto a freshly-created YAML node, keyed by
 * `PipelineNodeTypes`. Heterogeneous by design (baseline lines 285-302) — see
 * this module's doc comment.
 */
export const InitialNodeData: Readonly<Record<string, Record<string, unknown>>> = {
  ...GraphExtensionDefaults,
  [PipelineNodeTypes.Parallel]: FixedParallelNodeDefaults,
  [PipelineNodeTypes.Tool]: createToolNodeData(),
  [PipelineNodeTypes.Agent]: createAgentNodeData(),
  [PipelineNodeTypes.Pipeline]: createTransitionNodeData(),
  [PipelineNodeTypes.LLM]: createStructuredOutputNodeData(),
  [PipelineNodeTypes.Toolkit]: createDirectToolNodeData(),
  [PipelineNodeTypes.Mcp]: createDirectToolNodeData(),
  [PipelineNodeTypes.Function]: createFunctionNodeData(),
  [PipelineNodeTypes.Condition]: createConditionNodeData(),
  [PipelineNodeTypes.Decision]: createDecisionNodeData(),
  [PipelineNodeTypes.Loop]: createLoopNodeData(),
  [PipelineNodeTypes.LoopFromTool]: createLoopFromToolNodeData(),
  [PipelineNodeTypes.Router]: createRouterNodeData(),
  [PipelineNodeTypes.StateModifier]: createStateModifierNodeData(),
  [PipelineNodeTypes.Code]: createCodeNodeData(),
  [PipelineNodeTypes.Printer]: createPrinterNodeData(),
  [PipelineNodeTypes.Hitl]: createHitlNodeData(),
};
