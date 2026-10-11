/**
 * Shared type definitions for the pipeline flow-editor lib (unit A2c). Not a
 * 1:1 port of any single baseline file — the baseline (`apps/elitea-ui`) is
 * plain JS throughout the flow-editor, so these types are newly authored
 * here from the field usage evidenced across every helper this unit ports:
 * `flowEditor.helpers.js`, `parsePipeline.helpers.js`,
 * `parseRunsByEvent.helpers.js`, `state.helpers.js`, `yamlUpdate.helpers.js`,
 * `node.helpers.jsx`, `nodeOperations.helpers.js`, `nodeType.helpers.js`,
 * `conditionDecisionBuilders.helpers.js`, `decisionOutput.helpers.js`.
 *
 * `entities/pipeline`'s own module doc comment explicitly disclaims this
 * shape ("The flow-editor's `nodes`/`edges` client state ... is process-level
 * editing state, not part of this domain type — it belongs to a Wave-2
 * `processes/pipeline-editor` slice, not entities/"), confirming this is the
 * right layer to own it, not a promotion miss.
 *
 * `YamlPipelineNode` is intentionally permissive (index signature) — the
 * pipeline YAML DSL has a materially different field set per `type`
 * (compare `createConditionNodeData`/`createDecisionNodeData`/
 * `createHitlNodeData`/... in `constants/flowEditor.constants.ts`), the
 * baseline never modeled it as a discriminated union, and every helper in
 * this unit reads fields defensively (optional chaining) rather than
 * exhaustively — a strict union would invent a level of validation the
 * baseline never had.
 *
 * **Deliberately local, not `../types`:** a sibling flow-editor sub-unit
 * (A2d) has landed its own `lib/flow-editor/types.ts` with an overlapping
 * `YamlPipelineNode`/`YamlPipelineObject`/`FlowNode`/`FlowEdge`/
 * `PipelineConnection` surface, observed mid-build (verified via `Read`,
 * quoted in this unit's final report) — but that same subtree was observed
 * to disappear and reappear across consecutive filesystem checks minutes
 * apart during this build (the shared-worktree concurrency hazard this
 * batch's brief warns about, not a one-off). Hard-importing a module that
 * unstable would make this unit's own tsc/vitest runs nondeterministic. This
 * file's types are therefore intentionally STRUCTURALLY compatible with
 * A2d's (`FlowGraphConnection` mirrors `PipelineConnection`'s
 * `source`/`target`/`sourceHandle`/`targetHandle` fields, etc.) so that
 * real `PipelineConnection`/`YamlPipelineObject` values from A2d's modules
 * satisfy this file's parameter types via plain TS structural typing at
 * call sites — no import of A2d's `types.ts` is required either direction,
 * and neither side's churn breaks the other's build. This does NOT mean
 * zero coupling: several of A2d's landed helpers (`deletionOperations.
 * helpers.ts`, `connectionOperations.helpers.ts`, `connectionOperations.
 * toNode.helpers.ts`) hard-import `YamlPipelineDocument`/`YamlPipelineNode`
 * from THIS file directly (one-directional, A2d -> A2c, verified via
 * `grep` against their landed source) — a real, harmless, non-circular
 * dependency this unit does not reciprocate.
 */

/** One node in the pipeline's stored YAML `nodes` array. */
export interface YamlPipelineNode {
  readonly id: string;
  readonly type?: string;
  /** Present on Agent/Toolkit/Tool nodes and read defensively by `state.helpers.ts`'s run-event node matcher. */
  readonly toolkit_name?: string;
  readonly tool?: string;
  readonly transition?: string;
  /** Legacy inline condition (pre-migration). New pipelines use a standalone Condition-type node instead. */
  readonly condition?: YamlConditionSpec;
  /** Legacy inline decision (pre-migration, `migerateLegacyNodes`'s subject). New pipelines use `nodes`/`default_output` directly on a Decision-type node. */
  readonly decision?: YamlDecisionSpec;
  /** Decision-node (new format) branch target ids. */
  readonly nodes?: readonly string[];
  readonly default_output?: string;
  /** Router-node branch target ids, or HITL-node named routes. */
  readonly routes?: readonly string[] | Readonly<Record<string, string>>;
  readonly input?: readonly unknown[];
  readonly output?: readonly unknown[];
  readonly input_mapping?: Readonly<Record<string, YamlInputMappingEntry>>;
  readonly variables_mapping?: Readonly<Record<string, unknown>>;
  readonly structured_output?: boolean;
  readonly function?: unknown;
  readonly description?: string;
  readonly task?: string;
  readonly loop_tool?: string;
  readonly template?: string;
  readonly variables_to_clean?: readonly unknown[];
  readonly code?: { readonly type: string; readonly value: string };
  readonly user_message?: { readonly type: string; readonly value: string };
  readonly edit_state_key?: string;
  /** Router-node's own condition expression (distinct from the legacy `condition` sub-object above). */
  readonly condition_expression?: string;
  readonly [extra: string]: unknown;
}

export interface YamlConditionSpec {
  readonly conditional_outputs?: readonly string[];
  readonly condition_definition?: string;
  readonly condition_input?: readonly unknown[];
  readonly default_output?: string;
}

export interface YamlDecisionSpec {
  readonly nodes?: readonly string[];
  readonly default_output?: string;
  /** Legacy field name for `input`, renamed by `migerateLegacyNodes`. */
  readonly decisional_inputs?: readonly unknown[];
  readonly description?: string;
  readonly input?: readonly unknown[];
}

/** One entry of a node's `input_mapping` (a variable bound to a fixed value, an f-string, or another node's variable). */
export interface YamlInputMappingEntry {
  /** Usually `'fixed' | 'fstring' | 'variable'`, but the DSL never validates it — kept as plain `string` (not a redundant literal-union-widened-to-string) to match. */
  readonly type: string;
  readonly value: unknown;
  readonly enum?: readonly unknown[];
  readonly data_type?: string;
  readonly tooltip?: string;
  readonly multiline?: boolean;
}

/** The full stored pipeline YAML document (`yamlJsonObject` throughout the baseline). */
export interface YamlPipelineDocument {
  readonly nodes?: readonly YamlPipelineNode[];
  readonly state?: Readonly<Record<string, YamlStateVariableSpec | string>>;
  readonly entry_point?: string;
  /** `T | undefined` explicit — `removeInterruptReferences` reassigns these from a nullable-returning array helper. */
  readonly interrupt_before?: readonly string[] | undefined;
  readonly interrupt_after?: readonly string[] | undefined;
  readonly [extra: string]: unknown;
}

/** New-format state variable spec (`{ type, value }`); old-format was a bare type string, still accepted where read. */
interface YamlStateVariableSpec {
  readonly type?: string;
  readonly value?: unknown;
  /** Typed reducer (`append`, `sum_int`, `merge`). Edited in YAML only for now; the canvas keeps it as written. */
  readonly reducer?: string;
}

/** A React-Flow-shaped node/edge id and everything downstream needs — kept structural rather than importing `@xyflow/react`'s `Node`/`Edge` generics into every helper signature. */
export interface FlowGraphNode {
  readonly id: string;
  /** `string | undefined` explicit AND optional — some call sites omit it entirely, others (e.g. `parseYaml`) assign a possibly-`undefined` YAML `type` explicitly. */
  readonly type?: string | undefined;
  readonly data?: Record<string, unknown>;
  readonly position: { readonly x: number; readonly y: number };
  readonly draggable?: boolean;
  readonly measured?: { readonly width?: number; readonly height?: number };
  readonly style?: unknown;
  /** `string | undefined` explicit, same reason as `type` above — `parseYaml` sets it from `node.type` when re-tagging an unrecognised node type as `Default`. */
  readonly originalEliteAType?: string | undefined;
  readonly [extra: string]: unknown;
}

export interface FlowGraphEdge {
  readonly id: string;
  readonly source: string;
  readonly target: string;
  readonly sourceHandle?: string;
  readonly targetHandle?: string;
  readonly type?: string;
  /**
   * `label` is `string | undefined` (not `label?: string`) deliberately —
   * every branch-handler call site computes it as `condition ? 'interrupt' :
   * undefined` and always passes the `data` object, so the field must accept
   * an explicit `undefined` under `exactOptionalPropertyTypes`.
   */
  readonly data?: { readonly label: string | undefined };
}

/** The subset of `@xyflow/react`'s `Connection` this unit's helpers consume. */
export interface FlowGraphConnection {
  readonly source: string;
  readonly target: string;
  readonly sourceHandle?: string | null;
  readonly targetHandle?: string | null;
}
