import { create, type StoreApi, type UseBoundStore } from 'zustand';

/**
 * Zustand port of `apps/elitea-ui/src/[fsd]/features/toolkits/indexes/
 * model/indexes.slice.js` (unit A4a) — the baseline's Redux `indexes` slice.
 * This app has no Redux (`@reduxjs/toolkit` is not a dependency); `zustand`
 * is the established substitute (see e.g.
 * `features/agents/model/applicationsStore.ts`,
 * `features/pipelines/model/pipelineEditorStore.ts`).
 *
 * **What ported, and what did not:**
 *
 * - `toolkitScheduler`, `selectedHistoryItem` — genuine cross-component
 *   CLIENT state with no other home, ported as-is. `toolkitScheduler` is
 *   populated by `../api/indexesApi.ts`'s `getIndexSchedule` query success
 *   (mirrors the baseline's `getIndexSchedule.matchFulfilled` matcher) and
 *   trimmed by `deleteIndexItem` mutation success (mirrors
 *   `deleteIndexItem.matchFulfilled`'s matcher) — both wired at the API-hook
 *   layer, not here, since a zustand store has no matcher/extraReducers
 *   concept; see `indexesApi.ts`'s own doc comment.
 *
 * - `indexesList.{data,isFetching,isLoading,hasData}` — NOT ported. The
 *   baseline's four `getIndexesList.match*` matchers exist purely to mirror
 *   RTK Query's cache into this slice so components could `useSelector` it
 *   instead of the query hook directly. TanStack Query removes the reason
 *   for that mirror entirely: `useIndexesListQuery` (`indexesApi.ts`) IS the
 *   cache, and every real consumer of the list already calls it directly —
 *   mirroring it into a second, hand-synchronised store would be duplicate,
 *   driftable state with no benefit (same reasoning as
 *   `applicationsStore.ts`'s own documented `currentApplication`/
 *   `toolkitSchemas` omission).
 *
 * - `addTempLocalIndex`/`updateIndexDepMeta` — ported, but as an OVERLAY
 *   (`tempIndexes` + `indexPatches`) a caller merges onto the live
 *   `useIndexesListQuery` result, rather than mutating a locally-cached
 *   copy of the list (there is no local copy to mutate any more — see
 *   above). `IndexesContainer.tsx`'s own `indexesWithStub`-equivalent merge
 *   logic is exactly this: read `tempIndexes`/`indexPatches` from this
 *   store, overlay them onto the query result.
 *
 * Lazy-singleton factory pattern (R-S2: no store may be created at module
 * scope in a file also imported by `app/`) — mirrors
 * `applicationsStore.ts`'s own documented convention.
 */

/** The subset of an index row's `metadata` `updateIndexDepMeta` ever patches (baseline: `state`/`task_id`/`conversation_id`). */
export interface IndexDepMetaPatch {
  readonly state?: string;
  readonly task_id?: string;
  readonly conversation_id?: string;
  /**
   * The index's stored configuration, patched in by a successful
   * "Save"/"Save & Reindex" (issue 940/A5).
   *
   * It is here because a REINDEX of an existing index deliberately runs
   * `index.metadata.index_configuration` — the SERVER's copy — and not the
   * form (`useToolkitChat.hooks.ts`'s `resolveRunInputVariables`), which is
   * the rule that makes "Reindex uses the last SAVED configuration" true.
   * Without this patch, "Save & Reindex" would store the new configuration
   * and then immediately reindex with the OLD one: the save invalidates the
   * list query, but the refetch lands after the run has already been
   * dispatched. Patching the overlay closes that window without weakening
   * the rule.
   */
  readonly index_configuration?: Readonly<Record<string, unknown>>;
}

/** Loosely typed to match the historically dynamic shape of an index row (`{id, metadata: {...}}`) this domain has always used. */
export interface IndexRow {
  readonly id: string;
  readonly metadata: Record<string, unknown>;
  readonly [key: string]: unknown;
}

export interface ScheduleEntry {
  readonly cron?: string;
  readonly enabled?: boolean;
  readonly credentials?: unknown;
  readonly [key: string]: unknown;
}

interface IndexesState {
  /** Locally-added rows not yet reflected in the server list (baseline: `addTempLocalIndex`). Newest first, matching the baseline's `[payload, ...data]` prepend. */
  readonly tempIndexes: readonly IndexRow[];
  /** Per-id `metadata` overrides layered onto server rows (baseline: `updateIndexDepMeta`). */
  readonly indexPatches: Readonly<Record<string, IndexDepMetaPatch>>;
  /** Per-index-name schedule config, keyed by `metadata.collection` (baseline: `toolkitScheduler`). */
  readonly toolkitScheduler: Readonly<Record<string, ScheduleEntry>>;
  readonly selectedHistoryItem: Record<string, unknown> | null;
  readonly addTempLocalIndex: (index: IndexRow) => void;
  readonly updateIndexDepMeta: (id: string, patch: IndexDepMetaPatch) => void;
  readonly setToolkitScheduler: (scheduler: Readonly<Record<string, ScheduleEntry>>) => void;
  readonly removeToolkitSchedule: (indexName: string) => void;
  readonly selectHistoryItem: (item: Record<string, unknown> | null) => void;
  readonly reset: () => void;
}

type IndexesStore = UseBoundStore<StoreApi<IndexesState>>;

function createIndexesStore(): IndexesStore {
  return create<IndexesState>((set) => ({
    tempIndexes: [],
    indexPatches: {},
    toolkitScheduler: {},
    selectedHistoryItem: null,
    addTempLocalIndex: (index) => set((state) => ({ tempIndexes: [index, ...state.tempIndexes] })),
    updateIndexDepMeta: (id, patch) =>
      set((state) => ({
        indexPatches: {
          ...state.indexPatches,
          [id]: {
            ...state.indexPatches[id],
            ...(patch.state !== undefined && { state: patch.state }),
            ...(patch.task_id !== undefined && { task_id: patch.task_id }),
            ...(patch.conversation_id !== undefined && { conversation_id: patch.conversation_id }),
            ...(patch.index_configuration !== undefined && { index_configuration: patch.index_configuration }),
          },
        },
      })),
    setToolkitScheduler: (scheduler) => set({ toolkitScheduler: scheduler }),
    removeToolkitSchedule: (indexName) =>
      set((state) => {
        if (!(indexName in state.toolkitScheduler)) return state;
        const next = { ...state.toolkitScheduler };
        delete next[indexName];
        return { toolkitScheduler: next };
      }),
    selectHistoryItem: (item) => set({ selectedHistoryItem: item }),
    reset: () => set({ tempIndexes: [], indexPatches: {}, toolkitScheduler: {}, selectedHistoryItem: null }),
  }));
}

let instance: IndexesStore | undefined;

function resolveStore(): IndexesStore {
  instance ??= createIndexesStore();
  return instance;
}

function useIndexesStoreHook<T>(selector: (state: IndexesState) => T): T {
  return resolveStore()(selector);
}

/** @public The lazily-constructed singleton, exposed with the same hook + getState/setState surface this codebase's other stores use. */
export const useIndexesStore = Object.assign(useIndexesStoreHook, {
  getState: (): IndexesState => resolveStore().getState(),
  setState: (partial: Partial<IndexesState>): void => resolveStore().setState(partial),
});

/** Merges the temp/patch overlay onto a server-fetched index list — the equivalent of reading the baseline's mutated `indexesList.data` directly. */
export function mergeIndexesOverlay(
  serverIndexes: readonly IndexRow[],
  tempIndexes: readonly IndexRow[],
  indexPatches: Readonly<Record<string, IndexDepMetaPatch>>,
): IndexRow[] {
  const patched = serverIndexes.map((row) => {
    const patch = indexPatches[row.id];
    if (!patch) return row;
    return { ...row, metadata: { ...row.metadata, ...patch } };
  });
  return [...tempIndexes, ...patched];
}
