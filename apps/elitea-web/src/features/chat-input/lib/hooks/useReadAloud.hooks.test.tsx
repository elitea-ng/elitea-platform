import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { installWebStorageShim } from '../../../../test/webstorage';
import { server } from '../../../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { createTestSocketClient, type TestSocketClient } from '@/shared/api/socket/testing';

import { useReadAloud } from './useReadAloud.hooks';

installWebStorageShim();

const BASE = '/api/v2';

function createWrapper(): { wrapper: ({ children }: { children: ReactNode }) => ReactNode } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  }
  return { wrapper: Wrapper };
}

afterEach(() => {
  resetGeneratedClient();
});

describe('useReadAloud', () => {
  it('with a projectId + socket + a default tts model available, hasModelTTS is true and voices come from the server list', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/models/proj-1`, () =>
        HttpResponse.json({ items: [{ name: 'model-a', project_id: 'proj-1' }, { name: 'model-b', project_id: 'proj-1', default: true }], total: 2 }),
      ),
      http.get(`${BASE}/configurations/tts_voices/proj-1`, () => HttpResponse.json({ voices: [{ id: 'v1', name: 'Voice One' }] })),
    );
    const client: TestSocketClient = createTestSocketClient();
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useReadAloud({ projectId: 'proj-1', socket: client }), { wrapper });

    await waitFor(() => expect(result.current.voicePlayerProps.hasModelTTS).toBe(true));

    // The `default: true` row is preferred over list order.
    expect(result.current.voicePlayerProps.ttsModel?.name).toBe('model-b');
    await waitFor(() => expect(result.current.voicePlayerProps.voices).toEqual([{ id: 'v1', name: 'Voice One' }]));
  });

  it('without a socket, hasModelTTS is false even when a tts model exists — falls back to browserVoices', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/models/proj-1`, () => HttpResponse.json({ items: [{ name: 'model-a', project_id: 'proj-1' }], total: 1 })));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useReadAloud({ projectId: 'proj-1', socket: null }), { wrapper });

    await waitFor(() => expect(result.current.voicePlayerProps.ttsModel?.name).toBe('model-a'));
    expect(result.current.voicePlayerProps.hasModelTTS).toBe(false);
    // jsdom has no `speechSynthesis`, so the browser voice list is empty — not the server list.
    expect(result.current.voicePlayerProps.voices).toEqual([]);
  });

  it('without a projectId, the models query stays disabled and there is no tts model', () => {
    configureGeneratedClient({ baseUrl: BASE });
    let hit = false;
    server.use(
      http.get(`${BASE}/configurations/models/:projectId`, () => {
        hit = true;
        return HttpResponse.json({ items: [], total: 0 });
      }),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useReadAloud({ projectId: undefined, socket: null }), { wrapper });

    expect(result.current.voicePlayerProps.ttsModel).toBeNull();
    expect(hit).toBe(false);
  });

  it('onAutoSpeak converts markdown to speakable text, shows the player, and records the speaking message id + segments', () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/models/proj-1`, () => HttpResponse.json({ items: [], total: 0 })));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useReadAloud({ projectId: 'proj-1', socket: null }), { wrapper });

    act(() => result.current.onAutoSpeak('**Hello** world', 'msg-1'));

    expect(result.current.showPlayer).toBe(true);
    expect(result.current.speakingMessageId).toBe('msg-1');
    expect(result.current.speakingSegments).not.toBeNull();
  });

  /* issue 974: `onAutoSpeak` STARTS the voice. It used to arm the player and stop
   * there, so "Read out" — a control that sits on the answer and says it will
   * read it out — produced silence until a second, unrelated-looking control in
   * the composer was pressed, and the speaking-mode auto-read (which has no
   * control at all) never spoke. */
  it('issue 974: onAutoSpeak speaks the converted text, not only the player', () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/models/proj-1`, () => HttpResponse.json({ items: [], total: 0 })));
    const spoken: string[] = [];
    const synthesis = {
      speaking: false,
      paused: false,
      pending: false,
      getVoices: () => [],
      speak: (utterance: { text: string }) => {
        spoken.push(utterance.text);
      },
      cancel: () => {},
      pause: () => {},
      resume: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
    };
    const utteranceClass = class {
      text: string;
      constructor(text?: string) {
        this.text = text ?? '';
      }
    };
    Object.defineProperty(window, 'speechSynthesis', { configurable: true, value: synthesis });
    Object.defineProperty(window, 'SpeechSynthesisUtterance', { configurable: true, value: utteranceClass });

    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useReadAloud({ projectId: 'proj-1', socket: null }), { wrapper });

    act(() => result.current.onAutoSpeak('**Hello** world', 'msg-1'));

    expect(spoken, 'the browser engine must have been asked to speak').toHaveLength(1);
    // The SPEAKABLE text, markdown stripped — the same string the player was
    // armed with, so the two halves cannot drift.
    expect(spoken[0]).toContain('Hello');
    expect(spoken[0]).not.toContain('**');
    expect(result.current.showPlayer, 'the player still appears — stopping needs a control').toBe(true);
  });

  it('onAutoSpeak with empty/whitespace-only markdown does nothing', () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/models/proj-1`, () => HttpResponse.json({ items: [], total: 0 })));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useReadAloud({ projectId: 'proj-1', socket: null }), { wrapper });

    act(() => result.current.onAutoSpeak('', 'msg-1'));
    expect(result.current.showPlayer).toBe(false);
    expect(result.current.speakingMessageId).toBeNull();
  });
});
