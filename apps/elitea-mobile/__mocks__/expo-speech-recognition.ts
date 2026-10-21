/**
 * Jest stand-in for expo-speech-recognition (its native module does not exist
 * under Jest). Applied automatically to every test (a root `__mocks__` entry
 * for a node module). Tests drive it with `__emit` and `__reset`.
 */
type Listener = (event: unknown) => void;
const listeners = new Map<string, Set<Listener>>();

function emit(name: string, event: unknown): void {
  for (const listener of [...(listeners.get(name) ?? [])]) listener(event);
}

export const ExpoSpeechRecognitionModule = {
  isRecognitionAvailable: jest.fn(() => true),
  supportsOnDeviceRecognition: jest.fn(() => true),
  requestPermissionsAsync: jest.fn(async () => ({ granted: true, status: 'granted', canAskAgain: true, expires: 'never' })),
  start: jest.fn(),
  stop: jest.fn(() => emit('end', null)),
  abort: jest.fn(),
  addListener: (name: string, listener: Listener) => {
    const set = listeners.get(name) ?? new Set<Listener>();
    set.add(listener);
    listeners.set(name, set);
    return { remove: () => set.delete(listener) };
  },
  __emit: emit,
  __listenerCount: (name: string) => listeners.get(name)?.size ?? 0,
  __reset: () => {
    listeners.clear();
    ExpoSpeechRecognitionModule.isRecognitionAvailable.mockClear();
    ExpoSpeechRecognitionModule.supportsOnDeviceRecognition.mockClear();
    ExpoSpeechRecognitionModule.requestPermissionsAsync.mockClear();
    ExpoSpeechRecognitionModule.isRecognitionAvailable.mockImplementation(() => true);
    ExpoSpeechRecognitionModule.supportsOnDeviceRecognition.mockImplementation(() => true);
    ExpoSpeechRecognitionModule.requestPermissionsAsync.mockImplementation(async () => ({
      granted: true,
      status: 'granted',
      canAskAgain: true,
      expires: 'never',
    }));
    ExpoSpeechRecognitionModule.start.mockClear();
    ExpoSpeechRecognitionModule.stop.mockClear();
    ExpoSpeechRecognitionModule.abort.mockClear();
  },
};

export function useSpeechRecognitionEvent(): void {}
