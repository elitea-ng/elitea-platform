/** Jest stand-in for expo-sharing (no native share sheet under Jest); applied to every test. */
export const isAvailableAsync = jest.fn(async () => true);
export const shareAsync = jest.fn(async (_url: string, _options?: unknown) => {});
