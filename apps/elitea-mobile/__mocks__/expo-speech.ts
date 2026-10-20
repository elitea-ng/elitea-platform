/** Jest stand-in for expo-speech (no native synthesiser under Jest); applied to every test. */
export const speak = jest.fn();
export const stop = jest.fn(async () => {});
export const isSpeakingAsync = jest.fn(async () => false);
export const pause = jest.fn(async () => {});
export const resume = jest.fn(async () => {});
export const getAvailableVoicesAsync = jest.fn(async () => []);
