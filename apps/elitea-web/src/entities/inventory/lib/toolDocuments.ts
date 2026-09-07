/**
 * Turning one Inventory tool's JSON document into the shapes this app renders.
 *
 * THE ROW KEY IS NOT THE SAME IN EVERY DOCUMENT, and that is the provider's
 * legacy shape rather than a defect to normalise away upstream: `search_graph`
 * answers `results`, `query_graph` answers `matches`, and the three
 * `list_entities_by_*` tools answer `entities`. A reader that knows only one
 * of the three renders an empty list for the other two — a 200 with nothing on
 * screen, which is this repository's most-repeated defect (#132). So the row
 * reader takes all three, in one place, and every caller goes through it.
 *
 * EVERY FIELD IS OPTIONAL ON THE WIRE. A node written by an older ingestion
 * has no `layer`; one that came from a citation has no `file_path`. Missing is
 * rendered as empty, never as the string "undefined", and never as a reason to
 * drop the row: an entity the user can see and cannot fully describe is still
 * the entity they searched for.
 */
import type {
  IngestionStatus,
  InventoryCacheStats,
  InventoryCount,
  InventoryCrossRelation,
  InventoryEntity,
  InventoryNeighbour,
  InventoryStats,
} from '../model/types';

type Doc = Record<string, unknown> | undefined;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function text(value: unknown): string {
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'bigint' || typeof value === 'boolean') {
    return String(value);
  }
  return '';
}

/**
 * A count, or null. `null` and `0` are different facts here — "the provider did
 * not say" and "there are none" — and a screen that prints 0 for the first one
 * reports an empty graph for a document it could not read.
 */
function count(value: unknown): number | null {
  if (typeof value === 'number' && Number.isFinite(value)) return value;
  if (typeof value === 'string' && value.trim() !== '') {
    const parsed = Number(value);
    if (Number.isFinite(parsed)) return parsed;
  }
  return null;
}

function list(value: unknown): readonly unknown[] {
  return Array.isArray(value) ? value : [];
}

/** The first of `keys` the document actually carries as an array. */
function rowsUnder(document: Doc, keys: readonly string[]): readonly unknown[] {
  if (document === undefined) return [];
  for (const key of keys) {
    const candidate = document[key];
    if (Array.isArray(candidate)) return candidate;
  }
  return [];
}

/** One entity row, from whichever of the provider's three spellings carried it. */
export function readEntity(row: unknown): InventoryEntity | undefined {
  if (!isRecord(row)) return undefined;
  // `entity` is the nesting `search_graph` used on the legacy platform, where a
  // hit was `{entity: {...}, score}`. Reading through it costs one line and
  // saves an empty list against an engine that still answers that way.
  const inner = isRecord(row['entity']) ? row['entity'] : row;
  const id = text(inner['id']);
  const name = text(inner['name']);
  if (id === '' && name === '') return undefined;
  return {
    // A row with a name and no id is addressable BY NAME: the retrieval tools
    // resolve either (the fixture's own lookup matches both), so falling back
    // keeps such a row clickable rather than inert.
    id: id === '' ? name : id,
    name: name === '' ? id : name,
    type: text(inner['type']),
    layer: text(inner['layer']),
    sourceToolkit: text(inner['source_toolkit']),
    filePath: text(inner['file_path']),
  };
}

/** Every entity row a listing, a search or a query answered. */
export function readEntities(document: Doc): readonly InventoryEntity[] {
  const rows = rowsUnder(document, ['entities', 'results', 'matches']);
  const entities: InventoryEntity[] = [];
  for (const row of rows) {
    const entity = readEntity(row);
    if (entity !== undefined) entities.push(entity);
  }
  return entities;
}

/**
 * The neighbours of one entity.
 *
 * `get_related_entities` and `get_entity_neighbors` answer the same `related`
 * list, which is why one reader serves both — and why the browser can offer
 * "expand connections" without knowing which tool the deployment serves.
 */
export function readNeighbours(document: Doc): readonly InventoryNeighbour[] {
  const rows = rowsUnder(document, ['related', 'neighbors', 'neighbours']);
  const neighbours: InventoryNeighbour[] = [];
  for (const row of rows) {
    if (!isRecord(row)) continue;
    const entityId = text(row['entity_id'] ?? row['id'] ?? row['target']);
    if (entityId === '') continue;
    neighbours.push({
      entityId,
      relationType: text(row['relation_type'] ?? row['type']),
      direction: text(row['direction']),
    });
  }
  return neighbours;
}

/** The edges that join two different sources. */
export function readCrossRelations(document: Doc): readonly InventoryCrossRelation[] {
  const rows = rowsUnder(document, ['relations', 'edges']);
  const relations: InventoryCrossRelation[] = [];
  for (const row of rows) {
    if (!isRecord(row)) continue;
    const source = text(row['source']);
    const target = text(row['target']);
    if (source === '' || target === '') continue;
    relations.push({
      source,
      target,
      relationType: text(row['relation_type'] ?? row['type']),
      fromSource: text(row['from_source']),
      toSource: text(row['to_source']),
    });
  }
  return relations;
}

/**
 * A `{name: count}` breakdown, ordered by count descending.
 *
 * ORDERED HERE AND NOT IN THE COMPONENT. The provider answers a JSON object,
 * and object key order is an encoding detail no reader may depend on — a
 * breakdown rendered in that order is a list whose top row changes between two
 * identical graphs. Ties break by name so the order is total.
 */
export function readBreakdown(value: unknown): readonly InventoryCount[] {
  if (!isRecord(value)) return [];
  const counts: InventoryCount[] = [];
  for (const [name, raw] of Object.entries(value)) {
    const parsed = count(raw);
    if (name === '' || parsed === null) continue;
    counts.push({ name, count: parsed });
  }
  counts.sort((left, right) => right.count - left.count || left.name.localeCompare(right.name));
  return counts;
}

/** The graph's own numbers. */
export function readStats(document: Doc): InventoryStats {
  return {
    nodeCount: count(document?.['node_count']),
    edgeCount: count(document?.['edge_count']),
    byType: readBreakdown(document?.['entities_by_type'] ?? document?.['entity_types']),
    byLayer: readBreakdown(document?.['entities_by_layer'] ?? document?.['layers']),
    sourceToolkits: list(document?.['source_toolkits'])
      .map(text)
      .filter((name) => name !== ''),
  };
}

/** The local graph cache. */
export function readCacheStats(document: Doc): InventoryCacheStats {
  return {
    cachedGraphs: count(document?.['cached_graphs'] ?? document?.['total_graphs']),
    sizeBytes: count(document?.['cache_size_bytes'] ?? document?.['total_size_bytes']),
    hits: count(document?.['hits']),
    misses: count(document?.['misses']),
  };
}

/** One row of `get_sources_status`: what has been ingested, and how it went. */
export interface SourceStatusRow {
  readonly source: string;
  readonly status: string;
  readonly entityCount: number | null;
  readonly relationCount: number | null;
  readonly lastUpdated: string;
  readonly errorMessage: string;
}

/**
 * `get_sources_status`, keyed by the label the provider reports.
 *
 * THE LABEL IS NOT THE TOOLKIT ID. An ingestion reports `{type}:{id}` — the
 * expanded source object's own fields (`SourceLabelFor`,
 * internal/apps/inventory/run/fixture.go:149-172) — while the graph's
 * citations carry the SDK toolkit's short name. Both reach this reader, and
 * the caller matches on either, which is why the label is returned verbatim
 * rather than being split here into something that would look like an id.
 */
export function readSourceStatuses(document: Doc): readonly SourceStatusRow[] {
  const rows = rowsUnder(document, ['sources']);
  const statuses: SourceStatusRow[] = [];
  for (const row of rows) {
    if (!isRecord(row)) continue;
    const source = text(row['source'] ?? row['source_toolkit'] ?? row['toolkit_id']);
    if (source === '') continue;
    statuses.push({
      source,
      status: text(row['status']),
      entityCount: count(row['entity_count'] ?? row['entities_count']),
      relationCount: count(row['relation_count'] ?? row['relations_count']),
      lastUpdated: text(row['last_updated'] ?? row['updated_at']),
      errorMessage: text(row['error_message'] ?? row['error']),
    });
  }
  return statuses;
}

/**
 * `get_ingestion_status`.
 *
 * `running` is read from three spellings because the legacy handler reported
 * `has_active_ingestion` and the fixture reports `running`. Reading only one
 * makes the "an ingestion is already going" banner never appear, which is
 * exactly when a user starts a second one.
 */
export function readIngestionStatus(document: Doc): IngestionStatus {
  return {
    running: isRunning(document),
    lastStatus: text(first(document, ['last_status', 'status'])),
    entityCount: count(document?.['entity_count']),
    relationCount: count(document?.['relation_count']),
    source: text(first(document, ['source', 'current_source'])),
    message: text(first(document, ['progress_message', 'message'])),
  };
}

/** The first of `keys` the document carries with a usable value. */
function first(document: Doc, keys: readonly string[]): unknown {
  if (document === undefined) return undefined;
  for (const key of keys) {
    const value = document[key];
    if (value !== undefined && value !== null && value !== '') return value;
  }
  return undefined;
}

/** The three spellings of "an ingestion is under way", read as one fact. */
function isRunning(document: Doc): boolean {
  const value = first(document, ['running', 'has_active_ingestion', 'is_running']);
  return value === true || value === 'true';
}

/** The entity types a graph holds, with their counts, ordered by count. */
export function readEntityTypes(document: Doc): readonly InventoryCount[] {
  const counts = readBreakdown(document?.['counts']);
  if (counts.length > 0) return counts;
  // A provider that answers only the names still has something to show; the
  // count of a type nobody counted is 0, and the screen says so.
  return list(document?.['types'])
    .map(text)
    .filter((name) => name !== '')
    .map((name) => ({ name, count: 0 }));
}
