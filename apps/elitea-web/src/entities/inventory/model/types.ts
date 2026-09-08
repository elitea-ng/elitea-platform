/**
 * What an Inventory toolkit and its knowledge graph are, in this app's terms.
 *
 * EVERY SHAPE HERE IS READ OUT OF A TOOL RESULT, not out of a REST body. The
 * facade serves three routes (`/inventory/slots`, `/inventory/tools/…/invoke`,
 * `/inventory/invocations/…`) and all three carry an opaque SPI envelope: the
 * answer to "what does this graph hold" arrives as a JSON document encoded in
 * ONE STRING FIELD of the terminal poll. So the interfaces below describe the
 * provider's DOCUMENTS, and every reader validates rather than casts — a graph
 * written by an older engine is a document with fields missing, not a transport
 * error, and a screen that casts renders `undefined` as data.
 *
 * The wire field names stay the provider's, snake_case included, wherever a
 * document is read; the camelCase names below are this app's. The mapping is
 * done once, in `lib/toolDocuments.ts`, because the descriptor
 * (services/elitea-subapp-host/internal/apps/inventory/descriptor.json) is the
 * contract both sides are tested against and a second spelling of it in a
 * component is a second place for it to drift.
 */

/** One row of the project's toolkit listing that is an Inventory toolkit. */
export interface InventoryToolkitSummary {
  readonly id: string;
  readonly name: string;
}

/**
 * An Inventory toolkit's saved configuration, as the descriptor declares it:
 * `bucket`, `llm_model`, `embedding_model`, `sources` and the hidden
 * `source_configs`. Everything is `unknown` because these values come back
 * from a `jsonb` column that no schema narrows on the way out.
 */
export interface InventorySettings {
  readonly [key: string]: unknown;
}

/**
 * One source toolkit this Inventory toolkit ingests from.
 *
 * IT IS TWO DOCUMENTS JOINED. The ids and the per-source overrides live in the
 * Inventory toolkit's own settings (`sources`, `source_configs`); the status,
 * the counts and the error live in `sources_status.json`, which only exists
 * after an ingestion has run. A source that has never been ingested is a row
 * with an empty `status` — not a missing row — because the user added it and
 * has to be able to see that it is waiting.
 */
export interface InventorySource {
  readonly toolkitId: string;
  readonly name: string;
  readonly type: string;
  readonly branch: string;
  readonly filePatterns: string;
  readonly excludePatterns: string;
  readonly preset: string;
  /** `pending` | `in_progress` | `completed` | `error`, or '' when unreported. */
  readonly status: string;
  readonly entityCount: number | null;
  readonly relationCount: number | null;
  readonly lastUpdated: string;
  readonly errorMessage: string;
}

/** Whether an ingestion is running for this toolkit right now. */
export interface IngestionStatus {
  readonly running: boolean;
  readonly lastStatus: string;
  readonly entityCount: number | null;
  readonly relationCount: number | null;
  readonly source: string;
  readonly message: string;
}

/** One node of the knowledge graph, as every retrieval tool returns it. */
export interface InventoryEntity {
  readonly id: string;
  readonly name: string;
  readonly type: string;
  readonly layer: string;
  readonly sourceToolkit: string;
  readonly filePath: string;
}

/** One edge seen from an entity: what it links to, under which relation. */
export interface InventoryNeighbour {
  readonly entityId: string;
  readonly relationType: string;
  /** `outgoing` | `incoming`, or '' when the provider does not say. */
  readonly direction: string;
}

/** One edge that crosses a source boundary — the only edge kind with its own tool. */
export interface InventoryCrossRelation {
  readonly source: string;
  readonly target: string;
  readonly relationType: string;
  readonly fromSource: string;
  readonly toSource: string;
}

/** The graph's counts, as `get_stats` reports them. */
export interface InventoryStats {
  readonly nodeCount: number | null;
  readonly edgeCount: number | null;
  /** Descending by count, so a screen never has to sort a record it cannot order. */
  readonly byType: readonly InventoryCount[];
  readonly byLayer: readonly InventoryCount[];
  readonly sourceToolkits: readonly string[];
}

/** One `{name, count}` pair out of a `get_stats` breakdown. */
export interface InventoryCount {
  readonly name: string;
  readonly count: number;
}

/** The local graph cache, as `get_cache_stats` reports it. */
export interface InventoryCacheStats {
  readonly cachedGraphs: number | null;
  readonly sizeBytes: number | null;
  readonly hits: number | null;
  readonly misses: number | null;
}
