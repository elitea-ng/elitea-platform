/**
 * The toolkits / MCPs list's row helpers (`../Toolkits.tsx`): a generated
 * `ToolkitInstance` as the list's item, the "Types" filter's entries, and the
 * toolbar's search over the loaded rows.
 */
import type { ToolkitListItem } from '@/features/toolkits';
import type { ToolkitInstance } from '@/shared/api/generated/model';

/** `settings` narrowed to the two string fields `ToolkitListItem`/`toolkitDisplayName`'s fallback chain actually reads — `ToolkitInstance.settings` is an untyped `Record<string, unknown>` (arbitrary per-type JSON), not directly assignable to `ToolkitListItem`'s narrower shape. */
export function toListItem(row: ToolkitInstance): ToolkitListItem {
  const settings = row.settings;
  const eliteaTitle = typeof settings['elitea_title'] === 'string' ? settings['elitea_title'] : undefined;
  const configurationTitle = typeof settings['configuration_title'] === 'string' ? settings['configuration_title'] : undefined;
  return {
    id: row.id,
    name: row.name,
    type: row.type,
    ...((eliteaTitle !== undefined || configurationTitle !== undefined) && {
      settings: {
        ...(eliteaTitle !== undefined && { elitea_title: eliteaTitle }),
        ...(configurationTitle !== undefined && { configuration_title: configurationTitle }),
      },
    }),
  };
}

/** `tagList` — every distinct toolkit TYPE among the currently-loaded rows, name-sorted. Simpler than `useLoadToolkits.ts`'s own `buildProjectWideTagList` (the full schema-catalogue-derived list): that hook's `toolkitSchemas`/label-lookup machinery (`useGetCurrentToolkitSchemas`) is not exported from `features/toolkits`' public `index.ts` either (same §3.5 budget ceiling — see that barrel's own doc comment), so this reads the raw `type` string directly off whatever rows have actually loaded, same "presentation-layer, not a full re-implementation" scope every other disclosed gap in this unit takes. */
export function toTagList(rows: readonly ToolkitInstance[]): { readonly id: number; readonly name: string }[] {
  const seen = new Set<string>();
  const tags: { id: number; name: string }[] = [];
  for (const row of rows) {
    if (seen.has(row.type)) continue;
    seen.add(row.type);
    tags.push({ id: tags.length + 1, name: row.type });
  }
  return tags.sort((a, b) => a.name.localeCompare(b.name));
}

/** The toolbar's search over the loaded rows: the list endpoint takes no `query` (`ListToolkitInstancesParams` is `limit`/`offset` only). */
export function matchesQuery(item: ToolkitListItem, query: string): boolean {
  const needle = query.trim().toLowerCase();
  if (needle === '') return true;
  return [item.name, item.type, item.settings?.elitea_title, item.settings?.configuration_title].some((value) => value?.toLowerCase().includes(needle) === true);
}
