import type { IndexRow } from '../../model/indexesStore';
import { NEW_INDEX_ID } from '../constants/indexDetails.constants';

/**
 * Which index the Indexes tab should have selected after the list changed.
 *
 * The selection follows the index the user created or acted on, never the
 * order of the list (the server orders newest-created first, but nothing here
 * depends on that):
 *
 * - nothing selected (first load, or after a delete): the first finished row,
 *   the tab's long-standing default;
 * - the selected row is still listed: keep it. A re-run's fresher copy
 *   replaces it once it has a history, as before;
 * - the selection is the create stub and the list now holds a finished row
 *   with the stub's name: that is the index the user just created, select it;
 * - the stub's index is not listed yet, or an unrelated row finished: keep the
 *   stub, so a refetch never throws the user out of the form they are using;
 * - the selected row vanished from the list (deleted elsewhere): fall back to
 *   the default.
 *
 * Returns `null` for "leave the selection as it is".
 */
export function pickIndexSelection(current: IndexRow | null, list: readonly IndexRow[]): IndexRow | null {
  const firstFinished = list.find((row) => row.metadata['indexed'] !== undefined) ?? null;
  if (current === null) return firstFinished;

  const listed = list.find((row) => row.id === current.id);
  if (listed) {
    const history = (current.metadata['history'] as readonly unknown[] | undefined)?.length ?? 0;
    return history >= 1 ? listed : null;
  }

  if (current.id === NEW_INDEX_ID) {
    const name = current.metadata['collection'];
    const created = typeof name === 'string' ? list.find((row) => row.metadata['collection'] === name && row.metadata['indexed'] !== undefined) : undefined;
    return created ?? null;
  }

  return firstFinished;
}
