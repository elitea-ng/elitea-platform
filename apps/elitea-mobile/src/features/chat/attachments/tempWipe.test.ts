import { suspendActiveWorkspace } from '@/security/endSession';
import { bindWorkspace } from '@/security/secureStorage';
import { __resetSessionForTests, setSession } from '@/state/session';
import { TEST_WORKSPACE } from '@/test/fakeSecureStore';

import '@/features/chat/attachments/tempWipe';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-file-system', () => {
  const deleted: string[] = [];
  return {
    __deleted: deleted,
    Paths: { cache: { uri: 'file:///Library/Caches/' } },
    File: jest.fn(),
    Directory: jest.fn().mockImplementation((_parent: unknown, name: string) => ({
      exists: true,
      delete: () => deleted.push(name),
    })),
  };
});
const fs = jest.requireMock<{ __deleted: string[] }>('expo-file-system');

beforeEach(() => {
  fs.__deleted.splice(0);
  __resetSessionForTests();
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

// WS-SEC-2: a switch deletes nothing stored, but the plaintext attachment
// files the workspace being left wrote into the caches directory must go.
it("deletes the attachment temp files when the active workspace is put aside", async () => {
  bindWorkspace(TEST_WORKSPACE);
  setSession({ hydrated: true, serverOrigin: 'https://a.example.com', workspaceId: TEST_WORKSPACE });
  await suspendActiveWorkspace();
  expect([...fs.__deleted].sort()).toEqual(
    ['DocumentPicker', 'DocumentScan', 'ImageManipulator', 'com.apple.DocumentCamera', 'ImagePicker', 'att-tmp', 'att-up', 'export-tmp'].sort(),
  );
});
