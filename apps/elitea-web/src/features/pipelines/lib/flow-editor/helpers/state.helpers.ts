/**
 * Ported verbatim from `apps/elitea-ui/src/[fsd]/features/pipelines/
 * flow-editor/lib/helpers/state.helpers.js` (unit A2c). State-drawer
 * variable-name/value helpers (calculation, default/validate/convert by
 * `StateVariableTypes`).
 */
import {
  DRAWER_BREAKPOINT_EXPANDED,
  MIN_DRAWER_WIDTH,
  NAME_FIELD_WIDTH_EXPANDED,
  NAME_FIELD_WIDTH_NARROW,
} from '../constants/stateDrawer.constants';
import {
  DefaultState,
  LegacyIntType,
  STATE_INPUT,
  STATE_MESSAGES,
  StateVariableTypes,
} from '../constants/flowEditor.constants';
import { ValidationErrors } from '../constants/validation.constants';
import { isReservedStateKey } from '../constants/runtimeContract.constants';

/**
 * Calculate dynamic name field width based on drawer width. Uses linear
 * interpolation between breakpoints for smooth transitions.
 * @param drawerWidth - Current drawer width in pixels
 * @returns Calculated name field width in pixels
 */
export const calculateNameFieldWidth = (drawerWidth: number): number => {
  if (drawerWidth >= DRAWER_BREAKPOINT_EXPANDED) {
    return NAME_FIELD_WIDTH_EXPANDED;
  }
  const minWidth = NAME_FIELD_WIDTH_NARROW;
  const maxWidth = NAME_FIELD_WIDTH_EXPANDED;
  const minDrawerWidth = MIN_DRAWER_WIDTH;
  const maxDrawerWidth = DRAWER_BREAKPOINT_EXPANDED;
  const ratio = (drawerWidth - minDrawerWidth) / (maxDrawerWidth - minDrawerWidth);
  return Math.round(minWidth + ratio * (maxWidth - minWidth));
};

export const getDefaultValueForType = (type: string): string | number | unknown[] | Record<string, never> => {
  switch (type) {
    case StateVariableTypes.String:
      return '';
    case StateVariableTypes.Number:
      return 0;
    case StateVariableTypes.List:
      return [];
    case StateVariableTypes.Json:
      return {};
    default:
      return '';
  }
};

/** Legacy integer coercion — `getValueByType`'s `LegacyIntType` case. */
const getLegacyIntValue = (value: unknown): unknown => {
  if (typeof value === 'number') return Math.floor(value); // Ensure integer
  if (typeof value === 'string') {
    const parsed = parseInt(value.trim(), 10);
    return isNaN(parsed) ? value : parsed;
  }
  return value;
};

/** Number coercion — `getValueByType`'s `Number` case. */
const getNumberValue = (value: unknown): unknown => {
  if (typeof value === 'number') return value;
  if (typeof value === 'string' && value.trim()) {
    const parsed = Number(value.trim());
    return isNaN(parsed) ? value : parsed;
  }
  return value;
};

/** JSON-array parse — `getValueByType`'s `List` case; `undefined` on parse failure for the `messages` variable specifically. */
const getListValue = (name: string, value: unknown): unknown => {
  try {
    return JSON.parse(value as string) as unknown;
  } catch {
    return name !== STATE_MESSAGES ? [] : undefined;
  }
};

/** JSON-object parse — `getValueByType`'s `Json` case; the raw value on parse failure. */
const getJsonValue = (value: unknown): unknown => {
  try {
    return JSON.parse(value as string) as unknown;
  } catch {
    return value;
  }
};

export const getValueByType = (name: string, type: string, value: unknown): unknown => {
  switch (type) {
    case StateVariableTypes.String:
      return name !== STATE_INPUT || value ? value : undefined;
    case LegacyIntType:
      return getLegacyIntValue(value);
    case StateVariableTypes.Number:
      return getNumberValue(value);
    case StateVariableTypes.List:
      return getListValue(name, value);
    case StateVariableTypes.Json:
      return getJsonValue(value);
    default:
      return value;
  }
};

export const isDefaultVariableEnabled = (
  states: Readonly<Record<string, unknown>> | undefined,
  name: string,
): boolean => {
  if (!states) {
    return Object.hasOwn(DefaultState, name);
  }
  return Boolean(states[name]);
};

export const getMessagesFromState = (
  states: Readonly<Record<string, { readonly value?: unknown }>> | undefined,
): string | unknown[] =>
  states?.[STATE_MESSAGES]?.value ? JSON.stringify(states[STATE_MESSAGES]?.value, null, 2) : [];

export const validateVariableName = (
  name: string,
  excludeName: string | null | undefined,
  states: Readonly<Record<string, unknown>> | undefined,
): string => {
  if (!name) return '';
  // Allow the current name when editing (excludeName)
  if (states?.[name] !== undefined && name !== excludeName) return ValidationErrors.VariableNameExists;
  if (!/^[a-zA-Z][a-zA-Z0-9_]*$/.test(name)) {
    return ValidationErrors.VariableNameInvalid;
  }
  // The compiler refuses a user-declared `state:` key that collides with one
  // of its own channels (`compiler.rs:1373` -> `reserved_user_state_key`,
  // `compiler.rs:1456`), and it refuses the WHOLE document, not just the
  // variable. Several of those names — `output`, `result`, `session_id`,
  // `chat_history` — are ordinary-looking and pass the character rule above,
  // so without this branch the editor happily mints a pipeline that cannot
  // load. `input`/`messages` are deliberately absent from the reserved list
  // (`builtin_state_key`, `compiler.rs:1436`, is a different, wider set):
  // they are the two `DefaultState` keys the editor itself seeds.
  if (isReservedStateKey(name)) {
    return ValidationErrors.VariableNameReserved;
  }
  return '';
};

/** `validateValueByType`'s `Number` case. */
const validateNumberValue = (value: unknown): string => (isNaN(Number(value)) ? ValidationErrors.NumberFormatInvalid : '');

/** `validateValueByType`'s `List` case: already-an-array, or a JSON-array string, else invalid. */
const validateListValue = (value: unknown): string => {
  if (Array.isArray(value)) return '';
  if (typeof value !== 'string') return ValidationErrors.ListFormatInvalid;
  try {
    return Array.isArray(JSON.parse(value)) ? '' : ValidationErrors.ListFormatInvalid;
  } catch {
    return ValidationErrors.ListFormatInvalid;
  }
};

/** `validateValueByType`'s `Json` case: a non-array object, or a JSON-object string, else invalid. */
const validateJsonValue = (value: unknown): string => {
  if (typeof value === 'object' && value !== null) {
    return Array.isArray(value) ? ValidationErrors.JsonFormatInvalid : '';
  }
  if (typeof value !== 'string') return ValidationErrors.JsonFormatInvalid;
  try {
    // Faithful to the baseline: `typeof null === 'object'` and `!Array.isArray(null)`,
    // so a JSON `null` literal is treated as valid here, matching the original check.
    const parsed: unknown = JSON.parse(value);
    return typeof parsed !== 'object' || Array.isArray(parsed) ? ValidationErrors.JsonFormatInvalid : '';
  } catch {
    return ValidationErrors.JsonFormatInvalid;
  }
};

export const validateValueByType = (type: string, value: unknown): string => {
  // Skip validation for undefined or empty string (optional value)
  if (value === undefined || value === '') {
    return '';
  }
  switch (type) {
    case StateVariableTypes.Number:
      return validateNumberValue(value);
    case StateVariableTypes.List:
      return validateListValue(value);
    case StateVariableTypes.Json:
      return validateJsonValue(value);
    default:
      return '';
  }
};

export const convertValueByType = (type: string, value: unknown): string => {
  // For List and Json types, check if value is already a string (possibly invalid input)
  // If it's a string, return as-is to preserve user input and show validation errors
  if (type === StateVariableTypes.List) {
    if (typeof value === 'string') {
      return value; // Return raw string (may be invalid JSON)
    }
    return JSON.stringify(value); // Stringify valid array
  }

  if (type === StateVariableTypes.Json) {
    if (typeof value === 'string') {
      return value; // Return raw string (may be invalid JSON)
    }
    return JSON.stringify(value, null, 2); // Stringify valid object
  }

  if (type === StateVariableTypes.Number || type === LegacyIntType) {
    return String(value);
  }

  if (typeof value === 'string') {
    return value;
  }

  if (Array.isArray(value)) {
    return JSON.stringify(value);
  }

  return JSON.stringify(value, null, 2);
};

/**
 * Format state variables for AI prompt context. Extracts variable names and
 * types from pipeline state.
 * @param state - The pipeline state object (`yamlJsonObject.state`)
 * @returns Formatted string of state variables for prompt injection
 */
export const formatStateVariablesForPrompt = (
  state: Readonly<Record<string, { readonly type?: string }>> | null | undefined,
): string => {
  if (!state || typeof state !== 'object') {
    return '';
  }

  const entries = Object.entries(state);
  if (entries.length === 0) {
    return '';
  }

  // Map type codes to readable names
  const typeMap: Readonly<Record<string, string>> = {
    [StateVariableTypes.String]: 'str',
    [StateVariableTypes.Number]: 'number',
    [StateVariableTypes.List]: 'list',
    [StateVariableTypes.Json]: 'dict',
    [LegacyIntType]: 'int',
  };

  // Format each variable as: name (type)
  const formattedVars = entries
    .map(([name, config]) => {
      // `||`, not `??` — a state variable with an explicit empty-string `type` (`{ type: '' }`)
      // must also normalize to 'str'; `??` only substitutes on `null`/`undefined` and would
      // list a blank type instead. Matches baseline `state.helpers.js:206`'s `config?.type || 'str'`.
      const type = config?.type || 'str';
      const readableType = typeMap[type] ?? type;
      return `\`${name}\` (${readableType})`;
    })
    .join(', ');

  return `Available pipeline state variables: ${formattedVars}`;
};

/**
 * Format available pipeline nodes for AI prompt context. Extracts node ids
 * from pipeline for routing decisions.
 * @param nodes - The pipeline nodes array (`yamlJsonObject.nodes`)
 * @returns Formatted string of node ids for prompt injection
 */
export const formatAvailableNodesForPrompt = (
  nodes: readonly { readonly id?: string }[] | null | undefined,
): string => {
  if (!nodes || nodes.length === 0) {
    return '';
  }

  // Extract node ids and filter out special nodes
  const nodeIds = nodes
    .map(node => node.id)
    .filter((id): id is string => Boolean(id && id !== 'state' && !id.includes('~~~')))
    .map(id => `\`${id}\``);

  if (nodeIds.length === 0) {
    return '';
  }

  // Always include END as it's a valid routing target
  const allTargets = [...nodeIds, '`END`'].join(', ');

  return `Available routing targets (node IDs): ${allTargets}`;
};
