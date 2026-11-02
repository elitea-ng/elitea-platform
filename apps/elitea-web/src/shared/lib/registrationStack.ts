/**
 * An ordered set of registrations a shell collects from the page on screen —
 * the page's breadcrumb (`shared/ui/page-chrome`), the tabs it offers the web
 * side panel (`widgets/app-shell`) — where each registration is withdrawn
 * when the component that made it unmounts.
 *
 * The entries live outside React state on purpose. Kept as
 * `setState(current => [...current, entry])` / `setState(current =>
 * current.filter(...))` pairs, a StrictMode remount (register, withdraw,
 * register again in one commit) left the first entry behind, so the
 * previous page's crumb and tabs stayed on screen after navigating away.
 * Here a withdrawal deletes its entry at once, and React reads a snapshot
 * through `useSyncExternalStore`.
 */
import { useState, useSyncExternalStore } from 'react';

export interface RegistrationStack<T> {
  /** Adds `value`; the returned function withdraws exactly that entry. */
  readonly register: (value: T) => () => void;
  readonly subscribe: (listener: () => void) => () => void;
  /** The live entries, oldest first; a new array after every change. */
  readonly snapshot: () => readonly T[];
}

function createRegistrationStack<T>(): RegistrationStack<T> {
  const entries = new Map<number, T>();
  const listeners = new Set<() => void>();
  let next = 0;
  let snapshot: readonly T[] = [];
  const changed = (): void => {
    snapshot = Array.from(entries.values());
    listeners.forEach((listener) => listener());
  };
  return {
    register: (value) => {
      next += 1;
      const key = next;
      entries.set(key, value);
      changed();
      return () => {
        if (entries.delete(key)) changed();
      };
    },
    subscribe: (listener) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    snapshot: () => snapshot,
  };
}

/** One stack for the life of the calling component, and its live entries. */
export function useRegistrationStack<T>(): readonly [RegistrationStack<T>, readonly T[]] {
  const [stack] = useState(() => createRegistrationStack<T>());
  const entries = useSyncExternalStore(stack.subscribe, stack.snapshot, stack.snapshot);
  return [stack, entries];
}
