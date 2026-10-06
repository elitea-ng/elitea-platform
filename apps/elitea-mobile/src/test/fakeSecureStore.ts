/**
 * In-memory stand-in for `expo-secure-store` used by jest tests:
 *   jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
 */
export interface FakeSecureStore {
  values: Map<string, string>;
  WHEN_UNLOCKED_THIS_DEVICE_ONLY: number;
  WHEN_UNLOCKED: number;
  getItemAsync: jest.Mock<Promise<string | null>, [string, unknown?]>;
  setItemAsync: jest.Mock<Promise<void>, [string, string, unknown?]>;
  deleteItemAsync: jest.Mock<Promise<void>, [string, unknown?]>;
}

export function fakeSecureStoreModule(): FakeSecureStore {
  const values = new Map<string, string>();
  return {
    values,
    WHEN_UNLOCKED: 5,
    WHEN_UNLOCKED_THIS_DEVICE_ONLY: 6,
    getItemAsync: jest.fn(async (key: string) => values.get(key) ?? null),
    setItemAsync: jest.fn(async (key: string, value: string) => {
      values.set(key, value);
    }),
    deleteItemAsync: jest.fn(async (key: string) => {
      values.delete(key);
    }),
  };
}

/** The workspace id tests bind (`bindWorkspace(TEST_WORKSPACE)`). */
export const TEST_WORKSPACE = 'aaaaaaaaaaaa';

/** The physical keychain item of a workspace-scoped key (mirrors `physicalKey`). */
export function wsKey(key: string, workspaceId: string = TEST_WORKSPACE): string {
  return `az.ws.${workspaceId}.${key.slice('az.'.length)}`;
}

/** Registers `ids` (first = active) the way the workspace registry stores them. */
export function registryValue(ids: string[], active: string | null = ids[0] ?? null): string {
  return JSON.stringify({ v: 1, active, ids });
}
