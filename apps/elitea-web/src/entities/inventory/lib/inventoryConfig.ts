/**
 * The toolkit-settings readers, published as ONE symbol, for the reason
 * `inventoryDocuments` gives: the slice budget is twenty exported symbols and
 * these four are one concern — what a toolkit is configured to do.
 */
import {
  DEFAULT_INVENTORY_BUCKET,
  inventoryBucket,
  inventoryLlmModel,
  inventorySourceConfig,
  inventorySourceIds,
} from './toolkitSettings';

/** Reading an Inventory toolkit's saved configuration. */
export const inventoryConfig = Object.freeze({
  /** The bucket the graph lives in, defaulted the way the provider defaults it. */
  bucket: inventoryBucket,
  /** The extraction model, or '' — a toolkit that reads and cannot ingest. */
  llmModel: inventoryLlmModel,
  /** The source toolkit ids, as text, in saved order, without duplicates. */
  sourceIds: inventorySourceIds,
  /** One source's stored overrides, keyed by the id AS TEXT. */
  sourceConfig: inventorySourceConfig,
  /** What the provider stores a graph under when a toolkit names no bucket. */
  defaultBucket: DEFAULT_INVENTORY_BUCKET,
});
