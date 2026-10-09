/** Private Gate 5a/5c authoring contracts. Runtime admission stays separate. */
export type ExtensionRecord = Readonly<Record<string, unknown>>;
export interface ExtensionSettingsProps {
  readonly node: ExtensionRecord;
  readonly disabled: boolean;
  readonly change: (field: string, value: unknown) => void;
  readonly remove: (field: string) => void;
}
export const SHAPING_LIMITS = {
  input_items: 10_000, output_items: 10_000, groups: 1_000,
  bytes: 512 * 1024, depth: 32, values: 32_768,
} as const;
/** `groups` exists on aggregate only; split_out refuses it as an unknown key. */
export const shapingLimitKeys = (type: unknown): readonly string[] => Object.keys(SHAPING_LIMITS).filter((key) => key !== 'groups' || type === 'aggregate');
export const AGGREGATE_OPERATIONS = [
  'count_rows', 'collect_rows', 'collect', 'sum_int', 'min_int', 'max_int', 'first', 'last',
] as const;
