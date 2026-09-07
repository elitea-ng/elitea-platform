/**
 * Turning one Inventory tool's JSON document into rows this app can render.
 *
 * Two failures are guarded here and neither reports itself. The ROW KEY differs
 * per tool — `results`, `matches`, `entities` — so a reader that knows one of
 * them renders an empty list for the other two: a 200 with nothing on screen,
 * this repository's most-repeated defect. And a MISSING count is not zero: a
 * screen that prints 0 for a document it could not read reports an empty graph
 * for a full one.
 *
 * The breakdown ORDER is asserted for the same reason. Object key order is an
 * encoding detail; a list rendered in it has a top row that changes between two
 * identical graphs.
 */
import { describe, expect, it } from 'vitest';

import {
  readBreakdown,
  readCacheStats,
  readCrossRelations,
  readEntities,
  readEntity,
  readEntityTypes,
  readIngestionStatus,
  readNeighbours,
  readSourceStatuses,
  readStats,
} from './toolDocuments';

describe('readEntity', () => {
  it('reads every field the provider spells in snake_case', () => {
    expect(
      readEntity({
        id: 'code:checkout-service',
        name: 'CheckoutService',
        type: 'class',
        layer: 'application',
        source_toolkit: 'code',
        file_path: 'src/checkout/service.py',
      }),
    ).toEqual({
      id: 'code:checkout-service',
      name: 'CheckoutService',
      type: 'class',
      layer: 'application',
      sourceToolkit: 'code',
      filePath: 'src/checkout/service.py',
    });
  });

  it('reads through the legacy `entity` nesting of a scored search hit', () => {
    expect(readEntity({ score: 0.8, entity: { id: 'a', name: 'A' } })?.name).toBe('A');
  });

  it('keeps a row that names only one of id and name, so it stays clickable', () => {
    // The retrieval tools resolve either. Dropping such a row makes an entity
    // the user can see inert; rendering '' as its id makes it unopenable.
    expect(readEntity({ name: 'CheckoutService' })).toMatchObject({
      id: 'CheckoutService',
      name: 'CheckoutService',
    });
    expect(readEntity({ id: 'code:x' })).toMatchObject({ id: 'code:x', name: 'code:x' });
  });

  it('renders a missing field as empty, never as the string "undefined"', () => {
    expect(readEntity({ id: 'a' })).toMatchObject({ type: '', layer: '', sourceToolkit: '', filePath: '' });
  });

  it('reads a numeric id as text, because ids are compared as strings', () => {
    expect(readEntity({ id: 9010, name: false })).toMatchObject({ id: '9010', name: 'false' });
  });

  it('drops a row that names nothing and a row that is not an object', () => {
    expect(readEntity({ type: 'class' })).toBeUndefined();
    expect(readEntity('code:x')).toBeUndefined();
    expect(readEntity(null)).toBeUndefined();
    expect(readEntity(['code:x'])).toBeUndefined();
  });
});

describe('readEntities', () => {
  it('reads all three spellings of the row list', () => {
    const row = { id: 'a', name: 'A' };
    expect(readEntities({ entities: [row] })).toHaveLength(1);
    expect(readEntities({ results: [row], total: 1 })).toHaveLength(1);
    expect(readEntities({ matches: [row] })).toHaveLength(1);
  });

  it('prefers `entities` when a document carries more than one key', () => {
    expect(readEntities({ entities: [{ id: 'a' }], results: [{ id: 'b' }, { id: 'c' }] })).toEqual([
      { id: 'a', name: 'a', type: '', layer: '', sourceToolkit: '', filePath: '' },
    ]);
  });

  it('skips the rows it cannot read and keeps the rest', () => {
    expect(readEntities({ results: [null, { id: 'a' }, { type: 'class' }] })).toHaveLength(1);
  });

  it('reads an absent document and a key that is not a list as no rows', () => {
    expect(readEntities(undefined)).toEqual([]);
    expect(readEntities({})).toEqual([]);
    expect(readEntities({ results: 'none' })).toEqual([]);
  });
});

describe('readNeighbours', () => {
  it('reads the `related` list `get_entity_neighbors` answers', () => {
    expect(
      readNeighbours({
        entity_id: 'x',
        related: [{ entity_id: 'code:place-order', relation_type: 'defines', direction: 'outgoing' }],
        total: 1,
      }),
    ).toEqual([{ entityId: 'code:place-order', relationType: 'defines', direction: 'outgoing' }]);
  });

  it('reads the other spellings both sides of the port produced', () => {
    expect(readNeighbours({ neighbors: [{ id: 'a', type: 'calls' }] })).toEqual([
      { entityId: 'a', relationType: 'calls', direction: '' },
    ]);
    expect(readNeighbours({ neighbours: [{ target: 'b' }] })).toEqual([
      { entityId: 'b', relationType: '', direction: '' },
    ]);
  });

  it('drops an edge with no other end, which nothing can be opened from', () => {
    expect(readNeighbours({ related: [{ relation_type: 'calls' }, 'a string'] })).toEqual([]);
    expect(readNeighbours(undefined)).toEqual([]);
  });
});

describe('readCrossRelations', () => {
  it('reads an edge that joins two sources', () => {
    expect(
      readCrossRelations({
        relations: [
          {
            source: 'code:a',
            target: 'docs:b',
            relation_type: 'documents',
            from_source: 'code',
            to_source: 'docs',
          },
        ],
      }),
    ).toEqual([
      {
        source: 'code:a',
        target: 'docs:b',
        relationType: 'documents',
        fromSource: 'code',
        toSource: 'docs',
      },
    ]);
  });

  it('reads the `edges` spelling and the `type` spelling', () => {
    expect(readCrossRelations({ edges: [{ source: 'a', target: 'b', type: 'calls' }] })).toEqual([
      { source: 'a', target: 'b', relationType: 'calls', fromSource: '', toSource: '' },
    ]);
  });

  it('drops a half-edge, which cannot be drawn', () => {
    expect(readCrossRelations({ relations: [{ source: 'a' }, { target: 'b' }, 7] })).toEqual([]);
    expect(readCrossRelations(undefined)).toEqual([]);
  });
});

describe('readBreakdown', () => {
  it('orders by count descending and breaks ties by name', () => {
    // The provider answers a JSON object. Rendering it in key order gives a
    // list whose top row differs between two identical graphs.
    expect(readBreakdown({ function: 1, class: 3, document: 3 })).toEqual([
      { name: 'class', count: 3 },
      { name: 'document', count: 3 },
      { name: 'function', count: 1 },
    ]);
  });

  it('reads a numeric string, which is how some engines report a count', () => {
    expect(readBreakdown({ class: '4' })).toEqual([{ name: 'class', count: 4 }]);
  });

  it('drops an entry whose count is not a number', () => {
    expect(readBreakdown({ class: 'many', layer: null, ok: 2 })).toEqual([{ name: 'ok', count: 2 }]);
  });

  it('reads a value that is not an object as no breakdown', () => {
    expect(readBreakdown(undefined)).toEqual([]);
    expect(readBreakdown(['class'])).toEqual([]);
  });
});

describe('readStats', () => {
  it('reads what get_stats answers', () => {
    expect(
      readStats({
        node_count: 6,
        edge_count: 5,
        entities_by_type: { class: 3, function: 1, document: 2 },
        entities_by_layer: { application: 4, documentation: 2 },
        source_toolkits: ['code', 'docs'],
      }),
    ).toEqual({
      nodeCount: 6,
      edgeCount: 5,
      byType: [
        { name: 'class', count: 3 },
        { name: 'document', count: 2 },
        { name: 'function', count: 1 },
      ],
      byLayer: [
        { name: 'application', count: 4 },
        { name: 'documentation', count: 2 },
      ],
      sourceToolkits: ['code', 'docs'],
    });
  });

  it('reads the older `entity_types` and `layers` spellings', () => {
    const stats = readStats({ entity_types: { class: 1 }, layers: { app: 2 } });
    expect(stats.byType).toEqual([{ name: 'class', count: 1 }]);
    expect(stats.byLayer).toEqual([{ name: 'app', count: 2 }]);
  });

  it('reports an unreported count as null, which is not the same fact as zero', () => {
    // A screen that printed 0 here would report an empty graph for a document
    // it could not read.
    expect(readStats(undefined)).toEqual({
      nodeCount: null,
      edgeCount: null,
      byType: [],
      byLayer: [],
      sourceToolkits: [],
    });
  });

  it('drops a source toolkit with no name from the chip row', () => {
    expect(readStats({ source_toolkits: ['code', '', null] }).sourceToolkits).toEqual(['code']);
  });
});

describe('readCacheStats', () => {
  it('reads both spellings the cache reports under', () => {
    expect(readCacheStats({ cached_graphs: 2, cache_size_bytes: 4096, hits: 9, misses: 1 })).toEqual({
      cachedGraphs: 2,
      sizeBytes: 4096,
      hits: 9,
      misses: 1,
    });
    expect(readCacheStats({ total_graphs: 3, total_size_bytes: 10 })).toMatchObject({
      cachedGraphs: 3,
      sizeBytes: 10,
    });
  });

  it('reports an absent cache document as unknown, not as an empty cache', () => {
    expect(readCacheStats(undefined)).toEqual({
      cachedGraphs: null,
      sizeBytes: null,
      hits: null,
      misses: null,
    });
  });
});

describe('readSourceStatuses', () => {
  it('reads get_sources_status verbatim, label included', () => {
    // The label is NOT the toolkit id. Splitting it here would produce
    // something that looks like an id and joins to the wrong source.
    expect(
      readSourceStatuses({
        sources: [
          {
            source: 'github:9010',
            status: 'completed',
            entity_count: 4,
            relation_count: 3,
            last_updated: '2026-09-01T10:00:00Z',
            error_message: '',
          },
        ],
      }),
    ).toEqual([
      {
        source: 'github:9010',
        status: 'completed',
        entityCount: 4,
        relationCount: 3,
        lastUpdated: '2026-09-01T10:00:00Z',
        errorMessage: '',
      },
    ]);
  });

  it('reads the alternate field spellings an older ingestion wrote', () => {
    expect(
      readSourceStatuses({
        sources: [
          {
            source_toolkit: 'code',
            status: 'error',
            entities_count: 1,
            relations_count: 0,
            updated_at: 'yesterday',
            error: 'branch not found',
          },
          { toolkit_id: '9010', status: 'pending' },
        ],
      }),
    ).toEqual([
      {
        source: 'code',
        status: 'error',
        entityCount: 1,
        relationCount: 0,
        lastUpdated: 'yesterday',
        errorMessage: 'branch not found',
      },
      {
        source: '9010',
        status: 'pending',
        entityCount: null,
        relationCount: null,
        lastUpdated: '',
        errorMessage: '',
      },
    ]);
  });

  it('drops a status with no label, which nothing can be joined to', () => {
    expect(readSourceStatuses({ sources: [{ status: 'completed' }, 'code'] })).toEqual([]);
    expect(readSourceStatuses(undefined)).toEqual([]);
  });
});

describe('readIngestionStatus', () => {
  it('reads `running` from all three spellings in circulation', () => {
    // Reading only one makes the "an ingestion is already going" banner never
    // appear, which is exactly when a user starts a second one.
    expect(readIngestionStatus({ running: true }).running).toBe(true);
    expect(readIngestionStatus({ has_active_ingestion: true }).running).toBe(true);
    expect(readIngestionStatus({ is_running: 'true' }).running).toBe(true);
    expect(readIngestionStatus({ running: false, has_active_ingestion: true }).running).toBe(false);
  });

  it('reads the rest of the status document', () => {
    expect(
      readIngestionStatus({
        running: true,
        last_status: 'in_progress',
        entity_count: 4,
        relation_count: 3,
        current_source: 'github:9010',
        progress_message: 'Extracting entities',
      }),
    ).toEqual({
      running: true,
      lastStatus: 'in_progress',
      entityCount: 4,
      relationCount: 3,
      source: 'github:9010',
      message: 'Extracting entities',
    });
  });

  it('reads an absent document as "nothing is running"', () => {
    expect(readIngestionStatus(undefined)).toEqual({
      running: false,
      lastStatus: '',
      entityCount: null,
      relationCount: null,
      source: '',
      message: '',
    });
  });

  it('reads the fallback spellings of status, source and message', () => {
    expect(readIngestionStatus({ status: 'completed', source: 'code', message: 'Done' })).toMatchObject({
      lastStatus: 'completed',
      source: 'code',
      message: 'Done',
    });
  });
});

describe('readEntityTypes', () => {
  it('prefers the counted breakdown', () => {
    expect(readEntityTypes({ counts: { class: 3, function: 1 } })).toEqual([
      { name: 'class', count: 3 },
      { name: 'function', count: 1 },
    ]);
  });

  it('still shows the names when the provider counted nothing', () => {
    expect(readEntityTypes({ types: ['class', '', 'function'] })).toEqual([
      { name: 'class', count: 0 },
      { name: 'function', count: 0 },
    ]);
  });

  it('reads an absent document as no types', () => {
    expect(readEntityTypes(undefined)).toEqual([]);
  });
});
