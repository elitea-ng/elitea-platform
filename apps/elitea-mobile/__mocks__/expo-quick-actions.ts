// jest: expo-quick-actions has no native module under jest. The items last set are kept for assertions.
export const initial = undefined;
export const maxCount = 4;
export const setItems = jest.fn(async () => {});
export const isSupported = jest.fn(async () => true);
export const addListener = jest.fn(() => ({ remove: jest.fn() }));
