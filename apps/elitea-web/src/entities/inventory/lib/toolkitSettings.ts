/**
 * Reading an Inventory toolkit's own configuration.
 *
 * FOUR FIELDS DECIDE WHETHER ANYTHING WORKS, and each fails differently:
 *
 *   `bucket`         — where the graph and `sources_status.json` live. The
 *                      provider defaults it to "graphs" when a toolkit
 *                      configures none (run/compose.go:8-13), so a screen that
 *                      shows nothing for a blank bucket would be describing a
 *                      graph that exists.
 *   `llm_model`      — required to INGEST and to ASK; the read tools do not
 *                      need it. So a toolkit with no model is a working browser
 *                      and a refused ingestion, and the screen has to say which.
 *   `sources`        — the source toolkit ids. Numbers on the wire; text here,
 *                      because `source_configs` is keyed by the id AS A STRING
 *                      and comparing 9 to "9" silently loses every override.
 *   `source_configs` — the per-source overrides, hidden from the toolkit form
 *                      (`json_schema_extra.hidden`) and edited only here.
 *
 * THE `toolkit_configuration_` PREFIX is read for every one of them. It is the
 * spelling the legacy UI saved settings under, and the provider still reads
 * both (`ResolveBucket`, run/compose.go:56-66). A toolkit saved by the old app
 * and opened by this one must not read as unconfigured.
 */
import type { InventorySettings } from '../model/types';

/** The two spellings any setting may arrive under, in precedence order. */
function settingValue(settings: InventorySettings | undefined, key: string): unknown {
  if (settings === undefined) return undefined;
  const bare = settings[key];
  if (bare !== undefined && bare !== null && bare !== '') return bare;
  return settings[`toolkit_configuration_${key}`];
}

function asText(value: unknown): string {
  if (typeof value === 'string') return value.trim();
  if (typeof value === 'number' || typeof value === 'bigint') return String(value);
  return '';
}

/** What the provider stores the graph under when a toolkit names no bucket. */
export const DEFAULT_INVENTORY_BUCKET = 'graphs';

/** The bucket this toolkit's graph lives in, defaulted the way the provider defaults it. */
export function inventoryBucket(settings: InventorySettings | undefined): string {
  const bucket = asText(settingValue(settings, 'bucket'));
  return bucket === '' ? DEFAULT_INVENTORY_BUCKET : bucket;
}

/** The extraction model, or '' — which is a toolkit that can be read and not ingested. */
export function inventoryLlmModel(settings: InventorySettings | undefined): string {
  return asText(settingValue(settings, 'llm_model'));
}

/**
 * The source toolkit ids this Inventory toolkit ingests from, as text, in the
 * order they were saved. Duplicates are dropped: the list is a set in every
 * way that matters (`source_configs` is keyed by id) and a repeated id would
 * render two rows that share one status.
 */
export function inventorySourceIds(settings: InventorySettings | undefined): readonly string[] {
  const raw = settingValue(settings, 'sources');
  if (!Array.isArray(raw)) return [];
  const ids: string[] = [];
  for (const entry of raw as readonly unknown[]) {
    const id = asText(entry);
    if (id !== '' && !ids.includes(id)) ids.push(id);
  }
  return ids;
}

/** One source's stored overrides. Every field is optional and every one is text. */
export interface SourceConfig {
  readonly branch: string;
  readonly filePatterns: string;
  readonly excludePatterns: string;
  readonly preset: string;
}

const EMPTY_SOURCE_CONFIG: SourceConfig = {
  branch: '',
  filePatterns: '',
  excludePatterns: '',
  preset: '',
};

/**
 * The overrides saved for one source.
 *
 * Keyed by the id AS TEXT, which is how the facade reads it as well
 * (`mergeSourceConfig` indexes `source_configs` with `strconv.Itoa`,
 * internal/api/v2/inventory/sources.go:143-146). A caller holding a number
 * must convert before asking, or it gets the empty config and the ingestion
 * runs against the whole repository.
 */
export function inventorySourceConfig(
  settings: InventorySettings | undefined,
  toolkitId: string,
): SourceConfig {
  const all = settingValue(settings, 'source_configs');
  if (typeof all !== 'object' || all === null || Array.isArray(all)) return EMPTY_SOURCE_CONFIG;
  const entry = (all as Record<string, unknown>)[toolkitId];
  if (typeof entry !== 'object' || entry === null || Array.isArray(entry)) return EMPTY_SOURCE_CONFIG;
  const config = entry as Record<string, unknown>;
  return {
    branch: asText(config['branch']),
    filePatterns: asText(config['file_patterns']),
    excludePatterns: asText(config['exclude_patterns']),
    preset: asText(config['preset']),
  };
}
