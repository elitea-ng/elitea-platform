// jest: expo-app-intents has no native module under jest. The catalog and the pending queue
// are kept in memory for assertions; `useAppIntents` delivers the queue once on mount.
type Invocation = { id: string; name: string; params: Record<string, unknown>; createdAt: number };

export const catalogs = new Map<string, unknown[]>();
export const pending: Invocation[] = [];

export const isAvailable = jest.fn(() => true);
export const setEntityCatalogAsync = jest.fn(async (kind: string, entities: unknown[]) => {
  catalogs.set(kind, entities);
});
export const getEntityCatalogAsync = jest.fn(async (kind: string) => catalogs.get(kind) ?? []);
export const getPendingInvocationsAsync = jest.fn(async () => [...pending]);
export const removePendingInvocationAsync = jest.fn(async (id: string) => {
  const i = pending.findIndex((p) => p.id === id);
  if (i >= 0) pending.splice(i, 1);
});
export const clearPendingInvocationsAsync = jest.fn(async () => {
  pending.splice(0);
});
export const addAppIntentListener = jest.fn(() => ({ remove: jest.fn() }));
export const refreshShortcutsAsync = jest.fn(async () => {});
export const reindexEntitiesAsync = jest.fn(async () => {});
export const donateIntentAsync = jest.fn(async () => null);
export const deleteDonationsAsync = jest.fn(async () => []);
export const appEntityIdentifier = jest.fn();
export const AppEntityView = () => null;

export function useAppIntents(handler: (pending: Invocation[], next: Invocation | null) => void | Promise<void>): void {
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  const { useEffect } = require('react') as typeof import('react');
  useEffect(() => {
    void handler([...pending], null);
    // Once per mount, like the native hook's initial snapshot.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
}
