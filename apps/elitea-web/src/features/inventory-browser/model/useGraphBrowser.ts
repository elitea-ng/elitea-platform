/**
 * Browsing the knowledge graph: the entity list, however it was narrowed.
 *
 * ONE HOOK FOR FOUR TOOLS, because they answer the same thing — a list of
 * entities — and the screen must not make the user learn which tool their
 * question maps to. The filter decides:
 *
 *   a query          → `search_graph`             (token matching)
 *   a type           → `list_entities_by_type`
 *   a layer          → `list_entities_by_layer`
 *   a source         → `list_entities_by_source`
 *   nothing at all   → `search_graph` with an empty query, which the provider
 *                      reads as "everything"
 *
 * ONLY ONE READ RUNS. Calling all four and merging would multiply the load on
 * the engine by four for a screen that shows one list, and the four disagree
 * about ordering, so the merged list would have no order at all.
 *
 * THE PROVIDER DOES THE FILTERING, not this hook. `search_graph` takes
 * `entity_type`, `layer` and `source_toolkit` as arguments; narrowing a fetched
 * page in the browser would filter the FIRST `top_k` rows and report the result
 * as the whole graph — which reads as "there are three classes" on a graph with
 * three hundred.
 */
import { useMemo } from 'react';

import {
  INVENTORY_FAMILY,
  inventoryDocuments,
  useInventoryTool,
  type InventoryEntity,
  type InventoryTarget,
} from '@/entities/inventory';

/** How the list is narrowed. Every field is optional; an empty filter lists everything. */
export interface GraphFilter {
  readonly query: string;
  readonly entityType: string;
  readonly layer: string;
  readonly sourceToolkit: string;
}

/** The empty filter: everything the graph holds, up to the page size. */
export const EMPTY_GRAPH_FILTER: GraphFilter = {
  query: '',
  entityType: '',
  layer: '',
  sourceToolkit: '',
};

/**
 * How many rows one read asks for.
 *
 * The provider's own default is small and the screen is a browser, not a
 * preview. It is a CAP and not a page: the read tools take `top_k`/`limit` and
 * no cursor, so there is no second page to ask for — a graph larger than this
 * is narrowed with the filters, which is what they are for.
 */
const GRAPH_PAGE_SIZE = 200;

/** Which tool answers this filter, and with which arguments. */
export function toolForFilter(filter: GraphFilter): { readonly tool: string; readonly params: Record<string, unknown> } {
  if (filter.query.trim() !== '') {
    return {
      tool: 'search_graph',
      params: {
        query: filter.query.trim(),
        top_k: GRAPH_PAGE_SIZE,
        // Sent only when set: the provider's merge takes a tool argument over
        // a configured one only when it is truthy, and an empty string here
        // reads as "no filter" on both sides — but sending it invites a reader
        // to believe it clears a configured value.
        ...(filter.entityType === '' ? {} : { entity_type: filter.entityType }),
        ...(filter.layer === '' ? {} : { layer: filter.layer }),
        ...(filter.sourceToolkit === '' ? {} : { source_toolkit: filter.sourceToolkit }),
      },
    };
  }
  if (filter.entityType !== '') {
    return {
      tool: 'list_entities_by_type',
      params: {
        entity_type: filter.entityType,
        limit: GRAPH_PAGE_SIZE,
        ...(filter.sourceToolkit === '' ? {} : { source_toolkit: filter.sourceToolkit }),
      },
    };
  }
  if (filter.layer !== '') {
    return {
      tool: 'list_entities_by_layer',
      params: {
        layer: filter.layer,
        limit: GRAPH_PAGE_SIZE,
        ...(filter.sourceToolkit === '' ? {} : { source_toolkit: filter.sourceToolkit }),
      },
    };
  }
  if (filter.sourceToolkit !== '') {
    return {
      tool: 'list_entities_by_source',
      params: { source_toolkit: filter.sourceToolkit, limit: GRAPH_PAGE_SIZE },
    };
  }
  // The empty query is the provider's own "everything": its matcher treats it
  // as matching every entity. A screen that refused to read without a filter
  // would open on nothing and give the user no way to see what is there.
  return { tool: 'search_graph', params: { query: '', top_k: GRAPH_PAGE_SIZE } };
}

/** What the entity list renders. */
export interface GraphBrowserView {
  readonly entities: readonly InventoryEntity[];
  readonly isPending: boolean;
  readonly error: string | null;
  /** Which tool answered — shown so a reader can tell a search from a listing. */
  readonly tool: string;
}

export function useGraphBrowser(
  target: InventoryTarget,
  filter: GraphFilter,
  options: { readonly enabled?: boolean } = {},
): GraphBrowserView {
  const { tool, params } = useMemo(() => toolForFilter(filter), [filter]);
  const query = useInventoryTool(target, INVENTORY_FAMILY, tool, params, {
    enabled: options.enabled ?? true,
  });
  const entities = useMemo(
    () => inventoryDocuments.entities(query.data?.document),
    [query.data],
  );
  return {
    entities,
    isPending: query.isPending && query.fetchStatus !== 'idle',
    error: query.error instanceof Error ? query.error.message : null,
    tool,
  };
}
