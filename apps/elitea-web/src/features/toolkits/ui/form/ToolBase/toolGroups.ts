import { t } from '@/shared/i18n';

import type { ToolActionOption } from './ToolActionsItems';

/**
 * Tool GROUPS in the toolkit editor's Tools section — the display side of the
 * classification elitea-main serves at
 * `properties.selected_tools.tool_groups`
 * (`internal/api/v2/toolkits/tool_groups.go`).
 *
 * WHY THE SECTION IS GROUPED AT ALL. The picker used to be one flat chip row.
 * For GitHub that is 26+ chips in a single wrap, with `delete_file` sitting
 * between `create_file` and `get_file_metadata` and nothing telling them
 * apart. The person ticking those chips is deciding what an autonomous agent
 * will be permitted to do, and the screen gave them no way to see that one of
 * the chips was destructive.
 *
 * THE STORED VALUE IS UNCHANGED. `settings.selected_tools` is still the same
 * flat array of tool names it has always been; grouping is a view over it. An
 * agent, a pipeline, or an MCP client reading a saved toolkit sees exactly
 * what it saw before — which is why `ToolActionsSelector` can fall back to the
 * flat list for a toolkit whose tools carry no group metadata, with no
 * migration of anything.
 *
 * THE LABELS LIVE HERE, NOT ON THE SERVER. elitea-main serves opaque group
 * IDs and the fixed order; the wording is this layer's, so it can be
 * translated and reworded without a server release.
 */

/** The fixed order, least to most dangerous. Only the fallback — the served `tool_group_order` wins when present. */
const TOOL_GROUP_ORDER = ['read', 'create_update', 'delete', 'execute'] as const;

export interface ToolGroupDescriptor {
  /** The group's heading. */
  readonly label: string;
  /** The one-word consequence, shown as a badge beside the heading. */
  readonly badge: string;
  /** The ⓘ tooltip: what a tool in this group is allowed to do. */
  readonly hint: string;
}

/** Wording for the four groups elitea-main classifies into. */
export function toolGroupDescriptor(groupId: string): ToolGroupDescriptor {
  switch (groupId) {
    case 'read':
      return {
        label: t('features.toolkits.toolGroups.read.label', 'Read'),
        badge: t('features.toolkits.toolGroups.read.badge', 'Read-only'),
        hint: t('features.toolkits.toolGroups.read.hint', 'Returns data. Nothing is created, changed or destroyed.'),
      };
    case 'create_update':
      return {
        label: t('features.toolkits.toolGroups.createUpdate.label', 'Create & update'),
        badge: t('features.toolkits.toolGroups.createUpdate.badge', 'Changes data'),
        hint: t('features.toolkits.toolGroups.createUpdate.hint', 'Creates or modifies data in the target system.'),
      };
    case 'delete':
      return {
        label: t('features.toolkits.toolGroups.delete.label', 'Delete'),
        badge: t('features.toolkits.toolGroups.delete.badge', 'Destructive'),
        hint: t('features.toolkits.toolGroups.delete.hint', 'Destroys data. Not reversible from Elitea.'),
      };
    case 'execute':
      return {
        label: t('features.toolkits.toolGroups.execute.label', 'Execute'),
        badge: t('features.toolkits.toolGroups.execute.badge', 'Unrestricted'),
        hint: t(
          'features.toolkits.toolGroups.execute.hint',
          'Runs a caller-supplied query, script, pipeline or raw API call. Effect is not bounded by the tool.',
        ),
      };
    default:
      /*
       * A group id this build has no wording for. It is rendered rather than
       * dropped: a served group nobody displays would silently hide every
       * tool in it from the picker, which is the "absence reads as
       * correctness" failure in its most expensive form — the tool would be
       * unselectable and nothing would say why.
       */
      return { label: groupId, badge: '', hint: '' };
  }
}

export interface ToolGroupSection extends ToolGroupDescriptor {
  readonly id: string;
  /** The tools this group holds AFTER the search filter, alphabetical by label. */
  readonly tools: readonly ToolActionOption[];
  /** EVERY tool name in the group, filter or no filter — what the header control toggles (ELITEA-2685 + ELITEA-2696). */
  readonly allToolValues: readonly string[];
  /** Selected out of TOTAL — both counted over the whole group, never over the filtered view (ELITEA-2690). */
  readonly selectedCount: number;
  readonly totalCount: number;
  /** True when every tool in the WHOLE group is selected — what the header control toggles off. */
  readonly allSelected: boolean;
}

export interface BuildToolGroupSectionsParams {
  readonly toolsOptions: readonly ToolActionOption[];
  /** Tool name → group id, as served. A tool missing from it has no group. */
  readonly toolGroups: Readonly<Record<string, string>> | undefined;
  /** The served fixed order. Falls back to `TOOL_GROUP_ORDER`. */
  readonly groupOrder: readonly string[] | undefined;
  readonly selectedTools: readonly string[];
  /** The search box's current text. Empty means no filtering. */
  readonly query: string;
}

export interface ToolGroupSections {
  /** Groups with at least one tool matching the query, in the fixed order. */
  readonly sections: readonly ToolGroupSection[];
  /** True when the toolkit has tools but the query matches none of them. */
  readonly noMatches: boolean;
}

/**
 * Matches a tool against the search text.
 *
 * BOTH the display name and the RAW tool name (ELITEA-2695): the chips read
 * "Read file" while the SDK, the agent prompt and every log line say
 * `read_file`, and a person who knows one spelling must not have to guess the
 * other. Case-insensitive, substring, no tokenisation — `read_f` has to find
 * `read_file`.
 */
function matchesQuery(option: ToolActionOption, query: string): boolean {
  if (query === '') return true;
  const needle = query.toLowerCase();
  return option.label.toLowerCase().includes(needle) || option.value.toLowerCase().includes(needle);
}

function compareByLabel(left: ToolActionOption, right: ToolActionOption): number {
  return left.label.localeCompare(right.label, undefined, { sensitivity: 'base' });
}

/**
 * Splits the available tools into the ordered, counted, filtered sections the
 * Tools section renders.
 *
 * A tool whose name is not in `toolGroups` falls into the FIRST group of the
 * order rather than being dropped — same reasoning as the unknown-group-id
 * case above: an unclassified tool must still be selectable. In practice this
 * does not arise for a served catalogue (elitea-main groups every tool it
 * serves, asserted in `tool_groups_test.go`); it arises for a toolkit whose
 * saved `available_mcp_tools` list has grown past the served classification.
 */
/** The bucketing half of `buildToolGroupSections`, its own function so that function stays inside the §3.5 complexity budget. */
function bucketByGroup(
  toolsOptions: readonly ToolActionOption[],
  toolGroups: Readonly<Record<string, string>> | undefined,
  fallbackGroup: string,
): { readonly byGroup: Map<string, ToolActionOption[]>; readonly encountered: readonly string[] } {
  const byGroup = new Map<string, ToolActionOption[]>();
  const encountered: string[] = [];
  for (const option of toolsOptions) {
    const groupId = toolGroups?.[option.value] ?? fallbackGroup;
    let bucket = byGroup.get(groupId);
    if (bucket === undefined) {
      bucket = [];
      byGroup.set(groupId, bucket);
      encountered.push(groupId);
    }
    bucket.push(option);
  }
  return { byGroup, encountered };
}

export function buildToolGroupSections(params: BuildToolGroupSectionsParams): ToolGroupSections {
  const { toolsOptions, toolGroups, groupOrder, selectedTools, query } = params;
  const order = groupOrder !== undefined && groupOrder.length > 0 ? groupOrder : TOOL_GROUP_ORDER;
  const fallbackGroup = order[0] ?? TOOL_GROUP_ORDER[0];

  const { byGroup, encountered } = bucketByGroup(toolsOptions, toolGroups, fallbackGroup);

  // The served order first, then anything served that this build's order does
  // not name — appended rather than dropped, for the same reason unknown ids
  // are rendered rather than dropped.
  const orderedIds = [...order, ...encountered.filter((id) => !order.includes(id))];

  const selected = new Set(selectedTools);
  const sections: ToolGroupSection[] = [];
  let anyMatch = false;
  for (const groupId of orderedIds) {
    const all = byGroup.get(groupId);
    if (all === undefined || all.length === 0) continue;
    const selectedCount = all.filter((option) => selected.has(option.value)).length;
    const tools = all.filter((option) => matchesQuery(option, query)).sort(compareByLabel);
    if (tools.length > 0) anyMatch = true;
    if (tools.length === 0) continue;
    sections.push({
      ...toolGroupDescriptor(groupId),
      id: groupId,
      tools,
      allToolValues: all.map((option) => option.value),
      selectedCount,
      totalCount: all.length,
      allSelected: selectedCount === all.length,
    });
  }

  return { sections, noMatches: toolsOptions.length > 0 && !anyMatch };
}

/**
 * The next `selected_tools` value after a group header is clicked.
 *
 * It toggles EVERY tool of the group, including the ones the search filter is
 * hiding, and touches nothing outside it (ELITEA-2685). Order is preserved for
 * the tools that stay: the stored array is the toolkit's saved value and
 * reshuffling it would show up as a spurious diff on every save.
 */
export function toggleGroupSelection(params: {
  readonly selectedTools: readonly string[];
  readonly groupTools: readonly string[];
  readonly select: boolean;
}): readonly string[] {
  const { selectedTools, groupTools, select } = params;
  if (!select) {
    const group = new Set(groupTools);
    return selectedTools.filter((tool) => !group.has(tool));
  }
  const already = new Set(selectedTools);
  const added = groupTools.filter((tool) => !already.has(tool));
  return added.length === 0 ? selectedTools : [...selectedTools, ...added];
}
