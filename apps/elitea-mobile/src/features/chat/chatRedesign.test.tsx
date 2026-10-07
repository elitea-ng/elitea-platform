import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import * as Clipboard from 'expo-clipboard';
import type { ReactElement } from 'react';

import { AnswerActions } from '@/features/chat/actions/AnswerActions';
import { planActions } from '@/features/chat/actions/messageActions';
import { AssistantAvatar } from '@/features/chat/AssistantAvatar';
import { ChatEmptyState } from '@/features/chat/ChatEmptyState';
import { Composer } from '@/features/chat/Composer';
import { LiveReply, messageMeta, PersistedMessage } from '@/features/chat/ThreadRows';
import { SAMPLE_MESSAGES, sampleLive } from '@/features/designGallery/chatFixtures';

jest.mock('expo-clipboard', () => ({ setStringAsync: jest.fn(async () => true) }));
// The standard text size (the RN jest window reports font scale 2, an accessibility size).
jest.mock('react-native/Libraries/Utilities/useWindowDimensions', () => ({
  __esModule: true,
  default: () => ({ width: 393, height: 852, scale: 3, fontScale: 1 }),
}));

const answer = SAMPLE_MESSAGES[1];
const queries = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });
const withQueries = (ui: ReactElement) => render(<QueryClientProvider client={queries()}>{ui}</QueryClientProvider>);
const plan = (over: Partial<Parameters<typeof planActions>[1]> = {}) =>
  planActions(answer, { shareOut: true, contract13: true, online: true, canSpeak: true, canRegenerate: true, ...over });
const handlers = {
  projectId: null,
  reading: false,
  onReadAloud: jest.fn(),
  onStopReading: jest.fn(),
  onRegenerate: jest.fn(),
  onMore: jest.fn(),
  onDislike: jest.fn(),
  onNote: jest.fn(),
};

describe('the answer action row (§4.6)', () => {
  it('offers copy, like, dislike, read aloud, regenerate and the menu, as in the reference', async () => {
    await withQueries(<AnswerActions message={answer} plan={plan()} {...handlers} />);
    for (const name of ['Copy answer', 'Like', 'Dislike', 'Read answer aloud', 'Regenerate', 'Answer actions']) {
      expect(screen.getByRole('button', { name })).toBeTruthy();
    }
  });

  it('draws no dead buttons: no copy without share-out, no rating before 1.3, no regenerate on an older answer', async () => {
    await withQueries(<AnswerActions message={answer} plan={plan({ shareOut: false, contract13: false, canRegenerate: false })} {...handlers} />);
    expect(screen.queryByRole('button', { name: 'Copy answer' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Like' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Regenerate' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Read answer aloud' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Answer actions' })).toBeTruthy();
  });

  it('copies the answer and confirms it with a toast note', async () => {
    const onNote = jest.fn();
    await withQueries(<AnswerActions message={answer} plan={plan()} {...handlers} onNote={onNote} />);
    await fireEvent.press(screen.getByRole('button', { name: 'Copy answer' }));
    await waitFor(() => expect(onNote).toHaveBeenCalledWith('Copied'));
    expect(Clipboard.setStringAsync).toHaveBeenCalledWith(answer.text);
  });

  it('read aloud is a toggle: while reading it stops', async () => {
    const onStopReading = jest.fn();
    await withQueries(<AnswerActions message={answer} plan={plan()} {...handlers} reading onStopReading={onStopReading} />);
    await fireEvent.press(screen.getByRole('button', { name: 'Stop reading answer' }));
    expect(onStopReading).toHaveBeenCalled();
  });
});

describe('answers and the live reply', () => {
  it('an answer shows its agent avatar, the name line and the action row; markdown code is a copyable card', async () => {
    await withQueries(
      <PersistedMessage
        message={answer}
        highlight={{ people: [], agents: [] }}
        opening={null}
        onOpenFile={null}
        tools={[]}
        onOpenTool={jest.fn()}
        onActions={jest.fn()}
        avatar={<AssistantAvatar name="Research" imageUri={null} mascot />}
        actions={<AnswerActions message={answer} plan={plan()} {...handlers} />}
      />,
    );
    expect(screen.getByText(/^Research · /)).toBeTruthy();
    expect(screen.getByTestId('markdown-code-block')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Copy python code block' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Like' })).toBeTruthy();
  });

  it('thinking: the typing dots and "Thinking…" for screen readers; streaming: the caret', async () => {
    const view = await render(
      <LiveReply live={sampleLive('thinking')} author="Research" onRetry={jest.fn()} onReconnect={jest.fn()} onDismiss={jest.fn()} onOpenTool={jest.fn()} usageAvailable />,
    );
    expect(screen.getByTestId('thinking-dots', { includeHiddenElements: true })).toBeTruthy();
    expect(screen.getByText('Thinking…')).toBeTruthy();
    await view.rerender(
      <LiveReply live={sampleLive('streaming')} author="Research" onRetry={jest.fn()} onReconnect={jest.fn()} onDismiss={jest.fn()} onOpenTool={jest.fn()} usageAvailable />,
    );
    expect(screen.getByTestId('streaming-caret', { includeHiddenElements: true })).toBeTruthy();
    expect(screen.queryByText('Thinking…')).toBeNull();
  });

  it('the name line says who answered and when', () => {
    const now = new Date(2026, 9, 7, 15, 0).getTime();
    expect(messageMeta('Research', new Date(2026, 9, 7, 9, 41).getTime(), now)).toMatch(/^Research · .*9.*41/);
    expect(messageMeta('Research', null, now)).toBe('Research');
    expect(messageMeta(null, null, now)).toBeNull();
  });
});

describe('the empty conversation (§4.9)', () => {
  it('shows the mascot when the agent has no icon, and a suggestion goes into the composer, unsent', async () => {
    const onSuggestion = jest.fn();
    await render(<ChatEmptyState agentName="Research" iconUri={null} welcome={null} suggestions={['Find open bugs']} onSuggestion={onSuggestion} bottomInset={80} />);
    expect(screen.getByText('Ask Research anything')).toBeTruthy();
    await fireEvent.press(screen.getByRole('button', { name: 'Find open bugs' }));
    expect(onSuggestion).toHaveBeenCalledWith('Find open bugs');
  });

  it('the composer takes a suggestion as a draft and sends only on Send', async () => {
    const onSend = jest.fn(() => null);
    const view = await render(<Composer busy={false} onSend={onSend} draft={null} />);
    await view.rerender(<Composer busy={false} onSend={onSend} draft={{ text: 'Find open bugs', key: 1 }} />);
    expect(screen.getByLabelText('Message').props.value).toBe('Find open bugs');
    expect(onSend).not.toHaveBeenCalled();
  });

  it('the agent icon wins over the mascot', async () => {
    await render(<ChatEmptyState agentName="Research" iconUri="data:image/png;base64,AAAA" welcome="Hi" suggestions={[]} onSuggestion={jest.fn()} bottomInset={80} />);
    expect(screen.getByText('Hi')).toBeTruthy();
    expect(JSON.stringify(screen.toJSON())).toContain('data:image/png;base64,AAAA');
  });
});
