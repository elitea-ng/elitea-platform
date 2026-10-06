import { describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import fixture from '@/shared/lib/fixtures/code-debug-public-trace.json';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { ToolActionStatus } from '@/shared/lib/chat';
import { TraceStepDetailProvider } from '../model/traceStepDetail';
import { ToolModal } from '../ui/ToolModal';
import { applyChatStreamFrame, type ToolAction } from './chatStreamReducer';
import type { ChatStreamFrame } from './chatStreamFrame';
import type { ChatMessage } from './convertMessagesToChatHistory.types';

installCodeMirrorTestPolyfills();
const response = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
// Main's runtime generation in the proof differs from the admitted browser selector.
const browserGeneration = 'f21ee14e-19df-4bc4-99cc-6b26a736dd4c';
const initial: readonly ChatMessage[] = [{ id: response, role: 'assistant', name: 'Pipeline', content: '',
  createdAt: '2026-10-05T12:00:00Z', executionGeneration: browserGeneration, isStreaming: true, isLoading: true }];
function debugFrame(entry = fixture.first_attempt_history): ChatStreamFrame {
  return { type: 'agent_tool_end', message_id: response, execution_generation: browserGeneration, content: null,
    created_at: entry.timestamp_finish, response_metadata: entry };
}
function actions(history: readonly ChatMessage[]): readonly ToolAction[] {
  return (history[0]?.toolActions ?? []) as readonly ToolAction[];
}
function changedProof(change: Record<string, unknown>): ChatStreamFrame {
  const entry = fixture.first_attempt_history;
  const proof = Object.fromEntries(Object.entries({ ...entry.metadata.code_debug_v1, ...change }).filter(([, value]) => value !== undefined));
  return debugFrame({ ...entry, metadata: { ...entry.metadata, code_debug_v1: proof },
    tool_meta: { ...entry.tool_meta, metadata: { ...entry.tool_meta.metadata, code_debug_v1: proof } } } as typeof entry);
}

describe('completion-only Code debug trace', () => {
  it('retains distinct browser/runtime generations through Code phases, persistence and the final answer', () => {
    let history = initial;
    for (const phase of ['preparation', 'hydration', 'execution']) {
      const metadata = { tool_run_id: `phase-${phase}`, tool_name: `run / ${phase}`, metadata: { langgraph_node: 'run', node_type: 'code' } };
      history = applyChatStreamFrame(history, { type: 'agent_tool_start', message_id: response, execution_generation: browserGeneration, response_metadata: metadata });
      history = applyChatStreamFrame(history, { type: 'agent_tool_end', message_id: response, execution_generation: browserGeneration, response_metadata: { ...metadata, tool_output: 'Complete' } });
    }
    const phaseActions = actions(history);
    history = applyChatStreamFrame(history, debugFrame());
    history = applyChatStreamFrame(history, { type: 'partial_message', message_id: response, execution_generation: browserGeneration,
      response_metadata: { tool_calls: { [fixture.first_attempt_history.tool_run_id]: fixture.first_attempt_history } } });
    history = applyChatStreamFrame(history, { type: 'full_message', message_id: response, execution_generation: browserGeneration, content: 'debug_probe succeeded' });
    expect(actions(history)).toHaveLength(4);
    expect(actions(history).slice(0, 3)).toEqual(phaseActions);
    const exportAction = actions(history)[3]!;
    expect(exportAction).toMatchObject({ id: fixture.first_attempt_history.tool_run_id, name: 'run / debug export',
      status: ToolActionStatus.complete, toolInputs: {}, toolOutputs: null,
      toolMeta: { code_debug_v1: fixture.first_attempt_history.metadata.code_debug_v1 } });
    expect(history[0]).toMatchObject({ content: 'debug_probe succeeded', isStreaming: false });
    renderWithTheme(<TraceStepDetailProvider projectId="7"><ToolModal open onClose={vi.fn()} toolAction={{ toolMeta: exportAction.toolMeta ?? {} }} /></TraceStepDetailProvider>);
    expect(screen.getByText('Code debug · run · attempt 1')).toBeVisible();
    expect(screen.getByRole('button', { name: 'Download verified snapshot' })).toBeVisible();
  });

  it.each(['denied', 'unavailable'])('retains a completion-only %s warning', (status) => {
    const history = applyChatStreamFrame(initial, changedProof({ status, artifact: undefined }));
    expect(actions(history)).toHaveLength(1);
    renderWithTheme(<TraceStepDetailProvider projectId="7"><ToolModal open onClose={vi.fn()} toolAction={{ toolMeta: actions(history)[0]?.toolMeta ?? {} }} /></TraceStepDetailProvider>);
    expect(screen.getByRole('status')).toHaveTextContent(status === 'denied' ? 'export was denied' : 'artifact is unavailable');
    expect(screen.queryByRole('button', { name: 'Download verified snapshot' })).toBeNull();
  });

  it('keeps exact replay idempotent and cannot downgrade a committed reference', () => {
    const history = applyChatStreamFrame(initial, debugFrame());
    expect(applyChatStreamFrame(history, debugFrame())).toBe(history);
    expect(applyChatStreamFrame(history, debugFrame(fixture.same_visit_claim_replacement_warning as typeof fixture.first_attempt_history))).toBe(history);
    expect(actions(history)).toHaveLength(1);
  });

  it('promotes a valid same-visit warning to its committed reference', () => {
    const warning = applyChatStreamFrame(initial, debugFrame(fixture.same_visit_claim_replacement_warning as typeof fixture.first_attempt_history));
    const committed = applyChatStreamFrame(warning, debugFrame());
    expect(actions(committed)).toHaveLength(1);
    expect(actions(committed)[0]?.toolMeta?.['code_debug_v1']).toEqual(fixture.first_attempt_history.metadata.code_debug_v1);
  });

  it('retains the retry warning independently from the first attempt artifact', () => {
    let history = applyChatStreamFrame(initial, debugFrame());
    history = applyChatStreamFrame(history, debugFrame(fixture.retry_current_warning as typeof fixture.first_attempt_history));
    expect(actions(history)).toHaveLength(2);
    expect(actions(history)[0]?.toolMeta?.['code_debug_v1']).toEqual(fixture.first_attempt_history.metadata.code_debug_v1);
    expect(actions(history)[1]?.toolMeta?.['code_debug_v1']).toEqual(fixture.retry_current_warning.metadata.code_debug_v1);
  });

  it.each([
    changedProof({ revision: 2 }), changedProof({ extra: 'private' }),
    { ...debugFrame(), execution_generation: '7' },
    { ...debugFrame(), execution_generation: 'bf100012-c520-4a0f-8150-fb9d356b6658' }, { ...debugFrame(), message_id: 'another-response' },
    { ...debugFrame(), execution_id: 'another-execution' }, { ...debugFrame(), execution_generation: undefined },
    { ...debugFrame(), content: 'private payload' },
    { ...debugFrame(), response_metadata: { ...fixture.first_attempt_history, tool_inputs: { private: 'value' } } },
    { ...debugFrame(), response_metadata: { ...fixture.first_attempt_history, tool_output: 'private' } },
    { ...debugFrame(), response_metadata: { ...fixture.first_attempt_history, finish_reason: 'unknown' } },
    { ...debugFrame(), response_metadata: { ...fixture.first_attempt_history, run_id: 'another-run' } },
    { ...debugFrame(), response_metadata: { ...fixture.first_attempt_history, tool_run_id: 'ordinary-run', run_id: 'ordinary-run' } },
    { ...debugFrame(), response_metadata: { ...fixture.first_attempt_history, timestamp_finish: '2026-10-05T12:00:00Z' } },
    { ...debugFrame(), response_metadata: { ...fixture.first_attempt_history, tool_meta: { ...fixture.first_attempt_history.tool_meta,
      metadata: { ...fixture.first_attempt_history.tool_meta.metadata, code_debug_v1: { ...fixture.first_attempt_history.metadata.code_debug_v1, attempt: 2 } } } } },
  ])('rejects malformed, conflicting, payload-bearing and unfenced completions', (frame) => {
    expect(applyChatStreamFrame(initial, frame)).toBe(initial);
  });

  it('ignores conflicting visits or artifact bytes on the existing run id', () => {
    const history = applyChatStreamFrame(initial, debugFrame());
    expect(applyChatStreamFrame(history, changedProof({ generation: '8' }))).toBe(history);
    expect(applyChatStreamFrame(history, changedProof({ activation_id: 'e'.repeat(64) }))).toBe(history);
    expect(applyChatStreamFrame(history, changedProof({ artifact: { ...fixture.first_attempt_history.metadata.code_debug_v1.artifact, sha256: 'f'.repeat(64) } }))).toBe(history);
    expect(applyChatStreamFrame(history, { ...debugFrame(), response_metadata: { tool_run_id: fixture.first_attempt_history.tool_run_id } })).toBe(history);
  });

  it('rejects completion without the active message browser selector', () => {
    const unfenced = [{ ...initial[0]!, executionGeneration: undefined }];
    expect(applyChatStreamFrame(unfenced, debugFrame())).toBe(unfenced);
  });

  it('renders ordinary completion-only tools without Code debug receipts', () => {
    const history = applyChatStreamFrame(initial, { type: 'agent_tool_end', message_id: response, execution_generation: browserGeneration,
      response_metadata: { tool_run_id: 'ordinary', tool_name: 'search', tool_output: 'result' } });
    expect(actions(history)).toHaveLength(1);
    expect(actions(history)[0]).toMatchObject({ id: 'ordinary', name: 'search', status: ToolActionStatus.complete, toolOutputs: 'result' });
    expect(actions(history)[0]?.toolMeta).not.toHaveProperty('code_debug_v1');
  });

  it('rejects a Code debug completion whose proof copies are both missing', () => {
    const frame = debugFrame();
    const attrs = frame.response_metadata!;
    expect(applyChatStreamFrame(initial, { ...frame, response_metadata: {
      ...attrs, metadata: {}, tool_meta: { ...attrs.tool_meta, metadata: {} },
    } })).toBe(initial);
  });
});
