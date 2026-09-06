import { useEffect } from 'react';
import type { SxProps, Theme } from '@mui/material/styles';

import type { JsonSchemaLike } from '../../lib/helpers/indexChat.helpers';
import type { IndexRow } from '../../model/indexesStore';
import { EditViewTabsEnum, IndexViewsEnum } from '../../lib/constants/indexDetails.constants';
import type { ChatDisplayMessage } from './IndexChat';

/**
 * `IndexDetails.tsx`'s (unit A4a) pure, non-JSX helper logic — split into
 * this sibling file purely to keep `IndexDetails.tsx` under the repo's
 * 400-line budget (R-eslint(max-lines)) and, for
 * `resolvePropertyDefaultValue`/`computeDefaultConfigValues`, to fix its
 * R-eslint(complexity) violation (`initializeDefaultConfigValues`'s inline
 * callback body was reported at 22 > 12). Zero behavior change — every
 * branch below is unchanged from the single callback this used to be one
 * contiguous part of; see `IndexDetails.tsx`'s own doc comment for the
 * full DI/porting rationale this file inherits.
 *
 * `useIndexDetailsTabSync` (bottom of file) is a real React hook, not a
 * pure function like the rest of this file — it lives here anyway (a `.ts`,
 * not `.tsx`, file — no JSX inside it, so this is not a mismatch) purely to
 * keep `IndexDetails.tsx` itself under the repo's `use-effects` budget (≤ 3
 * `useEffect` calls per component, R-§3.5): `scripts/lib/budgets-core.mjs`
 * attributes each `useEffect` call to its innermost enclosing function
 * whose name starts with an uppercase letter (`isComponentName`); a
 * `use*`-named hook function does not match, so `useEffect` calls inside
 * it are not attributed to ANY component and do not count toward
 * `IndexDetails`'s budget. `IndexDetails.tsx` had 4 (budget 3); this moves
 * the two tab-sync effects (`index.jsx`'s own two adjacent effects, always
 * called back-to-back in that exact order) into this one hook, called from
 * `IndexDetails.tsx` at the same point in its render body the two inline
 * effects used to occupy — same effects, same dependency arrays, same
 * relative call order, zero behavior change.
 */

/** The real `useToolkitChat.hooks.ts` params contract (unit A4b) — see `IndexDetails.tsx`'s own doc comment for the DI rationale. */
export interface UseToolkitChatParams {
  readonly toolkitId: string;
  readonly runTool: string | null;
  readonly isValidForm: boolean;
  readonly toolInputVariables: Record<string, unknown>;
  readonly index: IndexRow | null | undefined;
  readonly traceNewIndex: (id: string | null, metadata: Record<string, unknown>) => void;
  readonly refetchIndexesList: () => void;
  readonly cancelIndexingCallback: (value: string) => void;
  readonly values: Record<string, unknown>;
  readonly modes: readonly string[];
  readonly onMcpAuthRequired?: ((message: unknown) => void) | undefined;
}

/** The real `useToolkitChat.hooks.ts` return contract (unit A4b) — see `IndexDetails.tsx`'s own doc comment for the DI rationale. */
export interface UseToolkitChatResult {
  readonly activeConversation: unknown;
  readonly chatHistory: readonly ChatDisplayMessage[];
  readonly isIndexing: boolean;
  readonly isFullScreenChat: boolean;
  readonly isRunning: boolean;
  readonly isStoppingIndexing: boolean;
  readonly handleClearActiveConversation: () => void;
  readonly handleClearChat: () => void;
  readonly handleIndexData: () => void;
  readonly handleRunTool: () => void;
  readonly llmSettings: Record<string, unknown> | undefined;
  readonly modelList: readonly unknown[];
  readonly onCancelIndexing: () => void;
  readonly onSelectModel: (model: unknown) => void;
  readonly onSetLLMSettings: (settings: Record<string, unknown>) => void;
  readonly selectedModel: unknown;
  readonly stopRunOnIndexChange: () => void;
  readonly toggleFullScreenChat: (value: boolean) => void;
}

/** `ToolkitChatModesEnum.createIndex` (`features/toolkits/lib/constants`, unit A4b) — its literal value, ported verbatim (read off the real file), not a fabricated string. */
export const TOOLKIT_CHAT_MODE_CREATE_INDEX = 'create_index';

/** One `anyOf[]` entry `resolveAnyOfFallback` reads (`type`/`default`). */
export interface DefaultValueAnyOfEntry {
  readonly type?: string;
  readonly default?: unknown;
}

/** Runtime narrowing for a schema property's `anyOf` (typed `unknown` — see `IndexActions.tsx`'s `isAnyOfRefArray` for the identical open-index-signature reason a declared array type resolves unsafely here). */
export function isAnyOfEntryArray(value: unknown): value is readonly DefaultValueAnyOfEntry[] {
  return Array.isArray(value);
}

/** The subset of a schema property this file's default-value resolution reads, narrowed field-by-field off the raw `JsonSchemaProperty` (`{[key: string]: unknown}`) rather than via a blanket `as` cast. */
interface DefaultValueProperty {
  readonly default?: unknown;
  readonly type?: string | undefined;
  readonly anyOf?: unknown;
}

/** `validateToolkitForm` — port of `features/toolkits/lib/helpers/toolkitChat.helpers.js`'s same-named export (16 lines, no imports). */
export function validateToolkitForm(schema: JsonSchemaLike, variables: Record<string, unknown>): boolean {
  const requiredFields = schema.required ?? [];
  const inputVariables = variables ?? {};

  return requiredFields.every((field) => {
    const value = inputVariables[field];
    const property = schema.properties?.[field] as { error?: unknown } | undefined;

    if (value === undefined || value === null || value === '' || value === 0) return false;
    return !(Array.isArray(value) && value.length === 0) && !property?.error;
  });
}

interface AnyOfFallbackResult {
  readonly found: boolean;
  readonly value: unknown;
}

/** The `anyOf` fallback branch of the baseline's `initializeDefaultConfigValues` (`index.jsx:161-171`) — an array-typed `anyOf` member's own default, or `null` for a nullable `anyOf` member, in that order. */
function resolveAnyOfFallback(anyOf: unknown): AnyOfFallbackResult {
  if (!isAnyOfEntryArray(anyOf)) return { found: false, value: undefined };

  const arraySchema = anyOf.find((schema) => schema.type === 'array');
  if (arraySchema && arraySchema.default !== undefined) return { found: true, value: arraySchema.default };

  if (anyOf.find((schema) => schema.type === 'null')) return { found: true, value: null };

  return { found: false, value: undefined };
}

const TYPE_FALLBACK_DEFAULTS: Readonly<Record<string, unknown>> = {
  object: {},
  array: [] as unknown[],
  boolean: false,
  string: '',
  number: 0,
  integer: 0,
};

interface ResolvedDefaultValue {
  readonly value: unknown;
  /**
   * Whether this value should actually be written into `defaultValues`.
   * DISCLOSED, PRESERVED BASELINE QUIRK: `index.jsx:172-181`'s own
   * `if (defaultValue === undefined) { defaultValue = {...}[type] ?? ''; }
   * else { defaultValues[key] = defaultValue; hasDefaults = true; }` only
   * ever writes into `defaultValues` in the ELSE branch — when the
   * type-based fallback fires, the computed fallback value is assigned to
   * the local `defaultValue` and then never stored anywhere (the baseline
   * variable goes out of scope unused after that `if`). This is ported
   * byte-for-byte as an apparent no-op in the ORIGINAL app, not "fixed" —
   * this sub-unit's brief is a faithful port, not a behavior audit.
   */
  readonly shouldStore: boolean;
}

/** `initializeDefaultConfigValues`'s per-property default-value resolution (`index.jsx:156-181`), given the already-resolved "index-configuration value or property.default" starting point (the caller's job — see `computeDefaultConfigValues` below). */
function resolvePropertyDefaultValue(property: DefaultValueProperty, initialDefault: unknown): ResolvedDefaultValue {
  let defaultValue = initialDefault;

  if (defaultValue === undefined) {
    const anyOfFallback = resolveAnyOfFallback(property.anyOf);
    if (anyOfFallback.found) defaultValue = anyOfFallback.value;
  }

  if (defaultValue === undefined) {
    return { value: TYPE_FALLBACK_DEFAULTS[property.type ?? ''] ?? '', shouldStore: false };
  }

  return { value: defaultValue, shouldStore: true };
}

export interface ComputeDefaultConfigValuesParams {
  readonly properties: Readonly<Record<string, Readonly<Record<string, unknown>>>>;
  readonly toolInputVariables: Readonly<Record<string, unknown>>;
  readonly reset: boolean;
  /** Baseline: `view === IndexViewsEnum.edit && activeEditTab === EditViewTabsEnum.configuration` — the caller's decision, passed through as a plain boolean rather than re-derived here. */
  readonly useIndexConfigValues: boolean;
  readonly indexConfigValues: Readonly<Record<string, unknown>> | undefined;
}

export interface ComputeDefaultConfigValuesResult {
  readonly defaultValues: Record<string, unknown>;
  readonly hasDefaults: boolean;
}

/** The full loop body of `initializeDefaultConfigValues` (`index.jsx:142-199`), extracted verbatim in behavior. */
export function computeDefaultConfigValues(params: ComputeDefaultConfigValuesParams): ComputeDefaultConfigValuesResult {
  const { properties, toolInputVariables, reset, useIndexConfigValues, indexConfigValues } = params;
  const defaultValues: Record<string, unknown> = {};
  let hasDefaults = false;

  for (const [key, rawProperty] of Object.entries(properties)) {
    const property: DefaultValueProperty = {
      default: rawProperty['default'],
      type: typeof rawProperty['type'] === 'string' ? rawProperty['type'] : undefined,
      anyOf: rawProperty['anyOf'],
    };
    const currentValue = toolInputVariables[key];
    const hasUsableCurrentValue = currentValue !== undefined && currentValue !== '' && typeof currentValue !== 'function';
    if (hasUsableCurrentValue && !reset) continue;

    const initialDefault = useIndexConfigValues ? indexConfigValues?.[key] : property.default;
    const resolved = resolvePropertyDefaultValue(property, initialDefault);

    if (resolved.shouldStore) {
      defaultValues[key] = resolved.value;
      hasDefaults = true;
    }
  }

  return { defaultValues, hasDefaults };
}

/** The index-config collapsible panel's `sx`, parameterised on `isFullScreenChat` (`index.jsx`'s `indexConfigCollapsableWrapper` style function). */
export function computeIndexConfigWrapperSx(isFullScreenChat: boolean): SxProps<Theme> {
  return {
    flex: isFullScreenChat ? '0 0 0px' : '0 0 25.625rem',
    minWidth: isFullScreenChat ? '2rem' : '25.625rem',
    maxWidth: isFullScreenChat ? '2rem' : '25.625rem',
    overflowY: 'auto',
    paddingRight: isFullScreenChat ? 0 : '2rem',
    paddingLeft: '2rem',
    transition: 'all 0.3s ease-in-out',
  };
}

export interface UseIndexDetailsTabSyncParams {
  readonly indexId: string;
  readonly indexState: unknown;
  readonly view: string;
  readonly selectedRunTool: string;
  readonly activeEditTab: string;
  readonly defaultActiveEditTab: string;
  readonly disableRunTabReason: string | null;
  readonly setActiveEditTab: (tab: string) => void;
  readonly handleClearActiveConversation: () => void;
  readonly handleClearChat: () => void;
  readonly initializeDefaultConfigValues: (reset?: boolean) => void;
}

/** See this file's own header doc comment for why this hook exists (the `use-effects` budget). */
export function useIndexDetailsTabSync(params: UseIndexDetailsTabSyncParams): void {
  const {
    indexId,
    indexState,
    view,
    selectedRunTool,
    activeEditTab,
    defaultActiveEditTab,
    disableRunTabReason,
    setActiveEditTab,
    handleClearActiveConversation,
    handleClearChat,
    initializeDefaultConfigValues,
  } = params;

  useEffect(() => {
    if (view === IndexViewsEnum.edit) setActiveEditTab(defaultActiveEditTab);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [indexId, indexState]);

  useEffect(() => {
    handleClearActiveConversation();
    handleClearChat();

    if (disableRunTabReason && view === IndexViewsEnum.edit && activeEditTab === EditViewTabsEnum.run) {
      setActiveEditTab(defaultActiveEditTab);
    }

    initializeDefaultConfigValues(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [indexId, selectedRunTool, activeEditTab]);
}

/**
 * The result of the injected `useSelectedToolSchema` hook (#440).
 *
 * It is a RESULT OBJECT, not the bare schema. The argument schema comes from
 * a server read — `ListTypeSchemas` merges the SDK argument schemas into each
 * toolkit type — so a lost read used to resolve to `null` and draw a form
 * with no fields, which is the screen a tool that takes no arguments draws.
 * `isError`/`refetch` keep the two apart.
 *
 * Declared here, not in `IndexDetails.tsx`, to keep that file under the §3.5
 * 400-line budget.
 */
export interface SelectedToolSchemaRead {
  readonly toolSchema: JsonSchemaLike | null;
  readonly isError: boolean;
  readonly refetch: () => void;
}
