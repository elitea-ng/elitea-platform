/**
 * messages.ts is hand-maintained (its generator was retired with the Go
 * prototype socket.io server it read, #126) — these tests are what hold the
 * discriminated union to its shape, and assert its runtime behaviour, in
 * particular the spec-mandated unknown_event fallback (§5.5: "unknown
 * discriminants route to a logged unknown_event branch rather than
 * crashing or silently dropping").
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { FRAME_TYPE } from '@elitea/client-core/stream';

import { SOCKET_MESSAGE_TYPES, parseSocketMessage, socketMessageSchema } from './messages';

describe('SOCKET_MESSAGE_TYPES', () => {
  it('is exactly @elitea/client-core FRAME_TYPE, in its declaration order', () => {
    expect(SOCKET_MESSAGE_TYPES).toEqual(Object.values(FRAME_TYPE));
    expect(new Set(SOCKET_MESSAGE_TYPES).size).toBe(SOCKET_MESSAGE_TYPES.length);
  });

  it('still includes the full verbatim baseline list from spec §5.5 (constants.js:157-193)', () => {
    const baseline = [
      'agent_start', 'agent_response', 'agent_exception', 'agent_tool_start', 'agent_tool_end',
      'agent_tool_error', 'agent_requires_confirmation', 'agent_hitl_interrupt', 'mcp_authorization_required',
      'agent_llm_start', 'agent_llm_chunk', 'agent_llm_end', 'agent_on_function_tool_node', 'agent_on_tool_node',
      'agent_on_transitional_edge', 'agent_on_conditional_edge', 'agent_on_decision_edge', 'references', 'chunk',
      'AIMessageChunk', 'chat_user_message', 'start_task', 'freeform', 'error', 'llm_error', 'pipeline_finish',
      'agent_thinking_step', 'agent_thinking_step_update', 'chat_predict_summary_started',
      'chat_predict_summary_finished', 'swarm_child_message', 'agent_swarm_agent_start',
      'agent_swarm_agent_response', 'agent_swarm_handoff',
    ];
    expect(SOCKET_MESSAGE_TYPES).toEqual(expect.arrayContaining(baseline));
  });

  it('has one schema variant per discriminant, no more', () => {
    expect(socketMessageSchema.options).toHaveLength(SOCKET_MESSAGE_TYPES.length);
  });
});

describe('socketMessageSchema — discriminated union', () => {
  it.each(SOCKET_MESSAGE_TYPES)('accepts a minimal valid message for discriminant %s', (type) => {
    const result = socketMessageSchema.safeParse({ type });
    expect(result.success).toBe(true);
    if (result.success) expect(result.data.type).toBe(type);
  });

  it('rejects a message with no `type` field', () => {
    expect(socketMessageSchema.safeParse({ content: 'hi' }).success).toBe(false);
  });

  it('rejects a message whose `type` is not one of the discriminants', () => {
    expect(socketMessageSchema.safeParse({ type: 'totally_made_up_event' }).success).toBe(false);
  });

  it('parses the real chat_user_message shape (Chat/hooks.js:466-489)', () => {
    const result = socketMessageSchema.safeParse({
      type: 'chat_user_message',
      author_participant_id: 'p1',
      uuid: 'm1',
      content: 'hello',
      sent_to_id: 'p2',
      created_at: '2026-07-26T00:00:00Z',
      message_items: [],
    });
    expect(result.success).toBe(true);
  });

  it('parses the real agent_hitl_interrupt shape with a deeply nested response_metadata (Chat/hooks.js:1036-1212)', () => {
    const result = socketMessageSchema.safeParse({
      type: 'agent_hitl_interrupt',
      response_metadata: {
        metadata: { parent_agent_name: 'sub', child_thread_id: 't1' },
        hitl_interrupts: [{ tool_call_id: 'c1', message: 'approve?', available_actions: ['approve', 'reject'] }],
      },
    });
    expect(result.success).toBe(true);
  });

  it('parses the real swarm_child_message shape (Chat/hooks.js:1306-1314)', () => {
    const result = socketMessageSchema.safeParse({
      type: 'swarm_child_message',
      message_id: 'child-1',
      parent_message_id: 'parent-1',
      agent_name: 'sub-agent',
      content: 'partial answer',
      created_at: '2026-07-26T00:00:00Z',
    });
    expect(result.success).toBe(true);
  });
});

describe('parseSocketMessage — the spec-mandated unknown_event fallback', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('returns ok:true with the parsed, typed message for a known discriminant', () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const result = parseSocketMessage({ type: 'agent_response', content: 'hi' });
    expect(result.ok).toBe(true);
    if (result.ok) expect(result.message.type).toBe('agent_response');
    expect(warnSpy).not.toHaveBeenCalled();
  });

  it('RED/GREEN proof: an unrecognised discriminant routes to the unknown_event branch, is logged, and never throws', () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    expect(() => parseSocketMessage({ type: 'a_brand_new_event_the_backend_just_shipped' })).not.toThrow();
    const result = parseSocketMessage({ type: 'a_brand_new_event_the_backend_just_shipped' });
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.reason).toBe('unknown_event');
      expect(result.rawType).toBe('a_brand_new_event_the_backend_just_shipped');
    }
    expect(warnSpy).toHaveBeenCalledWith('unknown message type', 'a_brand_new_event_the_backend_just_shipped');
  });

  it('never throws and still logs when the payload is not even an object', () => {
    expect(() => parseSocketMessage(null)).not.toThrow();
    expect(() => parseSocketMessage('a raw string')).not.toThrow();
    expect(() => parseSocketMessage(undefined)).not.toThrow();
    const result = parseSocketMessage(42);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.rawType).toBeUndefined();
  });

  it('never throws for a known type whose OTHER fields are malformed — still routes to unknown_event rather than crashing', () => {
    // response_metadata must be an object if present; a string here fails validation.
    const result = parseSocketMessage({ type: 'agent_start', response_metadata: 'not-an-object' });
    expect(result.ok).toBe(false);
  });
});
