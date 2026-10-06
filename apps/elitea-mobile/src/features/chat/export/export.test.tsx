import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';
import * as Sharing from 'expo-sharing';
import { Share } from 'react-native';

import { CAPTURE, conversationDetail13, exportBadFormat, exportHeaders, exportMarkdown } from '@/api/__fixtures__/chat13';
import { discoveryDocument11, discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import { ExportError, exportErrorText, exportFileName, fetchTranscriptMarkdown } from '@/api/queries/exportConversation';
import ThreadScreen from '@/app/(app)/c/[id]';
import { shareTranscript } from '@/features/chat/export/shareTranscript';
import { clearLiveTurns } from '@/features/chat/turnController';
import { STRICT_DEFAULTS, resetPolicy, setPolicy } from '@/security/policy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

jest.mock('expo-router', () => ({ useLocalSearchParams: () => ({ id: '3' }), Stack: { Screen: (p: { options?: { headerRight?: () => unknown } }) => p.options?.headerRight?.() ?? null }, router: { replace: jest.fn() } }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: () => '0f0e0d0c-0b0a-4908-8706-050403020100' }));
// A file system that records what is written and deleted.
const mockFs = { written: new Map<string, string>(), deleted: [] as string[] };
jest.mock('expo-file-system', () => {
  const join = (parts: unknown[]) => parts.map((p) => (typeof p === 'string' ? p : (p as { uri: string }).uri)).join('/');
  class Directory {
    uri: string;
    constructor(...parts: unknown[]) {
      this.uri = join(parts);
    }
    get exists() {
      return [...mockFs.written.keys()].some((k) => k.startsWith(this.uri));
    }
    create() {}
    delete() {
      mockFs.deleted.push(this.uri);
      for (const k of [...mockFs.written.keys()]) if (k.startsWith(this.uri)) mockFs.written.delete(k);
    }
  }
  class File {
    uri: string;
    constructor(...parts: unknown[]) {
      this.uri = join(parts);
    }
    write(text: string) {
      mockFs.written.set(this.uri, text);
    }
  }
  return { Directory, File, Paths: { cache: 'file:///cache' } };
});

const P = CAPTURE.projectId;
const ROUTE = `/elitea_core/conversation_export/prompt_lib/${P}/3`;
const md = (status = 200) => ({ status, raw: new TextEncoder().encode(exportMarkdown), headers: exportHeaders });

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  clearLiveTurns();
  resetPolicy();
  mockFs.written.clear();
  mockFs.deleted.length = 0;
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('exportConversation (captured)', () => {
  it('reads the Markdown document and names the file from Content-Disposition', async () => {
    const server = fakeServer();
    server.on('GET', ROUTE, md());
    await expect(fetchTranscriptMarkdown(P, '3', server.client)).resolves.toEqual({ markdown: exportMarkdown, fileName: 'Quick_wins_capture.md' });
    expect(server.requests[0].url.searchParams.get('format')).toBe('md');
  });

  it('a 400 is a conversation too long for one document (never a truncated export)', async () => {
    const server = fakeServer();
    server.on('GET', ROUTE, { status: 400, body: exportBadFormat }, { status: 403, body: {} }, { status: 404, body: {} });
    const tooLong = await fetchTranscriptMarkdown(P, '3', server.client).catch((e: unknown) => e);
    expect(tooLong).toMatchObject({ reason: 'too_long' });
    expect(exportErrorText(tooLong)).toMatch(/web app/);
    await expect(fetchTranscriptMarkdown(P, '3', server.client)).rejects.toMatchObject({ reason: 'not_allowed' });
    await expect(fetchTranscriptMarkdown(P, '3', server.client)).rejects.toMatchObject({ reason: 'not_available' });
  });

  it('file names are one safe segment', () => {
    expect(exportFileName('attachment; filename="../../etc/passwd"', '3')).toBe('etc_passwd.md');
    expect(exportFileName("attachment; filename*=UTF-8''R%C3%A9union%20notes.md", '3')).toBe('Réunion notes.md');
    expect(exportFileName(null, '42')).toBe('conversation-42.md');
    expect(exportFileName('attachment; filename=...', '42')).toBe('conversation-42.md');
  });
});

describe('shareTranscript', () => {
  it('writes a temp file, shares it, and deletes it even when the share sheet fails (iOS)', async () => {
    const order: string[] = [];
    const deps = {
      platform: 'ios',
      sweep: () => order.push('sweep'),
      fetchTranscript: async () => ({ markdown: exportMarkdown, fileName: 'Quick_wins_capture.md' }),
      writeTemp: (text: string, name: string) => {
        order.push(`write ${name} ${text.length}`);
        return { uri: `file:///cache/export-tmp/x/${name}`, delete: () => order.push('delete') };
      },
      share: async (uri: string) => {
        order.push(`share ${uri}`);
        throw new Error('sheet failed');
      },
    };
    await expect(shareTranscript(P, '3', deps)).rejects.toThrow('sheet failed');
    expect(order).toEqual([
      'sweep',
      `write Quick_wins_capture.md ${exportMarkdown.length}`,
      'share file:///cache/export-tmp/x/Quick_wins_capture.md',
      'delete',
    ]);
  });

  it('shares the FILE through expo-sharing as Markdown, never react-native Share (which drops `url` on Android)', async () => {
    const rnShare = jest.spyOn(Share, 'share');
    const shareAsync = jest.mocked(Sharing.shareAsync);
    shareAsync.mockClear();
    await shareTranscript(P, '3', {
      platform: 'android',
      sweep: () => {},
      fetchTranscript: async () => ({ markdown: exportMarkdown, fileName: 'Quick_wins_capture.md' }),
      writeTemp: (_t: string, name: string) => ({ uri: `file:///cache/export-tmp/x/${name}`, delete: () => {} }),
    });
    expect(rnShare).not.toHaveBeenCalled();
    expect(shareAsync).toHaveBeenCalledWith('file:///cache/export-tmp/x/Quick_wins_capture.md', {
      mimeType: 'text/markdown',
      UTI: 'net.daringfireball.markdown',
      dialogTitle: 'Quick_wins_capture.md',
    });
  });

  it('on Android the file outlives the share promise (the target reads it later) and the next export sweeps it', async () => {
    const order: string[] = [];
    const deps = {
      platform: 'android',
      sweep: () => order.push('sweep'),
      fetchTranscript: async () => ({ markdown: exportMarkdown, fileName: 'a.md' }),
      writeTemp: () => ({ uri: 'file:///cache/export-tmp/x/a.md', delete: () => order.push('delete') }),
      share: async () => {
        order.push('share');
      },
    };
    await shareTranscript(P, '3', deps);
    await expect(shareTranscript(P, '3', { ...deps, share: async () => Promise.reject(new Error('no target')) })).rejects.toThrow('no target');
    expect(order).toEqual(['sweep', 'share', 'sweep']);
  });

  it('share-out revoked while the transcript downloads: nothing is written or shared', async () => {
    const writeTemp = jest.fn();
    const share = jest.fn();
    const error = await shareTranscript(P, '3', {
      sweep: () => {},
      fetchTranscript: async () => {
        setPolicy({ ...STRICT_DEFAULTS, allow_share_out: false }, 'server');
        return { markdown: exportMarkdown, fileName: 'a.md' };
      },
      writeTemp,
      share,
    }).catch((e: unknown) => e);
    expect(error).toMatchObject({ reason: 'not_allowed' });
    expect(writeTemp).not.toHaveBeenCalled();
    expect(share).not.toHaveBeenCalled();
  });

  it.each([
    ['another workspace', { serverOrigin: 'https://other.example', workspaceId: 'w2' }],
    ['a workspace switch in progress', { switching: true }],
    ['signing out', { ending: true }],
  ])('the session moved to %s while the transcript downloads: nothing is written or shared', async (_label, patch) => {
    setSession({ workspaceId: 'w1' });
    const writeTemp = jest.fn();
    const share = jest.fn();
    const error = await shareTranscript(P, '3', {
      sweep: () => {},
      fetchTranscript: async () => {
        setSession(patch);
        return { markdown: exportMarkdown, fileName: 'a.md' };
      },
      writeTemp,
      share,
    }).catch((e: unknown) => e);
    expect(error).toMatchObject({ reason: 'cancelled' });
    expect(writeTemp).not.toHaveBeenCalled();
    expect(share).not.toHaveBeenCalled();
  });

  it('writes nothing when the server refuses', async () => {
    const writeTemp = jest.fn();
    await expect(
      shareTranscript(P, '3', { sweep: () => {}, fetchTranscript: async () => Promise.reject(new ExportError('too_long')), writeTemp }),
    ).rejects.toBeInstanceOf(ExportError);
    expect(writeTemp).not.toHaveBeenCalled();
  });
});

describe('Export in the thread header', () => {
  let queries: QueryClient | null = null;
  async function renderThread(ui: ReactElement) {
    queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
    return render(<QueryClientProvider client={queries}>{ui}</QueryClientProvider>);
  }
  afterEach(() => {
    queries?.clear();
    queries = null;
  });
  function install(contract: '1.1' | '1.3'): FakeServer {
    const server = fakeServer();
    globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
    server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, { status: 200, body: conversationDetail13 });
    server.on('GET', `/elitea_core/messages/prompt_lib/${P}/3`, { status: 200, body: { total: 0, rows: [] } });
    server.on('GET', `/elitea_core/message_traces/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
    server.on('GET', ROUTE, md());
    setWorkspaceInfo({
      origin: FAKE_ORIGIN,
      discovery: DiscoveryDocumentSchema.parse(contract === '1.3' ? discoveryDocument13() : discoveryDocument11()),
    });
    return server;
  }

  it('shares the transcript file and deletes it when the sheet closes', async () => {
    install('1.3');
    let sharedUri = '';
    let presentWhileSharing = false;
    jest.mocked(Sharing.shareAsync).mockImplementation(async (url: string) => {
      sharedUri = url;
      presentWhileSharing = mockFs.written.get(sharedUri) === exportMarkdown;
    });
    await renderThread(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Export conversation' }));
    await waitFor(() => expect(sharedUri).toMatch(/^file:\/\/\/cache\/export-tmp\/.+\/Quick_wins_capture\.md$/));
    expect(presentWhileSharing).toBe(true);
    await waitFor(() => expect(mockFs.written.size).toBe(0));
  });

  it('not offered when allow_share_out is off, or before client contract 1.3', async () => {
    install('1.3');
    setPolicy({ ...STRICT_DEFAULTS, allow_share_out: false }, 'server');
    const first = await renderThread(<ThreadScreen />);
    await screen.findByRole('button', { name: 'Answer actions' });
    expect(screen.queryByRole('button', { name: 'Export conversation' })).toBeNull();
    await first.unmount();
    resetPolicy();
    install('1.1');
    await renderThread(<ThreadScreen />);
    await screen.findByRole('button', { name: 'Answer actions' });
    expect(screen.queryByRole('button', { name: 'Export conversation' })).toBeNull();
  });
});
