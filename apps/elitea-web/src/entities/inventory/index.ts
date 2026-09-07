/**
 * `entities/inventory` — one Inventory toolkit and the knowledge graph it owns.
 *
 * It is an ENTITY and not a feature because four features consume it —
 * sources, browser, statistics and ask — and `no-sideways-features` forbids
 * one feature importing another. What is here is everything they share: how a
 * toolkit is read, how a tool is invoked, and how the answer is decoded.
 *
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 * Three of the nineteen are NAMESPACES rather than loose functions
 * (`inventoryDocuments`, `inventoryConfig`, `inventoryInvocations`), each with
 * its own file explaining why: the readers alone are thirteen symbols of one
 * concern, and spending two-thirds of a slice's budget on them would leave no
 * room for the API a caller actually starts from.
 *
 * WHAT IS DELIBERATELY NOT HERE:
 *
 *  - `buildInventoryRequest` — the envelope. It is reached through the three
 *    functions that send it, and a caller building its own would be free to
 *    omit `configuration.parameters`, which is how an invoke silently reads
 *    the default bucket instead of the toolkit's.
 *  - `readEntity` and its ten siblings individually — see `inventoryDocuments`.
 *
 * `runInventoryTool` IS here, next to the hook that wraps it, because one
 * caller is not a query: a maintenance tool is a WRITE run once on a click,
 * and expressing it as a react-query query would make it fire on mount and
 * again on every refocus — a graph re-indexed each time the tab regains
 * focus.
 */
export {
  useInventoryToolkit,
  useInventoryToolkits,
  useSourceToolkitNames,
} from './api/inventoryToolkitApi';
export { inventoryInvocations } from './api/inventoryInvocations';
export {
  INVENTORY_FAMILY,
  INVENTORY_SEARCH_FAMILY,
  runInventoryTool,
  type InventoryTarget,
} from './api/inventoryToolApi';
export { inventoryToolkitToolsKey, useInventoryTool } from './api/useInventoryTool';
export { inventoryDocuments } from './lib/inventoryDocuments';
export { inventoryConfig } from './lib/inventoryConfig';
export type {
  IngestionStatus,
  InventoryCount,
  InventoryEntity,
  InventoryNeighbour,
  InventorySettings,
  InventorySource,
  InventoryStats,
} from './model/types';
