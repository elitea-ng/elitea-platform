/**
 * Placement of every approval card (reference `AnswerApprovals.jsx`): below
 * the thinking view, coordinator cards plain, one approval accordion per
 * spawning instance labelled by `computeBreadcrumbs`.
 */
import { fireEvent, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { normalizeHitlInterrupt } from '../../lib/hitlInterrupts';
import { staticPauseBinding } from '../../lib/staticPipelinePause';
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';
import type { HitlResumePayload } from '../chat-hitl-actions/ChatHitlActions';
import { ApplicationAnswer } from './ApplicationAnswer';

type Tier = { name: string; call_id: string; sibling_ordinal?: number };
const at = '2026-10-16T10:00:00Z';

/** A pending sensitive-tool card shaped like the Worker's (`events.rs`) plus its hierarchy overlay. */
function guard(id: string, path: Tier[] = [], extra: Record<string, unknown> = {}) {
  const owner = path.at(-1);
  return {
    ...normalizeHitlInterrupt({
      type: 'hitl', interrupt_id: id, guardrail_type: 'sensitive_tool', node_name: 'sensitive_tool_guard',
      message: 'Deleting files needs approval.', available_actions: ['approve', 'reject', 'block_with_comment'],
      tool_call_id: `tc-${id}`, tool_name: 'delete_file', action_label: 'Delete file', policy_message: 'Deleting files needs approval.',
      tool_args: { path: '***' },
      ...(owner ? { parent_agent_path: path, parent_agent_name: owner.name, parent_agent_call_id: owner.call_id } : {}),
    }),
    ...extra,
  };
}

const step = (id: string, path: Tier[] = []) => ({
  id, type: 'tool', name: 'delete_file', status: 'complete', created_at: at, timestamp: at, toolOutputs: 'paused',
  ...(path.length ? { parent_agent_name: path.at(-1)?.name, parent_agent_call_id: path.at(-1)?.call_id, parent_agent_path: path } : {}),
});

function answerWith(interrupts: object[], toolActions: object[] = []): ChatMessage {
  return { id: 'answer-1', role: 'assistant', content: '', toolActions, hitlInterrupts: interrupts } as unknown as ChatMessage;
}

function renderAnswer(answer: ChatMessage, onHitlResume: (payload: HitlResumePayload) => void = vi.fn(), staticPause?: Parameters<typeof ApplicationAnswer>[0]['hitl']) {
  return renderWithTheme(
    <ApplicationAnswer
      answer={answer}
      messageId={answer.id}
      toolActions={answer.toolActions}
      hitl={{ hitlInterrupts: answer.hitlInterrupts, onHitlResume, ...staticPause }}
    />,
  );
}

const approvalSummaries = () =>
  screen.getAllByTestId('chat-answer-content').flatMap((block) => within(block).queryAllByTestId(/accordion-summary-/).map((node) => node.textContent));

describe('ApplicationAnswer approvals placement', () => {
  it('renders a root sensitive-tool card plain, below the thinking view, and Authorize/Block both answer it', () => {
    const onHitlResume = vi.fn<(payload: HitlResumePayload) => void>();
    renderAnswer(answerWith([guard('i1')], [step('s1')]), onHitlResume);
    const card = screen.getByTestId('sensitive-action-panel');
    const thinking = screen.getByTestId('chat-answer-thought-accordion');
    expect(thinking.compareDocumentPosition(card) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);
    expect(approvalSummaries()).toEqual([]);
    fireEvent.click(screen.getByTestId('sensitive-action-authorize-button'));
    fireEvent.click(screen.getByTestId('sensitive-action-block-button'));
    expect(onHitlResume.mock.calls.map(([payload]) => [payload.action, payload.interruptId])).toEqual([
      ['approve', 'i1'],
      ['reject', 'i1'],
    ]);
  });

  it('puts a pipeline → saved agent → tool guard card in the `Pipeline ▸ Agent` accordion', () => {
    const path = [{ name: 'Pipeline', call_id: 'p1' }, { name: 'Agent', call_id: 'a1' }];
    renderAnswer(answerWith([guard('i1', path)], [step('s1', path)]));
    expect(approvalSummaries()).toEqual(['Pipeline ▸ Agent']);
    const bucket = screen.getAllByTestId('chat-answer-nested-agent-accordion-details-Agent').at(-1) as HTMLElement;
    expect(within(bucket).getByTestId('sensitive-action-panel')).toBeTruthy();
  });

  it('gives two paused calls of one sub-agent their own accordions and leaves the sibling answerable', () => {
    const a = [{ name: 'Sub', call_id: 'call-a', sibling_ordinal: 1 }];
    const b = [{ name: 'Sub', call_id: 'call-b', sibling_ordinal: 2 }];
    const trace = [step('s1', a), step('s2', b)];
    const { rerender } = renderAnswer(answerWith([guard('i1', a), guard('i2', b)], trace));
    expect(approvalSummaries()).toEqual(['Sub (1)', 'Sub (2)']);
    // The first card was answered: it is queued and hidden; the second stays live.
    // The trace keeps the lone pending sibling numbered `(2)`.
    const answered = answerWith([guard('i1', a, { queued: true, hidden: true }), guard('i2', b)], trace);
    rerender(<ApplicationAnswer answer={answered} messageId={answered.id} toolActions={answered.toolActions} hitl={{ hitlInterrupts: answered.hitlInterrupts, onHitlResume: vi.fn() }} />);
    expect(approvalSummaries()).toEqual(['Sub (2)']);
    expect(screen.getByTestId('sensitive-action-authorize-button')).toBeEnabled();
  });

  it('numbers the root tier when the same sub-agent pauses under two parents', () => {
    renderAnswer(answerWith([
      guard('i1', [{ name: 'Parent', call_id: 'p1', sibling_ordinal: 1 }, { name: 'Sub', call_id: 's1' }]),
      guard('i2', [{ name: 'Parent', call_id: 'p2', sibling_ordinal: 2 }, { name: 'Sub', call_id: 's2' }]),
    ]));
    expect(approvalSummaries()).toEqual(['Parent (1) ▸ Sub', 'Parent (2) ▸ Sub']);
  });

  it('disables a queued card that is still shown', () => {
    renderAnswer(answerWith([guard('i1', [], { queued: true })]));
    expect(screen.getByTestId('sensitive-action-authorize-button')).toBeDisabled();
  });

  it('renders the answered ask_user summary like the reference', () => {
    const askUser = {
      id: 'ask-1', type: 'tool', name: 'ask_user', status: 'complete', created_at: at, timestamp: at,
      toolInputs: { questions: [{ id: 'q1', question: 'Which region?' }] },
      toolOutputs: 'User answered:\n- Which region?: EU',
    };
    renderAnswer({ id: 'answer-1', role: 'assistant', content: 'Done.', toolActions: [askUser] } as unknown as ChatMessage);
    const summary = screen.getByTestId('ask-user-answer-summary');
    expect(summary.textContent).toContain('1. Which region?');
    expect(summary.textContent).toContain('EU');
  });

  it('renders a root static pause as one inline card that continues on its own', () => {
    const pause = staticPauseBinding('30e0913e-10d4-43db-b8d0-c7b79480935a', 'ee92ccbd-3312-4c72-b20b-fddf224e7c0e', 'root', {
      revision: 1, pause_id: `pipeline-static:sha256:${'a'.repeat(64)}`, checkpoint_id: 'c', kind: 'before', node_name: 'tick',
      definition_digest: `sha256:${'b'.repeat(64)}`, node_digest: `sha256:${'c'.repeat(64)}`, pending_nodes: ['tick'], step: 1, descendant_path: [],
    }, undefined);
    if (!pause) throw new Error('fixture');
    const onContinue = vi.fn();
    renderAnswer(answerWith([]), vi.fn(), { staticPause: { pause, busy: false, onContinue } });
    const card = screen.getByTestId('chat-static-pause-panel');
    expect(card.textContent).toContain('tick');
    expect(card.textContent).not.toMatch(/before|root|child/);
    expect(screen.queryByRole('textbox')).toBeNull();
    fireEvent.click(screen.getByTestId('chat-static-pause-continue-button'));
    expect(onContinue).toHaveBeenCalledExactlyOnceWith(undefined);
  });
});
