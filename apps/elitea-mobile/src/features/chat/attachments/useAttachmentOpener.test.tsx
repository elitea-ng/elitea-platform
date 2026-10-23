import { act, renderHook } from '@testing-library/react-native';

import { DownloadError } from '@/api/attachments/download';
import { CONV } from '@/features/chat/__fixtures__/attachmentGroups';
import { useAttachmentOpener } from '@/features/chat/attachments/useAttachmentOpener';
import type { ChatAttachment } from '@/features/chat/messageModel';

jest.mock('expo-file-system', () => ({ File: jest.fn(), Directory: jest.fn(), Paths: { cache: 'file:///cache' } }));
jest.mock('expo-crypto', () => ({ randomUUID: () => 'r' }));

const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
const PDF = new TextEncoder().encode('%PDF-1.7');
const attachment = (storedName: string): ChatAttachment => ({
  key: storedName,
  name: storedName,
  filepath: `/chat-attachments/${CONV}/${storedName}`,
  storedName,
  conversationUuid: CONV,
  kind: storedName.endsWith('.png') ? 'image' : 'document',
  inlineDataUrl: null,
});

function harness(bytes: Uint8Array | Error) {
  const written: string[] = [];
  const deleted: string[] = [];
  const previewed: string[] = [];
  const alerts: string[] = [];
  const deps = {
    download: jest.fn(async () => {
      if (bytes instanceof Error) throw bytes;
      return { bytes, contentType: 'application/octet-stream' };
    }),
    write: jest.fn((_bytes: Uint8Array, name: string) => {
      const uri = `file:///cache/att-tmp/r/${name}`;
      written.push(uri);
      return { uri, delete: () => deleted.push(uri) };
    }),
    preview: jest.fn(async (uri: string) => void previewed.push(uri)),
    alert: (message: string) => alerts.push(message),
  };
  return { deps, written, deleted, previewed, alerts };
}

describe('useAttachmentOpener', () => {
  it('shows a downloaded image in the viewer and deletes its copy on close', async () => {
    const h = harness(PNG);
    const { result } = await renderHook(() => useAttachmentOpener(7, CONV, h.deps));
    await act(async () => result.current.open(attachment('shot.png')));
    expect(h.deps.download).toHaveBeenCalledWith(7, CONV, 'shot.png');
    expect(result.current.viewer).toEqual({ uri: 'file:///cache/att-tmp/r/shot.png', name: 'shot.png' });
    await act(async () => result.current.closeViewer());
    expect(result.current.viewer).toBeNull();
    expect(h.deleted).toEqual(['file:///cache/att-tmp/r/shot.png']);
  });

  it('hands a document to the system preview, and deletes it at the next open and when the thread goes', async () => {
    const h = harness(PDF);
    const { result, unmount } = await renderHook(() => useAttachmentOpener(7, CONV, h.deps));
    await act(async () => result.current.open(attachment('a.pdf')));
    expect(h.previewed).toEqual(['file:///cache/att-tmp/r/a.pdf']);
    expect(h.deleted).toEqual([]);
    await act(async () => result.current.open(attachment('b.pdf')));
    expect(h.deleted).toEqual(['file:///cache/att-tmp/r/a.pdf']);
    await unmount();
    expect(h.deleted).toEqual(['file:///cache/att-tmp/r/a.pdf', 'file:///cache/att-tmp/r/b.pdf']);
  });

  it('says why a file could not be opened, and keeps nothing', async () => {
    const h = harness(new DownloadError('not_allowed'));
    const { result } = await renderHook(() => useAttachmentOpener(7, CONV, h.deps));
    await act(async () => result.current.open(attachment('a.pdf')));
    expect(h.alerts).toEqual(["You don't have access to download this file."]);
    expect(h.written).toEqual([]);
    expect(result.current.opening).toBeNull();
  });

  function fakeAppState() {
    const listeners = new Set<(state: 'active' | 'background') => void>();
    return {
      currentState: 'active' as const,
      addEventListener: (_type: 'change', listener: (state: 'active' | 'background') => void) => {
        listeners.add(listener);
        return { remove: () => void listeners.delete(listener) };
      },
      emit: (state: 'active' | 'background') => {
        for (const listener of [...listeners]) listener(state);
      },
    };
  }

  it('deletes a previewed document when the app leaves the foreground (iOS: Quick Look is in-process)', async () => {
    const h = harness(PDF);
    const appState = fakeAppState();
    const { result } = await renderHook(() => useAttachmentOpener(7, CONV, { ...h.deps, appState, platform: 'ios' }));
    await act(async () => result.current.open(attachment('secret.pdf')));
    expect(h.deleted).toEqual([]);
    await act(async () => appState.emit('background'));
    expect(h.deleted).toEqual(['file:///cache/att-tmp/r/secret.pdf']);
  });

  it('on Android keeps it while the external viewer reads it, and deletes it when the user comes back', async () => {
    const h = harness(PDF);
    const appState = fakeAppState();
    const { result } = await renderHook(() => useAttachmentOpener(7, CONV, { ...h.deps, appState, platform: 'android' }));
    await act(async () => result.current.open(attachment('secret.pdf')));
    await act(async () => appState.emit('background'));
    expect(h.deleted).toEqual([]);
    await act(async () => appState.emit('active'));
    expect(h.deleted).toEqual(['file:///cache/att-tmp/r/secret.pdf']);
  });

  it('closes the in-app image viewer and deletes its copy when the app leaves the foreground', async () => {
    const h = harness(PNG);
    const appState = fakeAppState();
    const { result } = await renderHook(() => useAttachmentOpener(7, CONV, { ...h.deps, appState, platform: 'android' }));
    await act(async () => result.current.open(attachment('shot.png')));
    await act(async () => appState.emit('background'));
    expect(result.current.viewer).toBeNull();
    expect(h.deleted).toEqual(['file:///cache/att-tmp/r/shot.png']);
  });
});
