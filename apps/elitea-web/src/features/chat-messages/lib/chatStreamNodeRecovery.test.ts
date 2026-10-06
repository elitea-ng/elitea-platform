import { describe, expect, it } from 'vitest';
import receipt from '@/shared/lib/fixtures/node-recovery-required.json';
import type { MessageGroupWire } from '@/entities/message/lib/wire';
import { applyChatStreamFrame } from './chatStreamReducer';
import { isObserverTerminalFrame, isTurnTerminalFrame } from './chatStreamTurnEnd';
import { convertMessagesToChatHistory } from './convertMessagesToChatHistory';
import type { ChatMessage } from './convertMessagesToChatHistory.types';
import { currentNodeRecoveryBinding } from './nodeRecoveryBinding';

const response = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
const initial: ChatMessage = { id: response, role: 'assistant', name: 'Pipeline', content: '', createdAt: '',
  executionGeneration: '1', isStreaming: true, isLoading: true, isRegenerating: true };
const frame = { type: 'agent_node_recovery_required', message_id: response, execution_generation: '1',
  response_metadata: { node_recovery_required_v1: receipt } };
const row = { id: 1, uuid: response, role: 'assistant', content: '', is_streaming: true, created_at: '',
  meta: { execution_generation: '1', node_recovery_required_v1: receipt } } as MessageGroupWire;

describe('node recovery pause event and restore', () => {
  it('settles active loading, retains exact receipt, keeps Stop and new-turn blocking', () => {
    const paused = applyChatStreamFrame([initial], frame);
    expect(paused[0]).toMatchObject({ isStreaming: true, isLoading: false, isRegenerating: false });
    expect(currentNodeRecoveryBinding(paused)).toEqual({ responseMessageId: response, executionGeneration: '1', receipt });
    expect(isTurnTerminalFrame(frame)).toBe(false);
    expect(isObserverTerminalFrame(frame, '1')).toBe(false);
  });
  it('can bind an early pause with no previous frame', () => {
    expect(currentNodeRecoveryBinding(applyChatStreamFrame([], frame))?.receipt).toEqual(receipt);
  });
  it.each([
    { ...frame, execution_generation: '2' }, { ...frame, message_id: 'bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee' },
    { ...frame, response_metadata: { node_recovery_required_v1: { ...receipt, schema: 'unknown' } } },
    { ...frame, response_metadata: { ...frame.response_metadata, parent_agent_path: 'malformed' } },
    { ...frame, type: 'unknown_node_recovery' },
  ])('keeps stale, malformed, child and unknown frames inert', (badFrame) => {
    const history = [initial];
    const result = applyChatStreamFrame(history, badFrame);
    expect(currentNodeRecoveryBinding(result)).toBeUndefined();
    expect(result[0]).toEqual(initial);
  });
  it('does not clear a pause for a malformed final frame', () => {
    const paused = applyChatStreamFrame([initial], frame);
    expect(currentNodeRecoveryBinding(applyChatStreamFrame(paused, { type: 'full_message', message_id: response,
      execution_generation: '1' }))?.receipt).toEqual(receipt);
  });

  it('does not restore a pause onto a completed response', () => {
    const history = [{ ...initial, isStreaming: false, isLoading: false }];
    expect(applyChatStreamFrame(history, frame)).toBe(history);
  });
  it('rejects an older revision and clears the receipt after a real final result', () => {
    const paused = applyChatStreamFrame([initial], frame);
    expect(applyChatStreamFrame(paused, { ...frame, response_metadata: { node_recovery_required_v1: { ...receipt, journal_revision: 1 } } })).toBe(paused);
    const finished = applyChatStreamFrame(paused, { type: 'full_message', message_id: response, execution_generation: '1', content: 'done' });
    expect(finished[0]).toMatchObject({ content: 'done', isStreaming: false });
    expect(finished[0]?.nodeRecoveryRequired).toBeUndefined();
  });
  it('restores the active persisted pause after reload', () => {
    expect(currentNodeRecoveryBinding(convertMessagesToChatHistory([row]))?.receipt).toEqual(receipt);
  });
  it.each([
    { ...row, is_streaming: false },
    { ...row, meta: { ...row.meta, node_recovery_required_v1: { ...receipt, schema: 'unknown' } } },
    { ...row, meta: { ...row.meta, is_error: true, error: 'Failed' } },
  ])('does not restore terminal or malformed persisted metadata', (badRow) => {
    expect(currentNodeRecoveryBinding(convertMessagesToChatHistory([badRow]))).toBeUndefined();
  });
});
