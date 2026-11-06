/**
 * ADR-0050 decision 6: an `agent_state` frame on the chat stream (client
 * contract 1.38) moves the answer avatar and never reaches the transcript.
 */
import { act, render, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { server } from '@/test/setup';

import type { ChatMessage } from '../lib/convertMessagesToChatHistory';
import { applyAgentAvatarFrame, useAgentAvatarMotion, useAgentAvatarStore } from './agentAvatar.store';
import { useChatStreamTransport, type UseChatStreamTransportResult } from './useChatStreamTransport';

const BASE = '/api/v2';
const EVENTS_URL = '/api/v2/executions/7/exec-1/events';
const MESSAGE_ID = '63c6d989-2860-5d68-9e3e-3587c63350d3';
const globals = globalThis as unknown as Record<string, unknown>;
let registry: TestEventSourceRegistry;

function mockReducedMotion(reduce: boolean): void {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: reduce && query.includes('prefers-reduced-motion'),
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    onchange: null,
    dispatchEvent: () => false,
  }));
}

beforeEach(() => {
  useAgentAvatarStore.resetForTests();
  registry = installTestEventSource();
  globals['elitea_ui_config'] = { vite_server_url: BASE, vite_base_uri: '/', vite_public_project_id: 'public-1' };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: BASE });
  mockReducedMotion(false);
});

afterEach(() => {
  registry.restore();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('the chat stream', () => {
  it('routes agent_state into the avatar store, names the live answer, and settles on cancel', async () => {
    server.use(
      http.post(`${BASE}/elitea_core/messages/prompt_lib/7/uuid-1`, () =>
        HttpResponse.json({ task_id: 'exec-1', events_url: EVENTS_URL, response_message_id: MESSAGE_ID })),
    );
    const initial: readonly ChatMessage[] = [
      { id: MESSAGE_ID, role: 'assistant', name: 'Agent', content: '', createdAt: '2026-08-13T00:00:00.000Z', isStreaming: true, isLoading: true },
    ];
    const history = { current: initial };
    const api: { current: UseChatStreamTransportResult | undefined } = { current: undefined };
    function Probe(): null {
      api.current = useChatStreamTransport({
        setChatHistory: (updater) => { history.current = updater(history.current); },
        context: { name: 'Agent', now: () => '2026-08-13T00:00:00.000Z' },
      });
      return null;
    }
    render(<Probe />);
    await act(async () => {
      await api.current?.start({ projectId: 7, conversationUuid: 'uuid-1', contract: 'agent.execute.application.v1', body: { question: 'hi' } });
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));

    act(() => {
      registry.emit('execution.node_event', JSON.stringify({ type: 'agent_state', message_id: MESSAGE_ID, participant_id: 4, addressed_to: [], state: 'thinking', response_metadata: {} }));
    });

    expect(history.current).toEqual([{ ...initial[0], authorParticipantId: 4 }]);
    expect(history.current[0]?.content).toBe('');
    expect(useAgentAvatarStore.getState().byConversation['uuid-1']?.[4]?.state).toBe('thinking');

    // A cancel ends the run with no last state of its own; the avatar stops looping.
    act(() => { api.current?.close(); });
    expect(useAgentAvatarStore.getState().byConversation['uuid-1']?.[4]?.state).toBe('idle');
  });
});

describe('useAgentAvatarMotion', () => {
  it('moves only the active participant newest answer, then settles a success to the still', () => {
    vi.useFakeTimers({ now: 1_000 });
    const { result } = renderHook(() => useAgentAvatarMotion('uuid-1'));
    act(() => {
      applyAgentAvatarFrame('uuid-1', { type: 'agent_state', participant_id: 4, state: 'speaking' });
    });
    expect(result.current({ participantId: 4, newest: true })).toBe('speaking');
    expect(result.current({ participantId: 4, newest: false })).toBe('still');
    expect(result.current({ participantId: 9, newest: true })).toBe('still');

    act(() => {
      applyAgentAvatarFrame('uuid-1', { type: 'agent_state', participant_id: 4, state: 'success' });
    });
    expect(result.current({ participantId: 4, newest: true })).toBe('success');
    act(() => {
      vi.advanceTimersByTime(3_000);
    });
    expect(result.current({ participantId: 4, newest: true })).toBe('still');
  });

  it('shows the still under reduced motion', () => {
    mockReducedMotion(true);
    const { result } = renderHook(() => useAgentAvatarMotion('uuid-1'));
    act(() => {
      applyAgentAvatarFrame('uuid-1', { type: 'agent_state', participant_id: 4, state: 'speaking' });
    });
    expect(result.current({ participantId: 4, newest: true })).toBe('still');
  });

  it('keys states by conversation', () => {
    const { result } = renderHook(() => useAgentAvatarMotion('uuid-2'));
    act(() => {
      applyAgentAvatarFrame('uuid-1', { type: 'agent_state', participant_id: 4, state: 'speaking' });
    });
    expect(result.current({ participantId: 4, newest: true })).toBe('still');
  });
});
