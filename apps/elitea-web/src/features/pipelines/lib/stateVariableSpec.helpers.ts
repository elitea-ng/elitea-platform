export interface StateVariableSpecView {
  readonly rawSpec: unknown;
  readonly type: string | undefined;
  readonly hasDefault: boolean;
  readonly defaultValue: unknown;
}

/** Read an entry without changing its stored type or default presence. */
export function readStateVariableSpec(rawSpec: unknown): StateVariableSpecView {
  if (typeof rawSpec === 'string') {
    return {
      rawSpec,
      type: rawSpec,
      hasDefault: false,
      defaultValue: undefined,
    };
  }
  if (!isDescriptor(rawSpec)) {
    return {
      rawSpec,
      type: undefined,
      hasDefault: false,
      defaultValue: undefined,
    };
  }
  return {
    rawSpec,
    type: typeof rawSpec['type'] === 'string' ? rawSpec['type'] : undefined,
    hasDefault: Object.hasOwn(rawSpec, 'value'),
    defaultValue: rawSpec['value'],
  };
}

/** Convert only an explicitly edited bare entry. Keep all unknown fields. */
export function patchStateVariableSpec(
  rawSpec: unknown,
  changes: Readonly<Record<string, unknown>>,
): string | Readonly<Record<string, unknown>> {
  if (typeof rawSpec === 'string') {
    const keys = Object.keys(changes);
    if (keys.length === 0 || (keys.length === 1 && changes['type'] === rawSpec)) return rawSpec;
    return { type: rawSpec, ...changes };
  }
  if (!isDescriptor(rawSpec)) throw new Error('Cannot edit a malformed state descriptor');
  return { ...rawSpec, ...changes };
}

function isDescriptor(value: unknown): value is Readonly<Record<string, unknown>> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

export interface StateVariableDisplayConfig {
  readonly type?: string;
  readonly value?: unknown;
}

/** A read-only UI projection. It is never used as the source for an edit. */
export function stateVariableSpecsForDisplay(
  rawState: Readonly<Record<string, unknown>>,
): Readonly<Record<string, StateVariableDisplayConfig>> {
  return Object.fromEntries(
    Object.entries(rawState).map(([name, rawSpec]) => {
      const view = readStateVariableSpec(rawSpec);
      const type = view.type === undefined ? {} : { type: view.type };
      return [name, { ...type, ...(view.hasDefault ? { value: view.defaultValue } : {}) }];
    }),
  );
}

/** Rename retains the exact raw entry; a value/type edit patches only that entry. */
export function patchStateVariableMap(
  rawState: Readonly<Record<string, unknown>>,
  name: string,
  changes: Readonly<Record<string, unknown>>,
): {
  readonly state: Readonly<Record<string, unknown>>;
  readonly stateRename?: { readonly from: string; readonly to: string };
} {
  if (!Object.hasOwn(rawState, name)) throw new Error('Unknown pipeline state root');
  const newName = changes['newName'];
  if (typeof newName === 'string' && newName !== name) {
    if (Object.hasOwn(rawState, newName)) throw new Error('Pipeline state rename collides with an existing root');
    const entries = Object.entries(rawState).map(([key, value]) => [key === name ? newName : key, value] as const);
    return { state: Object.fromEntries<unknown>(entries), stateRename: { from: name, to: newName } };
  }
  const { newName: _newName, ...patch } = changes;
  return { state: { ...rawState, [name]: patchStateVariableSpec(rawState[name], patch) } };
}
