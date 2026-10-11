/**
 * Shared shapes for the pipeline **graph admission** surface.
 *
 * Admission = "would the Rust pipeline compiler accept this document?".
 * The authority is `services/elitea-worker-rust/src/agents/graph/` —
 * `compiler.rs` (`PipelineDefinition::from_yaml` -> `from_raw` ->
 * `parse_pipeline_nodes` -> `validate_node_state` / `validate_state`) plus
 * the per-family `from_raw` in `application.rs`, `decision.rs`,
 * `direct_tool.rs`, `hitl.rs`, `llm.rs`, `printer.rs`, `router.rs` and
 * `state_modifier.rs`. Every one of those structs is
 * `#[serde(deny_unknown_fields)]`, so a document is admitted whole or not
 * at all: ONE bad node rejects the entire pipeline.
 *
 * Why the editor has to know this at all: there is no server-side YAML
 * validation route (`publish_validate` is a publication check, not a graph
 * check), so until this module existed the first signal a user got that a
 * graph was illegal was a failed chat turn — in a different process, with
 * a stable, data-free error string (`graph.pipeline.invalid_configuration`)
 * that names no field.
 *
 * This module is a MIRROR, not a second authority. The compiler still
 * refuses anything that slips through. Every rule cites the exact runtime
 * `file:line` it transcribes so the next runtime change has a diff to
 * re-check.
 */
import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';

/**
 * One admission rule. The ids are stable — the panels key off them and the
 * unit suite has one case per id (`graphAdmission.helpers.test.ts`).
 */
export type GraphAdmissionRuleId =
  /** `compiler.rs:459-463` — a pipeline holds between 1 and `MAX_PIPELINE_NODES` (128) nodes. */
  | 'document.node-count'
  /** `compiler.rs:464-468` (malformed) and `compiler.rs:477-481` (names no node). */
  | 'document.entry-point'
  /** `compiler.rs:187-217, 1971-1984` — each pause list contains at most 128 unique stored node identifiers. */
  | 'document.static-interrupts'
  /** `compiler.rs:1373-1377` + `yaml.rs:371-378` + `compiler.rs:1456-1486` — state key grammar and the reserved list. */
  | 'state.key'
  /** `compiler.rs:1370-1372` / `compiler.rs:1378-1390` — the six admitted state types. */
  | 'state.type'
  /** `compiler.rs:1391-1395` — `input` must be `str`, `messages` must be `list`. */
  | 'state.builtin-type'
  /**
   * `compiler.rs:2774` (`typed_reducer`) — a declared `reducer` is a notice, never a refusal: the
   * visual editor cannot edit it yet, so the notice keeps it from being dropped unseen.
   */
  | 'state.reducer'
  /** `compiler.rs:1242-1270` — `parse_pipeline_node`'s allow-list of `type:` values. */
  | 'node.type'
  /** `yaml.rs:362-369` (`valid_graph_id`), `compiler.rs:1220-1224` (reserved), `compiler.rs:1225-1229` (unique). */
  | 'node.id'
  /** Per-family required serde fields — `application.rs:53-54,124,146`, `direct_tool.rs:52-53,159`, `printer.rs:41`, `decision.rs:40`, `hitl.rs:470-478`. */
  | 'node.required-field'
  /** `router.rs:330-335` (`validate_target`) plus `compiler.rs:482-490` (target must be `END` or a declared node). */
  | 'node.route-target'
  /** `compiler.rs:1277-1290` and the per-family arms at `compiler.rs:1293-1338`, `1344-1351`. */
  | 'node.state-reference'
  /** `llm.rs:171-190` and `direct_tool.rs:182-186` — structured-output cardinality. */
  | 'node.structured-output'
  /** Private extension contract checks. Production type admission stays disabled. */
  | 'node.extension-configuration';

/** One refusal, named precisely enough for a node panel to point at the offending control. */
export interface GraphAdmissionIssue {
  readonly rule: GraphAdmissionRuleId;
  /** The node the issue belongs to, or `undefined` for a document-level issue (`entry_point`, `state:`, interrupts). */
  readonly nodeId: string | undefined;
  /** The exact YAML field the compiler read — `output`, `input_mapping.task`, `transition`, `state.<key>`, `entry_point`. */
  readonly field: string;
  /** The exact offending value or key, verbatim. Empty string when the field itself is missing. */
  readonly subject: string;
  /**
   * User-facing sentence. Deliberately NOT routed through `t()`: every one
   * of these embeds a YAML field name and a user-authored identifier, which
   * are not translatable, and the i18n rule in force here is `jsx-only`
   * (`.oxlintrc.json:163-170`) so a plain string in a `.ts` helper is
   * legal. The panel chrome around them IS translated.
   */
  readonly message: string;
  /** `file:line` in `services/elitea-worker-rust/src/agents/graph/`. */
  readonly citation: string;
  /** `'warning'` is a notice that never blocks a save. Absent means the compiler refuses the document. */
  readonly severity?: 'warning';
}

/** One node, normalised: `id`/`type` resolved to strings so no rule has to re-narrow them. */
export interface AdmissionNode {
  readonly id: string;
  readonly type: string;
  readonly raw: YamlPipelineNode;
}

/** The document plus the three lookup sets every rule needs, built once per collection pass. */
export interface AdmissionGraph {
  readonly document: YamlPipelineDocument;
  readonly nodes: readonly AdmissionNode[];
  /** Declared node ids. `END` is NOT a member — it is a target keyword, not a node (`compiler.rs:484`). */
  readonly nodeIds: ReadonlySet<string>;
  /** Keys of the document's own `state:` mapping. */
  readonly stateKeys: ReadonlySet<string>;
  /** `state:` key -> its normalised type name (`str`/`int`/`float`/`bool`/`list`/`dict`), for the LLM output schema. */
  readonly stateTypes: ReadonlyMap<string, string>;
}

/** A rule: an id, the runtime line it transcribes, and the check itself. */
export interface GraphAdmissionRule {
  readonly id: GraphAdmissionRuleId;
  /** `file:line` in the Rust compiler this rule mirrors. */
  readonly citation: string;
  /** One line, for the rule catalogue in the unit test. */
  readonly summary: string;
  readonly check: (graph: AdmissionGraph) => readonly GraphAdmissionIssue[];
}

/** Terse `GraphAdmissionIssue` constructor — every rule builds its issues through this. */
export function admissionIssue(
  rule: GraphAdmissionRuleId,
  citation: string,
  nodeId: string | undefined,
  field: string,
  subject: string,
  message: string,
): GraphAdmissionIssue {
  return { rule, citation, nodeId, field, subject, message };
}

/**
 * A YAML list read as strings. The compiler's `Vec<String>` fields reject a
 * non-string entry at deserialize time, so a non-string here is dropped from
 * the rule's view and reported by `node.state-reference`/`node.route-target`
 * as the malformed entry it is.
 */
export function toStringList(value: unknown): readonly string[] {
  if (!Array.isArray(value)) return [];
  return value.filter((entry): entry is string => typeof entry === 'string');
}

/** How many entries of `value` are NOT strings — the compiler refuses the document over any one of them. */
export function countNonStringEntries(value: unknown): number {
  if (!Array.isArray(value)) return 0;
  return value.filter((entry) => typeof entry !== 'string').length;
}

/** `node.id`/`node.type` as the compiler reads them (`compiler.rs:1239` requires a string `type`). */
export function readNodeIdentity(node: YamlPipelineNode): { readonly id: string; readonly type: string } {
  return {
    id: typeof node.id === 'string' ? node.id : '',
    type: typeof node.type === 'string' ? node.type : '',
  };
}
