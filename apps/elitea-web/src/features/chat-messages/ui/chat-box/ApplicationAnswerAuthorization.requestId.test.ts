import { describe, expect, it } from 'vitest';

import { authorizationRequestId } from './ApplicationAnswerAuthorization';

import type { SubAgentGroupable } from '../../lib/subAgentGrouping';

/** The card must name the request the handler resolves (`exactRequestId`, same fallback order). */
describe('authorizationRequestId', () => {
  const card = (toolMeta: Record<string, unknown>) => ({ id: 'card-id', toolMeta }) as unknown as SubAgentGroupable;

  it('falls back interrupt_id → tool_run_id → tool_call_id → id', () => {
    expect(authorizationRequestId(card({ interrupt_id: 'int-1', tool_run_id: 'run-1' }))).toBe('int-1');
    expect(authorizationRequestId(card({ tool_run_id: 'run-1', tool_call_id: 'call-1' }))).toBe('run-1');
    expect(authorizationRequestId(card({ tool_call_id: 'call-1' }))).toBe('call-1');
    expect(authorizationRequestId(card({}))).toBe('card-id');
  });
});
