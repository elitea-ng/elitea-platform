import { ToolTypes } from './toolForm';
import type { ToolkitTypeSchemaMap } from './types';

/**
 * Ported from `apps/elitea-ui/src/hooks/application/useToolMenuItems.jsx`
 * (Wave-2 promotion pass, Part 3). That hook's full surface is Formik-free
 * but still pulls in `@mui/material`'s `useTheme` (for icon colouring),
 * `features/toolkits/lib/hooks` (schema-fetching, features-layer),
 * `common/toolkitUtils`'s `getToolIconByType`, and `JsonIcon` — none of
 * which belong in `entities/` (icon rendering + schema fetching are
 * features-layer concerns, and `entities/` may not import `features/`
 * either way). What IS ported: the pure filtering/labelling/schema-merge
 * decisions the hook made before handing off to icon rendering — the part
 * that is genuinely "toolkit entity" domain logic, not UI.
 */

function metadataOf(schema: Readonly<Record<string, unknown>> | undefined): Readonly<Record<string, unknown>> {
  const metadata = schema?.['metadata'];
  return typeof metadata === 'object' && metadata !== null ? (metadata as Readonly<Record<string, unknown>>) : {};
}

/**
 * The baseline's `isMCP` branch of `toolSchemas`: when the backend's
 * non-MCP schema map has an `mcp`-keyed entry, layer it into the MCP schema
 * map under the `mcp` key with its label overridden to "Remote MCP" (a
 * pre-built "remote MCP toolkit type" entry alongside the user's own
 * discovered MCP servers); otherwise just use `mcpSchemas` as-is.
 */
export function mergeMcpToolkitTypeSchemas(
  toolkitSchemas: ToolkitTypeSchemaMap,
  mcpSchemas: ToolkitTypeSchemaMap,
): ToolkitTypeSchemaMap {
  const mcpKey = Object.keys(toolkitSchemas).find((key) => key.toLowerCase() === 'mcp');
  if (mcpKey === undefined) return mcpSchemas;
  const mcpEntry = toolkitSchemas[mcpKey];
  return {
    ...mcpSchemas,
    mcp: { ...mcpEntry, metadata: { ...metadataOf(mcpEntry), label: 'Remote MCP' } },
  };
}

/** The baseline's non-MCP branch: every toolkit type schema except MCP-shaped ones. */
export function nonMcpToolkitTypeSchemas(toolkitSchemas: ToolkitTypeSchemaMap): ToolkitTypeSchemaMap {
  return Object.fromEntries(
    Object.entries(toolkitSchemas).filter(([key, value]) => {
      const type = value['type'];
      return key.toLowerCase() !== 'mcp' && type !== 'mcp' && !key.toLowerCase().endsWith('mcp');
    }),
  );
}

export interface ToolkitTypeMenuEntry {
  readonly key: string;
  readonly label: string;
  /**
   * True when the label came from a real source — this app's `ToolTypes`
   * override map or the backend's own `metadata.label` — rather than from the
   * humanised-key fallback.
   *
   * This exists because two callers need OPPOSITE things from an unrecognised
   * type, and the label alone can no longer distinguish them. The toolkit
   * create page renders every entry as a tile, so an unknown type must still
   * get a readable accessible name. The agents tool menu deliberately HIDES
   * unknown types (baseline parity: `!!obj.label`), and used to do it by
   * testing `label !== ''` — which silently stopped working the moment the
   * fallback guaranteed a non-empty label. Filter on this flag, not on the
   * label, so the two behaviours stay independent.
   */
  readonly hasKnownLabel: boolean;
  /**
   * True when the backend served `metadata.hidden` for this type — the
   * worker this deployment runs cannot build it (#865;
   * `internal/api/v2/toolkits/type_catalogue.go`'s capability verdict).
   * Only ever populated when the caller opts in with `includeHidden: true`
   * (see the function doc comment) — every other caller keeps getting NO
   * hidden entries at all, exactly as before #865.
   */
  readonly hidden?: boolean;
  /** Set alongside `hidden: true` when the backend named a reason (`metadata.unavailable_reason`). */
  readonly unavailableReason?: string;
}

/**
 * Turn a raw toolkit-type key into a readable label: `ado_boards` -> `Ado
 * Boards`. Used only as a last resort, when neither the frontend override map
 * nor the backend metadata supplies a name — its job is to guarantee that a
 * toolkit tile always has a non-empty accessible name.
 */
function humanizeToolkitTypeKey(key: string): string {
  return key
    .split(/[_\-\s]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(' ');
}

/**
 * The baseline's `toolkitsItems` filter + `toolMenuItems` label mapping,
 * minus icon/`onClick` (features-layer, see module doc): drops
 * internal-tool-categorised and agent/application-labelled entries, keeps
 * only application-type OR non-application-type entries per `isApplication`,
 * and overrides the backend's `metadata.label` with this app's own
 * `ToolTypes[key].label` when one exists.
 *
 * `includeHidden` (default `false`, #865): the baseline dropped
 * `metadata.hidden` entries unconditionally, which is what every caller got
 * before #865/#866 and what every caller EXCEPT the toolkit type picker
 * still gets — that default is preserved deliberately so the agent/pipeline
 * "add tool" dropdown (`features/agents/api/useToolMenuItems.ts`) keeps
 * offering only types that actually work with no UI change of its own to
 * make. Passed `true`, a hidden entry is kept (with `hidden: true` and
 * `unavailableReason` set from `metadata.unavailable_reason`) instead of
 * dropped, so the picker (`features/toolkits/lib/hooks/useToolMenuItems.ts`)
 * can render it as a greyed tile with a reason rather than making a type the
 * catalogue actually has look like it was never offered at all.
 */
export function toolkitTypeMenuEntries(
  schemas: ToolkitTypeSchemaMap,
  options: { readonly isApplication?: boolean; readonly includeHidden?: boolean } = {},
): readonly ToolkitTypeMenuEntry[] {
  const isApplication = options.isApplication ?? false;
  const includeHidden = options.includeHidden ?? false;
  const overrides: Readonly<Record<string, { readonly label: string; readonly value: string }>> = ToolTypes;

  return Object.entries(schemas)
    .filter(([key, value]) => {
      const metadata = metadataOf(value);
      const keyLower = key.toLowerCase();
      const label = typeof metadata['label'] === 'string' ? metadata['label'] : '';
      const labelLower = label.toLowerCase();
      const categories = Array.isArray(metadata['categories']) ? (metadata['categories'] as unknown[]) : [];
      const isInternalTool = categories.includes('internal_tool');
      const isAppType = metadata['application'] === true;
      const shouldInclude = isApplication ? isAppType : !isAppType;
      const isHidden = metadata['hidden'] === true;

      return (
        (includeHidden || !isHidden) &&
        !['agent', 'application'].includes(keyLower) &&
        !['agent', 'application'].includes(labelLower) &&
        !isInternalTool &&
        shouldInclude
      );
    })
    .map(([key, value]) => {
      const metadata = metadataOf(value);
      const backendLabel = typeof metadata['label'] === 'string' ? metadata['label'] : '';
      /*
       * A tile's label is its ONLY accessible name, so an empty one is a
       * critical `button-name` violation, not a cosmetic gap.
       *
       * Both prior sources can be empty at once. The backend's type schemas
       * carry no `metadata.label` at all (measured: every type returns
       * `metadata: {}`), so the label came solely from the frontend ToolTypes
       * map — and the catalogue serves `database` and `datasource`, which that
       * map has no entry for. The result was two buttons with `aria-label=""`
       * and an empty span: unreachable by name, invisible to a screen reader,
       * and axe-critical.
       *
       * Falling back to the humanised key means a type the frontend has never
       * heard of degrades to a plain readable name rather than to nothing. That
       * matters more than the two known keys: the backend can add a type at any
       * time without a frontend release, and the failure mode for that must not
       * be a silent a11y regression.
       */
      const knownLabel = overrides[key]?.label ?? backendLabel;
      const label = knownLabel || humanizeToolkitTypeKey(key);
      const hidden = metadata['hidden'] === true;
      const unavailableReason = typeof metadata['unavailable_reason'] === 'string' ? metadata['unavailable_reason'] : undefined;
      return {
        key,
        label,
        hasKnownLabel: knownLabel !== '',
        ...(hidden ? { hidden: true, ...(unavailableReason ? { unavailableReason } : {}) } : {}),
      };
    })
    .sort((a, b) => a.label.toLowerCase().localeCompare(b.label.toLowerCase()));
}
