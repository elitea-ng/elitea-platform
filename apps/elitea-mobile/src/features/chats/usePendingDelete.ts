import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AccessibilityInfo } from 'react-native';

/** How long a confirmed delete can be undone before it is sent (longer with a screen reader). */
export const UNDO_WINDOW_MS = 6_000;
export const UNDO_WINDOW_SCREEN_READER_MS = 12_000;

export interface PendingDelete<T, S> {
  /** The row waiting out its undo window (hidden from the list), or null. */
  pending: T | null;
  /** The scope (project) `pending` came from: it is hidden only there. */
  pendingScope: S | null;
  /**
   * Every row the list must keep hidden: the pending one plus those whose commit has not
   * settled yet (so a row never flashes back between the window ending and the list
   * showing the delete).
   */
  waiting: readonly { row: T; scope: S }[];
  /** Starts the window for `row` in the current scope; a delete already waiting is committed at once. */
  start: (row: T) => void;
  /** Keeps the row: nothing is sent. */
  undo: () => void;
}

/**
 * A confirmed delete waits `UNDO_WINDOW_MS` with an Undo before `commit` runs (issue #6: the
 * delete is undoable for a few seconds and then syncs). Leaving the screen, a second delete,
 * or a change of `scope` commits the waiting one: it was confirmed.
 *
 * `scope` is what the row's id is relative to (the project: conversation ids are per project,
 * so chat 42 of project B is not chat 42 of project A). It is captured when the delete
 * starts and `commit` gets that captured scope, never the one current when the timer fires.
 */
export function usePendingDelete<T, S>(
  scope: S,
  commit: (row: T, scope: S) => void | Promise<unknown>,
  sameScope: (a: S, b: S) => boolean = Object.is,
): PendingDelete<T, S> {
  const [pending, setPending] = useState<{ row: T; scope: S } | null>(null);
  const [settling, setSettling] = useState<readonly { row: T; scope: S }[]>([]);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  const current = useRef<{ row: T; scope: S } | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const commitRef = useRef(commit);
  const scopeRef = useRef(scope);
  useEffect(() => {
    commitRef.current = commit;
  });
  const screenReader = useRef(false);
  useEffect(() => {
    let live = true;
    void AccessibilityInfo.isScreenReaderEnabled?.()
      .then((on) => {
        if (live) screenReader.current = on === true;
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);

  const flush = useCallback(() => {
    if (timer.current !== null) clearTimeout(timer.current);
    timer.current = null;
    const item = current.current;
    current.current = null;
    if (item === null) return;
    const done = commitRef.current(item.row, item.scope);
    if (!(done instanceof Promise)) return;
    if (mounted.current) setSettling((prev) => [...prev, item]);
    const clear = () => {
      if (mounted.current) setSettling((prev) => prev.filter((other) => other !== item));
    };
    done.then(clear, clear);
  }, []);

  // A new scope (another project selected while a delete waits): send the waiting one to the
  // scope it came from now, so it never outlives the list it was made in.
  useEffect(() => {
    if (sameScope(scopeRef.current, scope)) return;
    scopeRef.current = scope;
    if (current.current !== null) {
      flush();
      setPending(null);
    }
  }, [scope, sameScope, flush]);

  const start = useCallback(
    (row: T) => {
      flush();
      const item = { row, scope: scopeRef.current };
      current.current = item;
      setPending(item);
      timer.current = setTimeout(
        () => {
          flush();
          setPending(null);
        },
        screenReader.current ? UNDO_WINDOW_SCREEN_READER_MS : UNDO_WINDOW_MS,
      );
    },
    [flush],
  );

  const undo = useCallback(() => {
    if (timer.current !== null) clearTimeout(timer.current);
    timer.current = null;
    current.current = null;
    setPending(null);
  }, []);

  // Leaving the screen sends a confirmed delete instead of dropping it.
  useEffect(() => flush, [flush]);
  const waiting = useMemo(() => (pending === null ? settling : [...settling, pending]), [pending, settling]);
  return { pending: pending?.row ?? null, pendingScope: pending?.scope ?? null, waiting, start, undo };
}
