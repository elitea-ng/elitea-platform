/**
 * ADR-0050 decision 7e in the transcript (issue 1612): a run of agent-to-agent
 * rows folds into one "Side conversation (n)" chip that expands into compact
 * "author → addressees" rows, and a platform line is centred with no avatar.
 *
 * The transcript is built the way the page builds it: wire message groups and
 * participants through `convertMessagesToChatHistory`, so the class, the
 * author and the addressee names come from the same fields Main writes.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import type { MessageGroupWire, MessageParticipantWire } from '@/entities/message';

import { convertMessagesToChatHistory } from '../../lib/convertMessagesToChatHistory';
import { ChatMessageList } from './ChatMessageList';
import { foldTranscript } from './SideTalk';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const PARTICIPANTS: MessageParticipantWire[] = [
  { id: '1', entity_name: 'user', kind: 'human', entity_meta: { id: 1 }, meta: { user_name: 'Reader' } },
  { id: '2', entity_name: 'application', kind: 'agent', entity_meta: { id: 41, name: 'release-bot' }, persona: { display_name: 'Mira' } },
  { id: '3', entity_name: 'application', kind: 'agent', entity_meta: { id: 42, name: 'CI Doctor' } },
  { id: '4', entity_name: 'application', kind: 'agent', entity_meta: { id: 43, name: 'Scribe' } },
  { id: '5', entity_name: 'system', kind: 'system', entity_meta: {}, meta: { name: 'turn_taking' } },
];

function group(id: number, fields: Partial<MessageGroupWire> & { readonly role: string }): MessageGroupWire {
  return {
    id: String(id),
    uuid: `00000000-0000-4000-8000-00000000000${id}`,
    content: `row ${id}`,
    created_at: new Date(Date.UTC(2026, 10, 8, 10, 0, id)).toISOString(),
    ...(fields.role === 'assistant' ? { reply_to_id: '1', question_id: '1' } : {}),
    ...fields,
  } as unknown as MessageGroupWire;
}

const GROUPS: MessageGroupWire[] = [
  group(1, { role: 'user', author_participant_id: '1', author_kind: 'human', content: '@Mira @CI Doctor is the release ready?' }),
  group(2, { role: 'assistant', author_participant_id: '2', author_kind: 'agent', addressed_to: [1], content: 'Checking with CI Doctor.' }),
  group(3, { role: 'assistant', author_participant_id: '2', author_kind: 'agent', addressed_to: [3], visibility: 'side', cause: 'agent_mention', content: 'Why is e2e red?' }),
  group(4, { role: 'assistant', author_participant_id: '3', author_kind: 'agent', addressed_to: [2, 4], visibility: 'side', cause: 'agent_mention', content: 'A flaky wait; fixed.' }),
  group(5, { role: 'assistant', author_participant_id: '5', author_kind: 'system', cause: 'system', content: 'The agents reached their turn limit for this message.' }),
  group(6, { role: 'assistant', author_participant_id: '3', author_kind: 'agent', addressed_to: [1], content: 'It is ready.' }),
];

function transcript() {
  return convertMessagesToChatHistory(GROUPS, PARTICIPANTS);
}

function renderList(): void {
  Element.prototype.scrollIntoView = vi.fn();
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <ChatMessageList chatHistory={transcript()} userId="1" projectId="1" />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe('foldTranscript', () => {
  it('folds the consecutive agent-to-agent rows into one entry, keyed by its first row', () => {
    const entries = foldTranscript(transcript());
    expect(entries.map((entry) => entry.kind)).toEqual(['row', 'row', 'side_talk', 'row', 'row']);
    const side = entries[2];
    expect(side?.kind === 'side_talk' ? side.rows.map((row) => row.message.content) : []).toEqual(['Why is e2e red?', 'A flaky wait; fixed.']);
    // The key is the first row's: stable while the run grows.
    expect(side?.kind === 'side_talk' ? side.key : '').toBe(`side:${transcript()[2]?.id ?? ''}`);
  });
});

describe('ChatMessageList side talk and system lines', () => {
  it('shows side talk collapsed under one chip and expands it into labelled compact rows', () => {
    renderList();
    const chip = screen.getByTestId('chat-side-talk-chip');
    expect(chip).toHaveTextContent('Side conversation (2)');
    expect(chip).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText('Why is e2e red?')).toBeNull();

    fireEvent.click(chip);
    expect(chip).toHaveAttribute('aria-expanded', 'true');
    const labels = screen.getAllByTestId('chat-side-talk-label').map((label) => label.textContent);
    expect(labels).toEqual(['Mira → CI Doctor', 'CI Doctor → Mira, Scribe']);
    expect(screen.getByText('A flaky wait; fixed.')).toBeInTheDocument();
  });

  it('draws a platform line centred, with no avatar', () => {
    renderList();
    const line = screen.getByTestId('chat-system-line');
    expect(line).toHaveTextContent('The agents reached their turn limit for this message.');
    expect(within(line).queryByTestId('chat-message-avatar')).toBeNull();
    // The answers to people still render as answers around it.
    expect(screen.getByText('It is ready.')).toBeInTheDocument();
  });
});
