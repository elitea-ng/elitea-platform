import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen } from '@testing-library/react-native';

import { CAPTURE, conversationDetail13 } from '@/api/__fixtures__/chat13';
import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import ThreadScreen from '@/app/(app)/c/[id]';
import { readAloud, useReading } from '@/features/speech/readAloud';
import { clearLiveTurns } from '@/features/chat/turnController';
import { lockController } from '@/security/lock/useLockController';
import { resetPolicy } from '@/security/policy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-router', () => ({ useLocalSearchParams: () => ({ id: '3' }), Stack: { Screen: () => null }, router: { replace: jest.fn() } }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
// A secure device whose unlock prompt never answers: the app stays locked.
jest.mock('@/security/lock/deviceAuth', () => ({
  deviceAuth: { security: async () => 'biometric', authenticate: () => new Promise(() => {}) },
}));

// The root __mocks__ stand-ins, the same instances the wrappers get (required, not
// imported: these modules are import-restricted to their wrappers).
const Speech = require('expo-speech') as { stop: jest.Mock };
const { ExpoSpeechRecognitionModule } = require('expo-speech-recognition') as { ExpoSpeechRecognitionModule: { abort: jest.Mock } };

const P = CAPTURE.projectId;
let queries: QueryClient | null = null;

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  clearLiveTurns();
  resetPolicy(); // strict defaults: require_device_lock
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, { status: 200, body: conversationDetail13 });
  server.on('GET', `/elitea_core/messages/prompt_lib/${P}/3`, { status: 200, body: { total: 0, rows: [] } });
  server.on('GET', `/elitea_core/message_traces/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13()) });
});
afterEach(() => {
  lockController.stop();
  queries?.clear();
  queries = null;
  jest.restoreAllMocks();
});

async function relock() {
  await act(async () => {
    void lockController.start('launch');
  });
  expect(lockController.getState().status).toBe('locked');
}

describe('the device lock covers sheets too (a Modal is its own native window, above LockGate)', () => {
  it('an open message actions sheet is hidden while the app is locked: no Copy, Share or Read aloud above the lock', async () => {
    queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
    await render(
      <QueryClientProvider client={queries}>
        <ThreadScreen />
      </QueryClientProvider>,
    );
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    expect(screen.getByRole('button', { name: 'Copy' })).toBeTruthy();

    await relock();
    expect(screen.queryByRole('button', { name: 'Copy' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Share…' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Read aloud' })).toBeNull();
  });

  it('locking stops reading aloud and dictation', async () => {
    const reading: (string | null)[] = [];
    function Probe() {
      reading.push(useReading());
      return null;
    }
    await render(<Probe />);
    await act(async () => readAloud('m1', 'The whole answer'));
    expect(reading.at(-1)).toBe('m1');
    Speech.stop.mockClear();
    ExpoSpeechRecognitionModule.abort.mockClear();

    await relock();
    expect(Speech.stop).toHaveBeenCalled();
    expect(ExpoSpeechRecognitionModule.abort).toHaveBeenCalled();
    expect(reading.at(-1)).toBeNull();
  });
});
