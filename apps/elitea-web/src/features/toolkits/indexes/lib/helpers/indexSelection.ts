import type { IndexRow } from '../../model/indexesStore';
import { IndexStatuses, NEW_INDEX_ID } from '../constants/indexDetails.constants';

/**
 * Which index the Indexes tab should have selected after the list changed.
 *
 * The selection follows the index the user created or acted on, never the
 * order of the list (the server orders newest-created first, but nothing here
 * depends on that):
 *
 * - nothing selected (first load, or after a delete): the first finished row,
 *   the tab's long-standing default;
 * - the selected row is still listed: always adopt the listed (fresh) object,
 *   so state and history updates (in_progress to completed, a re-run) reach
 *   the detail panel; the same object means nothing to do;
 * - the selection is the create stub and its run has started (the stub holds a
 *   task_id or is in_progress): the row the server lists for it (by the
 *   created index id when the stub has one, else by name) in any terminal
 *   state, failed included, is the index the user just created, select it. A
 *   stub whose create never started a run (rejected duplicate name) never
 *   matches, and neither does a finished row that merely shares the name of a
 *   fresh stub;
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
  if (listed) return listed === current ? null : listed;

  if (current.id === NEW_INDEX_ID) {
    const meta = current.metadata;
    const runStarted = typeof meta['task_id'] === 'string' || meta['state'] === IndexStatuses.progress;
    if (!runStarted) return null;
    const createdId = meta['index_id'];
    const name = meta['collection'];
    const created = list.find((row) =>
      typeof createdId === 'string' && createdId !== ''
        ? row.id === createdId
        : typeof name === 'string' && row.metadata['collection'] === name && isTerminal(row),
    );
    return created && isTerminal(created) ? created : null;
  }

  return firstFinished;
}

const TERMINAL_STATES: readonly string[] = [IndexStatuses.success, IndexStatuses.fail, IndexStatuses.cancelled, IndexStatuses.partlyOk];

function isTerminal(row: IndexRow): boolean {
  const state = row.metadata['state'];
  return row.metadata['indexed'] !== undefined || (typeof state === 'string' && TERMINAL_STATES.includes(state));
}
