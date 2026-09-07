/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * `toolForFilter` is deliberately internal: it is the decision about WHICH of
 * four tools answers a filter, it is tested here, and a second caller making
 * that decision for itself is how a screen comes to read the graph with one
 * tool and describe it with another.
 */
export {
  EMPTY_GRAPH_FILTER,
  useGraphBrowser,
  type GraphBrowserView,
  type GraphFilter,
} from './model/useGraphBrowser';
export { useGraphFacets, type GraphFacets } from './model/useGraphFacets';
export { useEntityDetail, type EntityDetailView } from './model/useEntityDetail';
export { GraphFilters } from './ui/GraphFilters';
export { EntityList } from './ui/EntityList';
export { EntityDetail } from './ui/EntityDetail';
