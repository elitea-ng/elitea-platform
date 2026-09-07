/**
 * Adding a source toolkit to an Inventory toolkit, and taking one away.
 *
 * A SOURCE IS A FIELD OF THE TOOLKIT, not a resource of its own. `sources` is a
 * JSON list of toolkit ids inside the Inventory toolkit's settings
 * (`descriptor.json`'s `toolkit_config.parameters.sources`), so adding one is a
 * PUT of the toolkit with a longer list. There is no `POST /sources` to call,
 * and the legacy UI did exactly this.
 *
 * THE WHOLE TOOLKIT IS SENT, not a settings patch: the route is a PUT and
 * replaces the resource, so posting only `settings` would clear every other
 * field the row carries. That is the same reason `features/wiki-settings`
 * spreads the toolkit, and it is preserved rather than tidied into a PATCH the
 * API does not serve.
 *
 * REMOVING A SOURCE DOES NOT REMOVE ITS ENTITIES. They stay in the graph until
 * `remove_source_entities` runs, and the sources table keeps showing them as a
 * row with no toolkit id for exactly that reason. The confirmation says so,
 * because a user who expects the removal to clean the graph will not understand
 * why the entity counts did not move.
 */
import { useMutation, useQueryClient, type UseMutationResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { inventoryConfig, type InventorySettings } from '@/entities/inventory';

/**
 * The toolkit types this facade can expand into source credentials.
 *
 * FOUR, because that is what the descriptor advertises
 * (`json_schema_extra.toolkit_types`). Only two of them are wired in the
 * facade's `SourceKinds` today — github and ado_repos — and offering only
 * those two here would hide a source the descriptor says is allowed; offering
 * all four and letting the facade refuse gives the user the provider's own
 * reason instead of a control that is silently missing.
 */
export const SOURCE_TOOLKIT_TYPES: readonly string[] = ['github', 'ado_repos', 'gitlab', 'bitbucket'];

/** What a save needs: the row as last read, and the new source list. */
export interface SaveSourcesInput {
  readonly projectId: string;
  readonly toolkitId: string;
  /** The toolkit as last read, so the PUT carries every field it already had. */
  readonly toolkit: Readonly<Record<string, unknown>>;
  readonly settings: InventorySettings;
  readonly sourceIds: readonly string[];
}

/**
 * The settings a save writes: the existing ones with a new `sources` list, and
 * `source_configs` narrowed to the sources that remain.
 *
 * NARROWING IS THE POINT of doing this in a function with a test. A removed
 * source leaves its per-source overrides behind, and the next source that
 * happens to be created with the same id inherits a branch and a pattern set
 * nobody chose for it. The facade reads `source_configs` by id
 * (`mergeSourceConfig`, internal/api/v2/inventory/sources.go:143-146), so the
 * inheritance would be silent and would change what gets ingested.
 */
export function settingsWithSources(
  settings: InventorySettings,
  sourceIds: readonly string[],
): InventorySettings {
  const kept: Record<string, unknown> = {};
  for (const id of sourceIds) {
    const config = inventoryConfig.sourceConfig(settings, id);
    const entry: Record<string, unknown> = {};
    if (config.branch !== '') entry['branch'] = config.branch;
    if (config.filePatterns !== '') entry['file_patterns'] = config.filePatterns;
    if (config.excludePatterns !== '') entry['exclude_patterns'] = config.excludePatterns;
    if (config.preset !== '') entry['preset'] = config.preset;
    if (Object.keys(entry).length > 0) kept[id] = entry;
  }
  return { ...settings, sources: [...sourceIds], source_configs: kept };
}

/**
 * Save a new source list.
 *
 * Invalidates `['inventory']` and not only the toolkit: the sources table, the
 * graph reads and the statistics all describe a toolkit whose configuration
 * just changed, and a screen that refreshed only the row it saved would keep
 * listing a source that is no longer configured.
 */
export function useSaveSources(): UseMutationResult<unknown, Error, SaveSourcesInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: SaveSourcesInput) => {
      const path = `/elitea_core/tool/prompt_lib/${input.projectId}/${input.toolkitId}`;
      return eliteaFetch<unknown>(path, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          ...input.toolkit,
          settings: settingsWithSources(input.settings, input.sourceIds),
        }),
      });
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['inventory'] });
    },
  });
}
