/**
 * The **runtime's** pipeline contract, restated for the visual editor.
 *
 * Nothing in here is a UI preference: every value below is a mechanical
 * transcription of a rule enforced by the Rust pipeline compiler
 * (`services/elitea-worker-rust/src/agents/graph/`). The editor used to
 * author documents the compiler refuses — this module exists so that a
 * refusal shows up as a failing unit test here instead of as
 * `graph.pipeline.invalid_configuration` at run time.
 *
 * Every entry cites the exact file:line it was read from. When the runtime
 * changes, these citations are the diff to re-check.
 *
 * NOT a duplicate of the compiler: the compiler stays the authority and
 * still refuses anything this table lets through. This is the editor's
 * *first* line of defence, not its only one.
 */
import { PipelineNodeTypes, type PipelineNodeType } from './flowEditor.constants';

// ─────────────────────────────────────────────────────────────────────────
// Node identifiers — `yaml.rs:362` `valid_graph_id`
// ─────────────────────────────────────────────────────────────────────────

/**
 * `graph/yaml.rs:362-370` (`valid_graph_id`): a node id must be non-empty,
 * at most {@link MAX_NODE_ID_BYTES} bytes, and consist ONLY of ASCII
 * alphanumerics plus `_`, `-`, `.` and `:`.
 *
 * `valid_graph_id` is called on every stored node's raw `id`
 * (`application.rs:119`, `decision.rs:80`, `direct_tool.rs:154`,
 * `hitl.rs:150`, `router.rs:165`) and on the document's `entry_point`
 * (`compiler.rs:464`), plus on every route/transition target
 * (`router.rs:331`, `hitl.rs:464`, `application.rs:140`,
 * `direct_tool.rs:190`, `state_modifier.rs:108`).
 *
 * A SPACE is not in the set. That is the whole defect this module was added
 * for: the editor used to mint `"Agent 1"`.
 *
 * The literal `END` is the one route/transition target that is legal without
 * naming a node (`compiler.rs:484`, `router.rs:331`, `hitl.rs:464`); it also
 * happens to satisfy this pattern.
 */
export const NODE_ID_PATTERN = /^[A-Za-z0-9_.:-]+$/;

/** `graph/yaml.rs:10` — `MAX_NODE_ID_BYTES`. */
export const MAX_NODE_ID_BYTES = 128;

/**
 * The separator the editor puts between a node's type name and its ordinal
 * when minting an id (`Agent_1`). `_` is the only word-separator-looking
 * character {@link NODE_ID_PATTERN} admits that reads as a space.
 */
export const NODE_ID_WORD_SEPARATOR = '_';

// ─────────────────────────────────────────────────────────────────────────
// Node types — `compiler.rs:1236` `parse_pipeline_node`
// ─────────────────────────────────────────────────────────────────────────

/**
 * `compiler.rs:1236-1270` (`parse_pipeline_node`) — the EXACT set of `type:`
 * values the compiler will parse. Anything else falls into the `_ =>`
 * arm at `compiler.rs:1267` and the whole document is rejected with
 * "the pipeline contains a node type that is not enabled".
 *
 * Deliberately NOT derived from `PipelineNodeTypes` minus a deny-list: this
 * is an allow-list on the runtime's side, so it is an allow-list here too.
 * A node type added to the editor is invisible to the Add-node menu until
 * somebody adds it here, having checked the runtime actually admits it.
 *
 * Code nodes require a configured sandbox runtime at execution time.
 * `custom` remains unsupported by the compiler.
 */
export const CompilerAdmittedNodeTypes: readonly PipelineNodeType[] = [
  PipelineNodeTypes.Code, // compiler.rs:1531 "code"; admitted sandbox runtime required
  PipelineNodeTypes.Decision, // compiler.rs:1243 "decision"
  PipelineNodeTypes.Agent, // compiler.rs:1246 "agent"
  PipelineNodeTypes.Toolkit, // compiler.rs:1249 "toolkit"
  PipelineNodeTypes.Mcp, // compiler.rs:1249 "mcp"
  PipelineNodeTypes.Hitl, // compiler.rs:1252 "hitl"
  PipelineNodeTypes.LLM, // compiler.rs:1255 "llm"
  PipelineNodeTypes.Printer, // compiler.rs:1258 "printer"
  PipelineNodeTypes.Router, // compiler.rs:1261 "router"
  PipelineNodeTypes.StateModifier, // compiler.rs:1264 "state_modifier"
];

const admittedNodeTypeSet: ReadonlySet<string> = new Set<string>(CompilerAdmittedNodeTypes);

/** Whether the pipeline compiler has a `parse_pipeline_node` arm for `type`. */
export const isCompilerAdmittedNodeType = (type: string): boolean => admittedNodeTypeSet.has(type);

// ─────────────────────────────────────────────────────────────────────────
// Reserved state keys — `compiler.rs:1456` `reserved_user_state_key`
// ─────────────────────────────────────────────────────────────────────────

/** One reserved state key plus the runtime line that reserves it. */
export interface ReservedStateKey {
  /** The literal key the compiler refuses in a user-authored `state:` block. */
  readonly key: string;
  /** `compiler.rs` line inside `reserved_user_state_key` that names it. */
  readonly citation: string;
  /** Short reason, for the message the editor shows. */
  readonly reason: string;
}

/**
 * `compiler.rs:1456-1486` (`reserved_user_state_key`), called from
 * `compiler.rs:1373` on EVERY key of the document's own `state:` mapping:
 *
 * ```rust
 * if !valid_output_key(&key) || reserved_user_state_key(&key) {
 * ```
 *
 * Declaring any of these as a pipeline state variable rejects the whole
 * document. Note the asymmetry with `builtin_state_key` (`compiler.rs:1436`):
 * `input` and `messages` are builtin but NOT reserved — they are exactly the
 * two `DefaultState` keys the editor seeds, and they stay legal.
 */
export const ReservedStateKeys: readonly ReservedStateKey[] = [
  // The four private resume/scope channels, held by name, not by literal.
  { key: '__elitea_hitl_resume_v1', citation: 'compiler.rs:1457 (hitl.rs:29)', reason: 'HITL resume channel' },
  { key: '__elitea_tool_resume_v1', citation: 'compiler.rs:1458 (direct_tool.rs:44)', reason: 'direct-tool resume channel' },
  { key: '__elitea_llm_tool_resume_v1', citation: 'compiler.rs:1459 (llm.rs:58)', reason: 'LLM tool resume channel' },
  { key: '__elitea_pipeline_node_event_scope_v1', citation: 'compiler.rs:1460 (node_events.rs:35)', reason: 'node event scope channel' },
  // The four `matches!` literals at compiler.rs:1463-1466.
  { key: '__elitea_application_task_v1', citation: 'compiler.rs:1463 (application.rs:43)', reason: 'Agent task channel' },
  { key: '__elitea_application_messages_v1', citation: 'compiler.rs:1464 (application.rs:44)', reason: 'Agent messages channel' },
  { key: '__elitea_application_result_v1', citation: 'compiler.rs:1465 (application.rs:45)', reason: 'Agent result channel' },
  { key: '__elitea_subgraph_result_v1', citation: 'compiler.rs:1466 (compiler.rs:56)', reason: 'subgraph result channel' },
  // The fifteen plain literals at compiler.rs:1470-1484.
  { key: 'output', citation: 'compiler.rs:1470', reason: 'runtime-owned output channel' },
  { key: 'result', citation: 'compiler.rs:1471', reason: 'runtime-owned result channel' },
  { key: 'router_output', citation: 'compiler.rs:1472', reason: 'router decision channel' },
  { key: 'elitea_response', citation: 'compiler.rs:1473', reason: 'runtime response channel' },
  { key: 'printer_output', citation: 'compiler.rs:1474', reason: 'printer output channel' },
  { key: 'state_types', citation: 'compiler.rs:1475', reason: 'state type table' },
  { key: 'context_info', citation: 'compiler.rs:1476', reason: 'runtime context channel' },
  { key: 'hitl_decisions', citation: 'compiler.rs:1477', reason: 'HITL decision log' },
  { key: 'hitl_interrupt', citation: 'compiler.rs:1478', reason: 'HITL interrupt channel' },
  { key: 'parallel_tasks', citation: 'compiler.rs:1479', reason: 'parallel task channel' },
  { key: '_pipeline_blocked', citation: 'compiler.rs:1480', reason: 'pipeline block flag' },
  { key: 'session_id', citation: 'compiler.rs:1481', reason: 'runtime session identity' },
  { key: 'thread_id', citation: 'compiler.rs:1482', reason: 'runtime thread identity' },
  { key: 'execution_finished', citation: 'compiler.rs:1483', reason: 'run completion flag' },
  { key: 'chat_history', citation: 'compiler.rs:1484', reason: 'runtime chat history' },
];

const reservedStateKeySet: ReadonlySet<string> = new Set(ReservedStateKeys.map(entry => entry.key));

/** Mirrors `compiler.rs:1456`'s `reserved_user_state_key` — true when the compiler refuses `key` in a user `state:` block. */
export const isReservedStateKey = (key: string): boolean => reservedStateKeySet.has(key);
