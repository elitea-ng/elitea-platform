import { fireEvent, screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { SubAgentGroupable } from '../../lib/subAgentGrouping';
import { ApplicationAnswerThinking } from './ApplicationAnswerThinking';

const at = '2026-10-16T10:00:00Z';
const coord = (id: string, name: string) => ({ id, type: 'llm', name, status: 'complete', content: name, created_at: at, timestamp: at });
// The delegation wrapper the parent's ToolNode emits, then the child's inner chip.
const wrapper = (id: string, name: string, ordinal: number, status = 'complete') => ({
  id, type: 'tool', name, original_name: name, parent_agent_call_id: id, sibling_ordinal: ordinal, status,
  toolMeta: { toolkit_type: 'application' }, created_at: at, timestamp: at,
});
const inner = (id: string, owner: { name: string; call_id: string; sibling_ordinal?: number }[], status = 'complete') => ({
  id, type: 'tool', name: 'search', status, parent_agent_name: owner.at(-1)?.name, parent_agent_call_id: owner.at(-1)?.call_id,
  parent_agent_path: owner, created_at: at, timestamp: at, toolOutputs: 'ok',
});

const actions = (list: object[]) => list as unknown as SubAgentGroupable[];
const summaries = () => screen.queryAllByTestId(/chat-answer-nested-agent-accordion-summary-/).map((node) => node.textContent);

describe('ApplicationAnswerThinking', () => {
  it('gives two parallel calls of one sub-agent their own ordinal labels and never merges them', () => {
    renderWithTheme(
      <ApplicationAnswerThinking
        isStreaming
        actions={actions([
          coord('c1', 'Plan'),
          wrapper('call-a', 'Sub', 1, 'processing'),
          wrapper('call-b', 'Sub', 2, 'processing'),
          inner('i1', [{ name: 'Sub', call_id: 'call-a', sibling_ordinal: 1 }]),
          inner('i2', [{ name: 'Sub', call_id: 'call-b', sibling_ordinal: 2 }]),
        ])}
      />,
    );
    expect(summaries()).toEqual(['Sub (1)', 'Sub (2)']);
  });

  it('labels a nested instance with its full breadcrumb', () => {
    renderWithTheme(
      <ApplicationAnswerThinking
        isStreaming
        actions={actions([inner('i1', [{ name: 'Pipeline', call_id: 'p1' }, { name: 'Agent', call_id: 'a1' }])])}
      />,
    );
    expect(summaries()).toEqual(['Pipeline ▸ Agent']);
  });

  it('interleaves coordinator runs and sub-agents in timeline order', () => {
    renderWithTheme(
      <ApplicationAnswerThinking
        isStreaming
        actions={actions([coord('c1', 'Plan'), wrapper('call-a', 'Sub', 1), coord('c2', 'Summarise')])}
      />,
    );
    const details = screen.getByTestId('chat-answer-thought-accordion');
    const order = within(details)
      .getAllByText(/Plan|Summarise|^Sub$/)
      .map((node) => node.textContent);
    expect(order[0]).toBe('Plan');
    expect(order.at(-1)).toBe('Summarise');
  });

  it('stays folded while streaming and after, until the reader opens it', () => {
    const list = actions([coord('c1', 'Plan')]);
    const { rerender } = renderWithTheme(<ApplicationAnswerThinking isStreaming actions={list} />);
    const button = () => screen.getByRole('button', { name: /Thought for/ });
    expect(button().getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(button());
    expect(button().getAttribute('aria-expanded')).toBe('true');
    rerender(<ApplicationAnswerThinking isStreaming={false} actions={list} />);
    expect(button().getAttribute('aria-expanded')).toBe('true');
    fireEvent.click(button());
    expect(button().getAttribute('aria-expanded')).toBe('false');
  });

  it('stops the shimmer of an instance that owns a pending card', () => {
    const key = JSON.stringify([['Sub', 'call-a']]);
    renderWithTheme(
      <ApplicationAnswerThinking
        isStreaming
        pendingInstanceKeys={new Set([key])}
        actions={actions([inner('i1', [{ name: 'Sub', call_id: 'call-a' }], 'processing')])}
      />,
    );
    const summary = screen.getByTestId('chat-answer-nested-agent-accordion-summary-Sub');
    expect(summary.querySelector('.MuiCircularProgress-root')).toBeNull();
  });
});
