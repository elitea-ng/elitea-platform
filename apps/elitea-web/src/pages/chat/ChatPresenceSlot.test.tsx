/**
 * A desktop Local work thread opened on the web (ADR-0061 decision 6, issue 3069): the participants panel is
 * read-only (agents only, Local/Cloud badges, the Lead chip, no lead menu), and an answer whose turn ran a command
 * outside the sandbox says so — only when Main stamped `meta.local_work.ran_outside_sandbox`.
 */
import { act, renderHook, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { RanOutsideSandboxMarker } from '@/features/chat-messages/ui/chat-box/UnattendedMarkers';
import { unattendedMarksOf } from '@/features/chat-messages/lib/unattendedMarks';
import { applyLocalAgentState, useAgentAvatarStore } from '@/features/chat-messages/model/agentAvatar.store';
import { ranOutsideSandbox } from '@/shared/lib/localWork';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { presenceAgents, usePresenceSlot } from './ChatPresenceSlot';

const PARTICIPANTS = [
  { id: 5, entity_name: 'user' },
  { id: 11, entity_name: 'application', entity_meta: { name: 'Mira' } },
  { id: 12, entity_name: 'application', entity_meta: { name: 'Reviewer' }, entity_settings: { executor: 'cloud' }, persona: { display_name: 'CI Doctor' } },
  { id: 13, entity_name: 'toolkit', entity_meta: { name: 'Jira' } },
];
const CONVERSATION = { uuid: 'conv-1', meta: { local_work: { folder_name: 'repo' }, floor: { policy: 'lead', lead_participant_id: 11 } }, participants: PARTICIPANTS };

beforeEach(() => {
  useAgentAvatarStore.resetForTests();
  vi.stubGlobal('matchMedia', (query: string) => ({ matches: false, media: query, addEventListener: () => {}, removeEventListener: () => {}, addListener: () => {}, removeListener: () => {}, onchange: null, dispatchEvent: () => false }));
});
afterEach(() => { vi.unstubAllGlobals(); });

describe('the read-only team view on the web (issue 3069)', () => {
  it('reads the agents and where each runs, persona names first', () => {
    expect(presenceAgents(PARTICIPANTS)).toEqual([
      { participantId: 11, name: 'Mira', cloud: false },
      { participantId: 12, name: 'CI Doctor', cloud: true },
    ]);
  });

  it('gives a Local work thread the agents-only, read-only panel with badges and no lead menu', () => {
    const lead = vi.fn(() => <span data-testid="chat-lead-menu" />);
    const { result } = renderHook(() => usePresenceSlot(CONVERSATION, lead, true));
    expect(result.current.panelProps).toMatchObject({ context: 'local_work', readOnly: true });
    act(() => { applyLocalAgentState('conv-1', { participant_id: 12, state: 'working', activity: { tool: 'jira_get_issue' } }); });
    const render = result.current.panelProps.renderAgentLead;
    expect(render).toBeDefined();
    renderWithTheme(<>{render?.({ conversationId: 'conv-1', participantId: 11 })}{render?.({ conversationId: 'conv-1', participantId: 12 })}</>);
    const executors = screen.getAllByTestId('participant-executor').map((chip) => chip.getAttribute('data-executor'));
    expect(executors).toEqual(['local', 'cloud']);
    expect(screen.getAllByTestId('participant-lead-badge')).toHaveLength(1);
    expect(screen.getByText('Running jira_get_issue')).toBeInTheDocument();
    expect(screen.queryByTestId('participant-lead-menu')).toBeNull();
    expect(lead).not.toHaveBeenCalled();
  });

  it('keeps an ordinary chat\'s own lead menu beside the live label', () => {
    const lead = vi.fn(() => <span data-testid="chat-lead-menu" />);
    const { result } = renderHook(() => usePresenceSlot({ ...CONVERSATION, meta: {} }, lead, false));
    expect(result.current.panelProps).not.toHaveProperty('context');
    act(() => { applyLocalAgentState('conv-1', { participant_id: 11, state: 'thinking' }); });
    renderWithTheme(<>{result.current.panelProps.renderAgentLead?.({ conversationId: 'conv-1', participantId: 11 })}</>);
    expect(screen.getByTestId('chat-lead-menu')).toBeInTheDocument();
    expect(screen.getByTestId('participant-state')).toHaveTextContent('Thinking');
    expect(screen.queryByTestId('participant-executor')).toBeNull();
    // A long agent name must not squeeze the lead menu under the row's hover actions (the webkit journey, issue 3067).
    expect(screen.getByTestId('participant-presence-lead')).toHaveStyle({ flexShrink: '0' });
  });
});

describe('the "Ran outside the sandbox" marker (issue 3069)', () => {
  it('is set only by meta.local_work.ran_outside_sandbox === true', () => {
    expect(ranOutsideSandbox({ local_work: { ran_outside_sandbox: true } })).toBe(true);
    for (const meta of [undefined, null, {}, { local_work: {} }, { local_work: null }, { local_work: { ran_outside_sandbox: 'true' } }, { ran_outside_sandbox: true }]) {
      expect(ranOutsideSandbox(meta)).toBe(false);
    }
  });

  it('reaches the answer marks only when flagged', () => {
    expect(unattendedMarksOf({ local_work: { ran_outside_sandbox: true } })).toEqual({ ranOutsideSandbox: true });
    expect(unattendedMarksOf({ local_work: { folder_name: 'repo' } })).toEqual({});
  });

  it('renders the marker text', () => {
    renderWithTheme(<RanOutsideSandboxMarker />);
    expect(screen.getByTestId('chat-message-ran-outside-sandbox')).toHaveTextContent('Ran outside the sandbox');
  });
});
