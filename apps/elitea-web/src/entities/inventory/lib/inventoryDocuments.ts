/**
 * The document readers, published as ONE symbol.
 *
 * Not a style preference: the slice budget is twenty exported symbols
 * (spec §3.5, `scripts/check-budgets.mjs`), and the readers alone are eleven.
 * They are also one thing — "read what an Inventory tool answered" — used
 * together by every panel, so a caller that has one almost always wants
 * another. Publishing them as a namespace keeps the barrel a curated API
 * instead of spending half its budget on a single concern.
 *
 * Frozen, because a namespace a caller can reassign is a namespace that
 * behaves differently in one test than in the app.
 */
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
import { toolArtifacts, toolErrorText, toolResultDocument, toolResultText } from './toolResult';

/** Every reader that turns a provider answer into something renderable. */
export const inventoryDocuments = Object.freeze({
  /** Layer 1+2: the tool's own result text out of the SPI envelope. */
  text: toolResultText,
  /** The sentence to show for a refusal: the same envelope, peeled. */
  errorText: toolErrorText,
  /** Layer 3: the JSON document an `output_format: 'json'` read answered. */
  document: toolResultDocument,
  /** The artifacts a terminal body carried — the graph, the status, the checkpoint. */
  artifacts: toolArtifacts,
  entity: readEntity,
  entities: readEntities,
  neighbours: readNeighbours,
  crossRelations: readCrossRelations,
  breakdown: readBreakdown,
  stats: readStats,
  cacheStats: readCacheStats,
  sourceStatuses: readSourceStatuses,
  ingestionStatus: readIngestionStatus,
  entityTypes: readEntityTypes,
});
