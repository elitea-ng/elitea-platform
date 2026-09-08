/**
 * Whether the "Indexes" tab is offered at all, and with which tools —
 * `apps/elitea-ui/src/pages/Toolkits/EditToolkit.jsx`'s own
 * `shouldHideIndexesTab` (lines 205-217) and `selectedIndexTools` (199-203).
 *
 * REGRESSION THIS FIXES (found while wiring #149). The port dropped
 * `shouldHideIndexesTab` entirely and rendered the tab unconditionally, for
 * every toolkit AND every MCP. The baseline's very first branch is
 * `if (mcpId) return true` — on `/mcps/:tab/:mcpId` the tab is given
 * `display: 'none'` (`components/StyledTabs.jsx:241`,
 * `sx={[styles.tab, {display: tab.display}]}`), so an MCP edit screen has NO
 * Indexes tab in the baseline at all. Its second branch hides the tab for
 * any toolkit type whose schema offers no indexing tool. Between them, every
 * screen where the port showed a clickable-but-empty Indexes tab is a screen
 * the baseline never showed one on.
 *
 * DISCLOSED ADAPTATION — where the available tool names are read from. The
 * baseline reads `toolSchema.properties.selected_tools.items.enum`. This
 * backend does not answer that shape: measured against the running stack on
 * 2026-08-09 (`GET /api/v2/elitea_core/toolkits/prompt_lib/1`), every type
 * carries `properties.selected_tools.args_schemas` (an object keyed by tool
 * name) and NONE carries `items.enum`. A byte-faithful port of that
 * expression would therefore hide the tab on every screen, which is the
 * same defect in the other direction. Both shapes are read here — the exact
 * precedent `ui/test-tools/TestToolSettings.tsx:156` already set for the
 * identical lookup ("`args_schemas` keys or `items.enum`").
 *
 * Measured tool availability on that stack: `artifact` and `datasource`
 * offer `index_data`; `application`, `custom`, `database`, `github`, `jira`
 * and `openapi` offer none.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * THE WORKER-CAPABILITY VERDICT (the second gate, added here)
 * ─────────────────────────────────────────────────────────────────────────
 * Offering an indexing tool in the schema says the toolkit CLASS declares
 * one. It does not say the worker that has to run it can import that class.
 * elitea-main answers that separately, on the same type schema:
 * `services/elitea-main/internal/api/v2/toolkits/type_catalogue.go:232-240`
 * writes `metadata.unavailable = true` plus a `metadata.unavailable_reason`
 * sentence whenever `workerCapability.SupportsToolkitType` says no, and the
 * comment there is explicit that the verdict "is written LAST and is not
 * negotiable".
 *
 * Until now NOTHING in this client read it except the type CHOOSER
 * (`entities/toolkit/model/toolMenu.ts:113`, which filters on the sibling
 * `metadata.hidden`). The chooser gate only stops such a type being created
 * NEW — it does nothing for a toolkit that already exists, which is every
 * toolkit created before the capability changed, imported, or seeded. Open
 * one of those and the Indexes tab was fully drawn: a create form built
 * from the very schema whose own metadata says the run cannot happen, an
 * enabled `Index` button, and a run that dies in the worker with nothing on
 * screen to explain why.
 *
 * So the verdict is resolved here, beside the tool rule, and returned as a
 * REASON rather than as a second `hidden`. Hiding the tab would repeat the
 * silence in a quieter key — the toolkit still has indexes, the user still
 * followed a link to one, and "the tab vanished" explains nothing. The tab
 * stays, and `IndexesTab` renders the server's own sentence in place of the
 * form.
 */
import { IndexesToolsEnum } from '../../indexes/lib/constants/indexDetails.constants';

const INDEX_TOOL_NAMES: readonly string[] = Object.values(IndexesToolsEnum);

interface SelectedToolsSchema {
  readonly args_schemas?: Readonly<Record<string, unknown>> | undefined;
  readonly items?: { readonly enum?: readonly string[] | undefined } | undefined;
}

interface ToolkitTypeSchemaLike {
  readonly properties?: { readonly selected_tools?: SelectedToolsSchema | undefined } | undefined;
  readonly metadata?: ToolkitTypeMetadataLike | undefined;
}

/** The two capability fields `type_catalogue.go` writes onto a type's metadata. */
interface ToolkitTypeMetadataLike {
  readonly unavailable?: unknown;
  readonly unavailable_reason?: unknown;
}

/**
 * The server's own sentence for why the worker cannot run this type, or
 * `undefined` when it can.
 *
 * `unavailable` is the gate and the reason is only its explanation, so a
 * verdict that arrives without a sentence still gates — it falls back to a
 * generic line at the render site rather than being read as "available".
 */
function resolveUnavailableReason(schema: ToolkitTypeSchemaLike | undefined): string | undefined {
  const metadata = schema?.metadata;
  if (metadata === undefined || metadata === null) return undefined;
  if (metadata.unavailable !== true) return undefined;
  const reason = metadata.unavailable_reason;
  return typeof reason === 'string' && reason.trim() !== '' ? reason : '';
}

/** The tool names a toolkit TYPE offers — `args_schemas` keys, or the baseline's `items.enum`. */
function availableToolNames(schema: ToolkitTypeSchemaLike | undefined): readonly string[] {
  const selectedTools = schema?.properties?.selected_tools;
  if (selectedTools === undefined) return [];
  const fromArgsSchemas = Object.keys(selectedTools.args_schemas ?? {});
  if (fromArgsSchemas.length > 0) return fromArgsSchemas;
  return selectedTools.items?.enum ?? [];
}

export interface IndexesTabVisibilityParams {
  /** True on the `/mcps/:tab/:mcpId` route — the baseline's `if (mcpId) return true`. */
  readonly isMCP: boolean;
  /** The schema for the toolkit's OWN type, i.e. `toolkitSchemas[editToolDetail.type]`. */
  readonly toolkitTypeSchema: unknown;
  /** The toolkit instance's saved `settings.selected_tools`. */
  readonly selectedTools: unknown;
}

export interface IndexesTabVisibility {
  /** When true the tab is not rendered at all — no label, no panel. */
  readonly hidden: boolean;
  /**
   * The index tools this toolkit actually has selected. Empty means the tab
   * is offered (the TYPE supports indexing) but nothing can be run yet —
   * the baseline's `disableIndexingReason.needToSelectIndexData`.
   */
  readonly selectedIndexTools: readonly string[];
  /**
   * Set when the served type schema carries the worker-capability verdict
   * `metadata.unavailable === true`. The string is the server's own
   * `unavailable_reason`; an empty string means "unavailable, no sentence
   * given". `undefined` means the worker can run this type.
   */
  readonly unavailableReason?: string | undefined;
}

export function resolveIndexesTabVisibility(params: IndexesTabVisibilityParams): IndexesTabVisibility {
  const { isMCP, toolkitTypeSchema, selectedTools } = params;

  const selectedIndexTools = (Array.isArray(selectedTools) ? (selectedTools as readonly unknown[]) : [])
    .filter((tool): tool is string => typeof tool === 'string' && INDEX_TOOL_NAMES.includes(tool));

  if (isMCP) return { hidden: true, selectedIndexTools };

  const schema = toolkitTypeSchema as ToolkitTypeSchemaLike | undefined;
  const schemaTools = availableToolNames(schema);
  const hidden = schemaTools.length === 0 || !schemaTools.some((tool) => INDEX_TOOL_NAMES.includes(tool));

  const unavailableReason = resolveUnavailableReason(schema);
  // `exactOptionalPropertyTypes`: an available type must omit the KEY, not
  // carry an explicit `undefined` — an empty string is a real verdict here.
  if (unavailableReason === undefined) return { hidden, selectedIndexTools };
  return { hidden, selectedIndexTools, unavailableReason };
}
