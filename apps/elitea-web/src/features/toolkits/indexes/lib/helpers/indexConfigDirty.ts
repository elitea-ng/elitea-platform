/**
 * "Has the index configuration on screen diverged from the one the server
 * holds?" — the single question the Indexes tab's Save / Save & Reindex split
 * is gated on (ELITEA-2880/2883/2887).
 *
 * The configuration tab used to have no save at all: the ONLY writer of an
 * index's stored `index_configuration` was an indexing RUN, so a person who
 * changed `progress_step` could persist it only by re-indexing the whole
 * collection. With a save that does not run anything, the buttons have to
 * report which of the two states the form is in, and "dirty" has to mean the
 * same thing here as it does to the server: the values that would be SENT
 * differ from the values that were last STORED.
 *
 * Both helpers are pure and live outside the components on purpose — the
 * comparison is the part worth testing, and it is the part that decides
 * whether the unsaved-changes guard fires.
 */

/**
 * The values the form would show for a freshly-loaded configuration tab: the
 * stored `index_configuration`, restricted to the keys the tool schema
 * declares.
 *
 * Restricted deliberately. A stored configuration written by an older run can
 * carry keys the current schema no longer has; those keys are not editable,
 * are not re-sent, and must not make an untouched form look dirty forever.
 */
export function pickIndexConfigValues(
  schemaKeys: readonly string[],
  values: Readonly<Record<string, unknown>> | undefined,
): Record<string, unknown> {
  const picked: Record<string, unknown> = {};
  if (values === undefined) return picked;
  for (const key of schemaKeys) {
    if (Object.hasOwn(values, key)) picked[key] = values[key];
  }
  return picked;
}

/**
 * Treated as "the field holds nothing".
 *
 * `undefined` (never set), `null` (#311's explicit clear, which the save path
 * preserves rather than dropping) and `''` (an emptied text input) are three
 * spellings of the same state in this form, and a save that round-trips one
 * spelling into another must not leave the form permanently dirty.
 *
 * `[]` and `{}` join them because the FORM produces them where the stored
 * configuration has nothing:
 * `IndexDetails.helpers.ts`'s `computeDefaultConfigValues` resolves a
 * property whose `anyOf` declares `{"type":"array","default":[]}` to `[]`
 * (`include_extensions`, `skip_extensions` on every indexing toolkit type)
 * and an object property to `{}`. Without this, every index whose stored
 * configuration omits one of those keys would open ALREADY DIRTY, the
 * navigation guard would fire on a form nobody touched, and "Reindex" would
 * never be offered at all — the exact failure this comparison exists to
 * prevent, arrived at from the other side.
 *
 * Clearing a NON-empty list is still a change: the comparison is against the
 * stored value, and `['*.png']` is not empty.
 */
function isEmptyConfigValue(value: unknown): boolean {
  if (value === undefined || value === null || value === '') return true;
  if (Array.isArray(value)) return value.length === 0;
  return typeof value === 'object' && Object.keys(value).length === 0;
}

/**
 * Structural equality over one field.
 *
 * `JSON.stringify` with SORTED keys, not a reference or shallow compare: the
 * chunking-config editor hands back a fresh object on every keystroke, so a
 * reference compare would call an untouched form dirty, and an unsorted
 * stringify would call a re-serialized identical object dirty — both of which
 * arm the navigation guard against changes that do not exist.
 */
function sameConfigValue(left: unknown, right: unknown): boolean {
  if (isEmptyConfigValue(left) && isEmptyConfigValue(right)) return true;
  return stableStringify(left) === stableStringify(right);
}

function stableStringify(value: unknown): string {
  return JSON.stringify(value, (_key, nested: unknown) => {
    if (nested === null || typeof nested !== 'object' || Array.isArray(nested)) return nested;
    const entries = Object.entries(nested as Record<string, unknown>).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    return Object.fromEntries(entries);
  }) ?? 'undefined';
}

export interface IndexConfigDirtyParams {
  /** The tool schema's own property keys — the editable surface, and nothing else. */
  readonly schemaKeys: readonly string[];
  /** What the form holds right now (`toolInputVariables`). */
  readonly current: Readonly<Record<string, unknown>>;
  /** What the server last stored, as `pickIndexConfigValues` resolved it. */
  readonly saved: Readonly<Record<string, unknown>>;
}

/** True when a Save would change something. */
export function isIndexConfigDirty(params: IndexConfigDirtyParams): boolean {
  const { schemaKeys, current, saved } = params;
  return schemaKeys.some((key) => !sameConfigValue(current[key], saved[key]));
}
