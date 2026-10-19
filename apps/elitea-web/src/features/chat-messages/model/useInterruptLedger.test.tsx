/**
 * Live resolution on the project SSE plane (fan-out contract v1.1 §6, §8, §9):
 * another tab's decision drops the card here, a wake attaches this view to the
 * new execution, and a reload or a returning tab reconciles from the list.
 * msw answers the list route; the `shared/api/sse` double stands in for the
 * browser `EventSource` jsdom lacks.
 */
import { act, renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { fanoutFixture } from '../__tests__/fanoutFixtures';
import { applyChatStreamFrame } from '../lib/chatStreamReducer';
import type { ChatMessage } from '../lib/convertMessagesToChatHistory';
import type { NormalizedHitlInterrupt } from '../lib/hitlInterrupts';
import { LEDGER_RESOLVED_EVENT, LEDGER_WAKE_EVENT, useInterruptLedger } from './useInterruptLedger';

const globals = globalThis as unknown as Record<string, unknown>;
const RESPONSE = '10000000-0000-4000-8000-000000000043';
const CONVERSATION = 'c0000000-0000-4000-8000-000000000001';
const LIST = '*/elitea_core/task/prompt_lib/:projectId/:responseMessageId/interrupts';
const card = fanoutFixture<{ interrupt_id: string; interrupt_key: string }>('fanout-interrupt-card-v1');
const resolvedFixture = fanoutFixture('fanout-interrupt-resolved-v1');
const list = fanoutFixture('fanout-interrupt-list-v1');

let registry: TestEventSourceRegistry;

function withCard(extra: Partial<ChatMessage> = {}): readonly ChatMessage[] {
  const seed: readonly ChatMessage[] = [{ id: RESPONSE, role: 'assistant', name: 'Pipeline', content: '', createdAt: '2026-10-19T00:00:00Z', ...extra }];
  return applyChatStreamFrame(seed, { type: 'agent_interrupt_pending', message_id: RESPONSE, response_metadata: { pause_progress: true, interrupt_card_v1: card } });
}

function harness(initial: readonly ChatMessage[]) {
  let history = initial;
  const attachWake = vi.fn(() => true);
  const setChatHistory = (update: (previous: readonly ChatMessage[]) => readonly ChatMessage[]): void => { history = update(history); };
  const view = renderHook((messages: readonly ChatMessage[]) => useInterruptLedger({
    projectId: 7, conversationUuid: CONVERSATION, messages, setChatHistory, attachWake,
  }), { initialProps: history });
  return { attachWake, read: () => history, rerender: () => view.rerender(history), view };
}

function installList(): { readonly calls: string[] } {
  const calls: string[] = [];
  server.use(http.get(LIST, ({ params }) => {
    calls.push(`${String(params['projectId'])}/${String(params['responseMessageId'])}`);
    return HttpResponse.json(list);
  }));
  return { calls };
}

const cardsOf = (history: readonly ChatMessage[]) => (history[0]?.hitlInterrupts ?? []) as readonly NormalizedHitlInterrupt[];

beforeEach(() => {
  registry = installTestEventSource();
  globals['elitea_ui_config'] = { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public-1' };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  registry.restore();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
});

describe('useInterruptLedger', () => {
  it('listens on the project plane only while the view holds ledger work', async () => {
    installList();
    renderHook(() => useInterruptLedger({ projectId: 7, conversationUuid: CONVERSATION, messages: [], setChatHistory: () => undefined, attachWake: () => true }));
    expect(registry.getOpen()).toHaveLength(0);
    harness(withCard());
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    expect(registry.getSources()[0]?.url).toBe('/api/v2/events/prompt_lib/7');
  });

  it('drops the card another tab resolved, by conversation and response, and never reads a value', async () => {
    installList();
    const { read } = harness(withCard());
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    const leaked = fanoutFixture('fanout-interrupt-resolved-v1.invalid.value-leaked');
    act(() => {
      registry.emit(LEDGER_RESOLVED_EVENT, JSON.stringify({ ...resolvedFixture, conversation_id: 'another', root_response_id: RESPONSE }));
    });
    expect(cardsOf(read()).some((entry) => entry.interrupt_key === card.interrupt_key)).toBe(true);
    act(() => {
      registry.emit(LEDGER_RESOLVED_EVENT, JSON.stringify({ ...leaked, conversation_id: CONVERSATION, root_response_id: RESPONSE, unknown_field: { nested: true } }));
    });
    expect(cardsOf(read()).some((entry) => entry.interrupt_key === card.interrupt_key)).toBe(false);
    expect(JSON.stringify(read())).not.toContain('staging');
  });

  it('matches the conversation uuid case-insensitively and ignores a frame that does not parse', async () => {
    installList();
    const { read } = harness(withCard());
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    act(() => { registry.emit(LEDGER_RESOLVED_EVENT, 'not json'); });
    act(() => { registry.emit(LEDGER_RESOLVED_EVENT, JSON.stringify({ ...resolvedFixture, conversation_id: CONVERSATION.toUpperCase(), root_response_id: RESPONSE })); });
    expect(cardsOf(read()).some((entry) => entry.interrupt_key === card.interrupt_key)).toBe(false);
  });

  it('attaches to the woken execution of a parked response in this conversation only', async () => {
    installList();
    const { attachWake } = harness(withCard({ fanoutParked: true }));
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    const wake = { conversation_id: CONVERSATION, root_response_id: RESPONSE, execution_id: 'e'.repeat(32), generation: 2 };
    act(() => { registry.emit(LEDGER_WAKE_EVENT, JSON.stringify({ ...wake, root_response_id: '20000000-0000-4000-8000-000000000001' })); });
    act(() => { registry.emit(LEDGER_WAKE_EVENT, JSON.stringify({ ...wake, conversation_id: 'another' })); });
    expect(attachWake).not.toHaveBeenCalled();
    act(() => { registry.emit(LEDGER_WAKE_EVENT, JSON.stringify(wake)); });
    expect(attachWake).toHaveBeenCalledWith({ responseMessageId: RESPONSE, executionId: 'e'.repeat(32) });
  });

  it('reconciles a reloaded parked response from the list once, and again when the tab becomes visible', async () => {
    const { calls } = installList();
    const reloaded: readonly ChatMessage[] = [{ id: RESPONSE, role: 'assistant', name: 'Pipeline', content: '', createdAt: '2026-10-19T00:00:00Z', fanoutParked: true }];
    const { read } = harness(reloaded);
    await waitFor(() => expect(cardsOf(read())).toHaveLength(2));
    expect(calls).toEqual([`7/${RESPONSE}`]);
    expect(cardsOf(read()).map((entry) => [entry.interrupt_id, Boolean(entry.hidden)])).toEqual([['hitl:sensitive:0001', true], ['ask:7f3a9c01', false]]);
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });
    act(() => { document.dispatchEvent(new Event('visibilitychange')); });
    await waitFor(() => expect(calls).toHaveLength(2));
  });
});
