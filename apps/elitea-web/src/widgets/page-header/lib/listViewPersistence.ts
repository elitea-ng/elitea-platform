/**
 * Remembers which of the two list renderings (`cards` / `table`) a reader
 * last chose, per list page.
 *
 * The URL is the live value — `shared/ui/EntityCardList` reads `?view=` and
 * `ListViewToggle` writes it — so this store only answers the question the
 * URL cannot: "what did this reader pick the LAST time they opened this
 * page?". A fresh visit carries no `view` key, and the toggle then restores
 * the remembered choice into the URL.
 *
 * One key per page (`list.view.agents`, `list.view.pipelines`) because the
 * baseline's own toggle is per page too: `?view=` is a route-local param
 * there, so switching the agents list to a table never changed the pipelines
 * list.
 *
 * `createStorage('local')` is called FRESH inside each function, never cached
 * at module scope — the same test-environment hazard
 * `widgets/sidebar/lib/collapsedPersistence.ts` documents in full applies
 * here unchanged.
 */
import { createStorage } from '@/shared/lib/storage';
import { ENTITY_LIST_VIEW, type EntityListView } from '@/shared/ui/EntityCardList';

const KEY_PREFIX = 'list.view.';

/** The remembered view for `pageKey`, or `undefined` when nothing valid is stored. */
export function readPersistedListView(pageKey: string): EntityListView | undefined {
  const raw = createStorage('local').get(KEY_PREFIX + pageKey);
  if (raw === ENTITY_LIST_VIEW.table) return ENTITY_LIST_VIEW.table;
  if (raw === ENTITY_LIST_VIEW.cards) return ENTITY_LIST_VIEW.cards;
  return undefined;
}

export function writePersistedListView(pageKey: string, view: EntityListView): void {
  createStorage('local').set(KEY_PREFIX + pageKey, view);
}
