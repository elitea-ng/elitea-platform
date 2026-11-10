/** Private Gate 5a/5c authoring contracts. Runtime admission stays separate. */
export type ExtensionRecord = Readonly<Record<string, unknown>>;
export interface ExtensionSettingsProps {
  readonly node: ExtensionRecord;
  readonly disabled: boolean;
  readonly change: (field: string, value: unknown) => void;
  readonly remove: (field: string) => void;
}
/**
 * The node types a fan-out node runs directly as its child, per family:
 * `libs/proto/contracts/pipeline-fanout-child-families-v1.json`, which the
 * Worker compiler and Main's save check assert too (W3-2 admits Code).
 */
export const FANOUT_CHILD_FAMILIES = {
  parallelBranch: ['agent', 'llm', 'code'],
  mapWorker: ['state_modifier', 'agent', 'llm', 'code'],
} as const satisfies Readonly<Record<string, readonly string[]>>;
/** A direct Router child is refused in every layer with this reason. */
export const ROUTER_CHILD_TEXT = 'a Router runs inside a child pipeline, not as a Parallel branch or Map worker';
export const SHAPING_LIMITS = {
  input_items: 10_000, output_items: 10_000, groups: 1_000,
  bytes: 512 * 1024, depth: 32, values: 32_768,
} as const;
/** `groups` exists on aggregate only; split_out refuses it as an unknown key. */
export const shapingLimitKeys = (type: unknown): readonly string[] => Object.keys(SHAPING_LIMITS).filter((key) => key !== 'groups' || type === 'aggregate');
export const AGGREGATE_OPERATIONS = [
  'count_rows', 'collect_rows', 'collect', 'sum_int', 'min_int', 'max_int', 'first', 'last',
] as const;
