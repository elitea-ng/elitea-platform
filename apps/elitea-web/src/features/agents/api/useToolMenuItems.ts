import { useMemo } from 'react';

import { ToolTypes, toolkitTypeMenuEntries } from '@/entities/toolkit';
import type { ToolkitTypeSchemaMap } from '@/entities/toolkit';
import { useListToolkits } from '@/shared/api/generated/toolkits/toolkits';

/**
 * Ported from `apps/elitea-ui/src/hooks/application/useToolMenuItems.jsx`
 * (Wave-2 unit A1e). Builds the ordered, labelled list of toolkit TYPES the
 * "Toolkit"/"MCP" add-menus offer (`ToolMenu`'s own dropdown items) — the
 * icon/`onClick` half of the baseline hook is left to the caller, same as
 * `entities/toolkit/model/toolMenu.ts`'s own module doc comment already
 * establishes for the pure filtering/labelling logic it promoted from this
 * exact baseline file.
 *
 * **REAL BACKEND GAPS this port ran into (disclosed):**
 *
 *  1. No `{mcp: true}`-equivalent query param exists on the generated
 *     `listToolkits`/`useListToolkits` (grepped `ListToolkitsParams` —
 *     there is none; `listToolkits(projectId)` takes no params at all). The
 *     baseline's `useGetCurrentMCPSchemas` issued a SEPARATE fetch with
 *     `params: { mcp: true }` against a richer legacy-era endpoint returning
 *     a whole prebuilt-MCP-server catalogue distinct from the general
 *     toolkit-type map. That catalogue has no generated-client equivalent
 *     to fetch at all. Rather than inventing a second request this app's
 *     backend cannot serve, the `isMCP` branch derives its schema map from
 *     the SAME single `listToolkits` response, filtered to mcp-flavoured
 *     entries (`mcpFlavouredToolkitTypeSchemas`/`nonMcpFlavouredToolkitTypeSchemas`
 *     below — a LOCAL duplicate of `entities/toolkit/model/toolMenu.ts`'s
 *     own `mergeMcpToolkitTypeSchemas`/`nonMcpToolkitTypeSchemas`: those two
 *     are real, working functions in that file, but are NOT re-exported
 *     from `entities/toolkit`'s public `index.ts` — its own doc comment
 *     says so explicitly ("several more pure helpers exist in those files
 *     but are NOT re-exported here yet... intra-slice-only for now"), and
 *     R-L3 forbids a deep import straight to `model/toolMenu.ts` to reach
 *     them anyway. Small, pure, and cheap to duplicate — same class of
 *     boundary-crossing duplication `entities/application-form/model/
 *     initialValues.ts` already documents for `LATEST_VERSION_NAME`).
 *     This is provably correct for the one case that matters today (the
 *     backend's own `'mcp'`-keyed entry, when present) and simply yields an
 *     empty list for anything the removed prebuilt-catalogue endpoint used
 *     to add on top — a real, observable feature reduction, not a silent
 *     reinterpretation.
 *  2. The current Go `ListTypeSchemas` handler
 *     (`services/elitea-main/internal/api/v2/toolkits/handler.go:100-233`)
 *     returns a STATIC map of bare `{type, properties}` JSON schemas with
 *     NO `metadata` object at all (no `label`/`hidden`/`categories`/
 *     `application` — confirmed by reading the handler directly) and no
 *     `'mcp'`-keyed entry. `toolkitTypeMenuEntries`'s `metadataOf()` already
 *     degrades this gracefully (empty metadata, not a crash); combined with
 *     `ToolTypes`' FE-owned label overrides, backend entries with a known
 *     override (`github`, `jira`, `openapi`, `custom`, `artifact`,
 *     `application`, ...) still render with a real label, entries with
 *     neither backend metadata NOR an override (`database`, `datasource` —
 *     the two real static-map keys `ToolTypes` does not list) resolve to an
 *     empty
 *     label and are filtered out below — matching the baseline's own
 *     `!!obj.label` guard exactly, just against a much sparser real
 *     catalogue than the baseline's legacy backend ever served. Tracked
 *     against the same class of gap already flagged for `entities/toolkit`'s
 *     `Toolkit` (not `ToolkitTypeSchemaMap`) shape.
 */

function metadataOf(schema: Readonly<Record<string, unknown>> | undefined): Readonly<Record<string, unknown>> {
  const metadata = schema?.['metadata'];
  return typeof metadata === 'object' && metadata !== null ? (metadata as Readonly<Record<string, unknown>>) : {};
}

function isMcpFlavouredKey(key: string, value: Readonly<Record<string, unknown>>): boolean {
  return key.toLowerCase() === 'mcp' || value['type'] === 'mcp' || key.toLowerCase().endsWith('mcp');
}

/** Local duplicate of `entities/toolkit/model/toolMenu.ts`'s `nonMcpToolkitTypeSchemas` — see gap 1 above for why it can't be imported. */
function nonMcpFlavouredToolkitTypeSchemas(schemas: ToolkitTypeSchemaMap): ToolkitTypeSchemaMap {
  return Object.fromEntries(Object.entries(schemas).filter(([key, value]) => !isMcpFlavouredKey(key, value)));
}

/** The exact inverse of the predicate above — see gap 1 above. */
function mcpFlavouredToolkitTypeSchemas(schemas: ToolkitTypeSchemaMap): ToolkitTypeSchemaMap {
  return Object.fromEntries(Object.entries(schemas).filter(([key, value]) => isMcpFlavouredKey(key, value)));
}

/** Local duplicate of `entities/toolkit/model/toolMenu.ts`'s `mergeMcpToolkitTypeSchemas` — see gap 1 above for why it can't be imported. */
function mergeMcpFlavouredToolkitTypeSchemas(toolkitSchemas: ToolkitTypeSchemaMap, mcpSchemas: ToolkitTypeSchemaMap): ToolkitTypeSchemaMap {
  const mcpKey = Object.keys(toolkitSchemas).find((key) => key.toLowerCase() === 'mcp');
  if (mcpKey === undefined) return mcpSchemas;
  const mcpEntry = toolkitSchemas[mcpKey];
  return { ...mcpSchemas, mcp: { ...mcpEntry, metadata: { ...metadataOf(mcpEntry), label: 'Remote MCP' } } };
}

interface ToolMenuItem {
  readonly key: string;
  readonly label: string;
}

export interface UseToolMenuItemsParams {
  readonly projectId: string | undefined;
  /** @default false */
  readonly isMCP?: boolean;
  /** @default false */
  readonly isApplication?: boolean;
}

export interface UseToolMenuItemsResult {
  readonly toolMenuItems: readonly ToolMenuItem[];
  readonly isFetchingToolkitTypes: boolean;
}

/** Stable identity fallback — a fresh `{}` literal on every render would defeat `useMemo` below (the whole reason this hook memoizes at all). */
const EMPTY_SCHEMAS: ToolkitTypeSchemaMap = {};

export function useToolMenuItems({ projectId, isMCP = false, isApplication = false }: UseToolMenuItemsParams): UseToolMenuItemsResult {
  const query = useListToolkits(projectId ?? '', { query: { enabled: projectId !== undefined } });
  // Same error-envelope-unreachable cast as `features/apps/api/useToolkitTypeSchemas.ts`
  // (eliteaFetch throws instead of resolving with the error variant).
  const responseData = (query.data as { data?: ToolkitTypeSchemaMap } | undefined)?.data;
  const allSchemas = responseData ?? EMPTY_SCHEMAS;

  const activeSchemas = useMemo(() => {
    if (!isMCP) return nonMcpFlavouredToolkitTypeSchemas(allSchemas);
    return mergeMcpFlavouredToolkitTypeSchemas(allSchemas, mcpFlavouredToolkitTypeSchemas(allSchemas));
  }, [allSchemas, isMCP]);

  const entries = useMemo(
    () =>
      // Filter on hasKnownLabel, NOT on `label !== ''`. Entries now always carry
      // a non-empty label (an unknown type falls back to a humanised key, so a
      // toolkit tile can never be a nameless button — see toolMenu.ts). That
      // made the old empty-string test match nothing and would have silently
      // surfaced unknown backend types in this menu, which the baseline hides.
      toolkitTypeMenuEntries(activeSchemas, { isApplication }).filter((entry) => entry.hasKnownLabel),
    [activeSchemas, isApplication],
  );

  const toolMenuItems = useMemo((): readonly ToolMenuItem[] => {
    if (entries.length === 0) return [];
    // Don't include "Custom" for the MCP menu or for the applications (agent/pipeline) menu — matches the baseline exactly.
    const predefined: ToolMenuItem[] = !isMCP && !isApplication ? [{ key: ToolTypes.custom.value, label: ToolTypes.custom.label }] : [];
    const seenKeys = new Set(predefined.map((item) => item.key));
    const merged = [...predefined];
    for (const entry of entries) {
      if (!seenKeys.has(entry.key)) {
        seenKeys.add(entry.key);
        // Project to {key,label} explicitly. `hasKnownLabel` is an entities-layer
        // detail used only by the filter above; letting it ride along would widen
        // this hook's public ToolMenuItem shape by accident.
        merged.push({ key: entry.key, label: entry.label });
      }
    }
    return merged.sort((a, b) => a.label.toLowerCase().localeCompare(b.label.toLowerCase()));
  }, [entries, isMCP, isApplication]);

  return { toolMenuItems, isFetchingToolkitTypes: query.isFetching };
}
