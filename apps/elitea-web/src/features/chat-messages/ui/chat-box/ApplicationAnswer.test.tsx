import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { normaliseAssistantMessage } from '@/entities/message';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';
import type { ChatContinueAuthModalSlotProps } from '../chat-continue/ChatContinue';
import { ApplicationAnswer } from './ApplicationAnswer';

describe('ApplicationAnswer', () => {
  it.each(['sharepoint', 'openapi', 'custom-delegated'])('opens configured %s OAuth without losing public settings', (toolkitType) => {
    const onContinue = vi.fn();
    const publicMetadata = {
      authorization_servers: ['https://login.example.test'],
      oauth_authorization_server: {
        authorization_endpoint: 'https://login.example.test/authorize',
        token_endpoint: 'https://login.example.test/token',
      },
      provided_settings: { mcp_client_id: 'public-client', mcp_client_secret: '********', scopes: ['offline_access', 'read'] },
      configuration_uuid: 'config-1',
      toolkit_id: '12',
    };
    const answer = normaliseAssistantMessage({
      id: 'response-row', uuid: 'answer-configured', content: '',
      created_at: '2026-09-06T00:00:00Z',
      meta: { authorization_requests: [{
        interrupt_id: 'auth-configured', tool_call_id: 'call-1', guardrail_type: 'mcp_auth',
        available_actions: ['authorize', 'skip'], parent_agent_name: 'Child',
        server_url: 'https://api.example.test', toolkit_type: toolkitType,
        resource_metadata: publicMetadata,
      }] },
    }, [], undefined) as unknown as ChatMessage;
    const renderAuthModal = vi.fn((props: ChatContinueAuthModalSlotProps) => <button onClick={() => { props.onClose(true); }}>Complete OAuth</button>);
    renderWithTheme(<ApplicationAnswer answer={answer} messageId={answer.id} toolActions={answer.toolActions}
      continuation={{ onContinueMcpExecution: onContinue, renderAuthModal }} />);
    fireEvent.click(screen.getByRole('button', { name: 'Authorize' }));
    expect(onContinue).not.toHaveBeenCalled();
    expect(renderAuthModal.mock.lastCall?.[0]).toMatchObject({
      toolkitId: '12',
      mcpAuthMetadata: { configurationUuid: 'config-1', providedSettings: publicMetadata.provided_settings,
        oauthAuthorizationServer: publicMetadata.oauth_authorization_server },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Complete OAuth' }));
    expect(onContinue).toHaveBeenCalledExactlyOnceWith('answer-configured', false, 'auth-configured');
  });

  it('does not invent endpoints after configured OAuth discovery fails', () => {
    const answer = { id: 'answer-unavailable', role: 'assistant', content: '', toolActions: [{
      id: 'auth-1', authorizationRequestId: 'auth-1', type: 'toolkit', status: 'action_required',
      toolMeta: { interrupt_id: 'auth-1', resource_metadata: {
        authorization_servers: ['https://login.example.test'], provided_settings: { mcp_client_id: 'public-client' },
      } },
      toolOutputs: { server_url: 'https://api.example.test' },
    }] } as unknown as ChatMessage;
    renderWithTheme(<ApplicationAnswer answer={answer} messageId={answer.id} toolActions={answer.toolActions}
      continuation={{ renderAuthModal: () => null, onContinueMcpExecution: vi.fn() }} />);
    expect(screen.getByRole('button', { name: 'Authorize' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Skip Auth' })).toBeEnabled();
    expect(screen.getByText('Authorization details are unavailable. You can skip this tool.')).toBeInTheDocument();
  });

  it('reloads exact parallel Skip controls without inventing OAuth discovery data', () => {
    const onContinue = vi.fn();
    const answer = normaliseAssistantMessage({
      id: 'response-row',
      uuid: 'answer-pending',
      content: 'Both children need authorization.',
      created_at: '2026-09-06T00:00:00Z',
      meta: {
        authorization_requests: ['name', 'surname'].map((id) => ({
          interrupt_id: `auth-${id}`,
          tool_call_id: `call-${id}`,
          guardrail_type: 'mcp_auth',
          available_actions: ['authorize', 'skip'],
          parent_agent_name: `${id} resolver`,
          parent_agent_call_id: `child-${id}`,
          parent_agent_path: [{ name: `${id} resolver`, call_id: `child-${id}` }],
          resource_metadata: null,
          authorization_servers: null,
        })),
      },
    }, [], undefined) as unknown as ChatMessage;

    renderWithTheme(<ApplicationAnswer
      answer={answer}
      messageId={answer.id}
      toolActions={answer.toolActions}
      continuation={{ onContinueMcpExecution: onContinue }}
    />);

    const skipButtons = screen.getAllByRole('button', { name: 'Skip Auth' });
    expect(skipButtons).toHaveLength(2);
    const guard = screen.getByRole('region', { name: 'surname resolver authorization' });
    const answerText = screen.getByText('Both children need authorization.');
    expect(guard.compareDocumentPosition(answerText) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);
    for (const button of screen.getAllByRole('button', { name: 'Authorize' })) {
      expect(button).toBeDisabled();
    }
    fireEvent.click(skipButtons[1]!);
    expect(onContinue).toHaveBeenCalledExactlyOnceWith('answer-pending', true, 'auth-surname');
  });

  it('renders durable partial content with its terminal error', () => {
    const answer = {
      id: 'answer-1',
      role: 'assistant',
      content: 'The response reached this durable point.',
      exception: 'The runtime operation failed.',
    } as ChatMessage;

    renderWithTheme(<ApplicationAnswer answer={answer} messageId={answer.id} />);

    expect(screen.getByText('The response reached this durable point.')).toBeInTheDocument();
    expect(screen.getByText('The runtime operation failed.')).toBeInTheDocument();
  });

  it('renders a refusal that arrived before any content', () => {
    // The shape `recordStreamFailure` appends when a run is refused before it
    // streams anything (`lib/chatStreamSettle.ts`): no content, no tool
    // actions, only the reason. Nothing else on the row would be visible, so
    // if the exception did not render the turn would look like a lost message
    // — which is exactly what a live stack showed.
    const answer = {
      id: 'answer-2',
      role: 'assistant',
      content: '',
      exception: 'Configuration type is not supported.',
      isStreaming: false,
      isLoading: false,
    } as ChatMessage;

    renderWithTheme(<ApplicationAnswer answer={answer} messageId={answer.id} />);

    expect(screen.getByTestId('error-trace')).toBeInTheDocument();
    expect(screen.getByText('Configuration type is not supported.')).toBeInTheDocument();
  });

  it('renders every authorization request and resumes only after OAuth succeeds', () => {
    const onContinue = vi.fn();
    const action = (id: string) => ({
      id,
      authorizationRequestId: id,
      type: 'toolkit',
      status: 'action_required',
      toolMeta: { interrupt_id: id, authorization_servers: ['https://login.example.test'] },
      toolOutputs: { server_url: 'https://mcp.example.test' },
    });
    const answer = {
      id: 'answer-auth',
      role: 'assistant',
      content: '',
      toolActions: [action('auth-1'), action('auth-2')],
    } as unknown as ChatMessage;

    renderWithTheme(<ApplicationAnswer
      answer={answer}
      messageId={answer.id}
      toolActions={answer.toolActions}
      continuation={{
        onContinueMcpExecution: onContinue,
        renderAuthModal: (props) => <button onClick={() => { props.onClose(true); }}>Complete OAuth</button>,
      }}
    />);

    expect(screen.getAllByRole('button', { name: 'Authorize' })).toHaveLength(2);
    fireEvent.click(screen.getAllByRole('button', { name: 'Authorize' })[0]!);
    expect(onContinue).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Complete OAuth' }));
    expect(onContinue).toHaveBeenCalledWith('answer-auth', false, 'auth-1');
  });
});

/**
 * The two elements this row lacked entirely, both visible in every reference
 * screenshot and measured on the live production transcript.
 */
describe('ApplicationAnswer caption line and reasoning summary', () => {
  const withActions = {
    id: 'answer-3',
    role: 'assistant',
    content: 'Hello! How can I help you today?',
    createdAt: '2026-08-29T10:00:00Z',
    toolActions: [
      {
        id: 'step-1',
        type: 'thinking_step',
        status: 'complete',
        name: 'Thinking step',
        content: 'The user is greeting me.',
        created_at: '2026-08-29T10:00:00Z',
        timestamp: '2026-08-29T10:00:01Z',
        ended_at: '2026-08-29T10:00:01Z',
      },
    ],
  } as unknown as ChatMessage;

  it('captions the answer with the participant, the reply-to link and the time', () => {
    renderWithTheme(
      <ApplicationAnswer
        answer={withActions}
        messageId={withActions.id}
        toolActions={withActions.toolActions}
        author={{ participantName: 'Elitea' }}
      />,
    );

    const header = screen.getByTestId('chat-message-header');
    expect(header.textContent).toContain('Elitea');
    expect(screen.getByTestId('chat-message-recipient').textContent).toBe('Message');
    expect(screen.getByTestId('chat-message-time')).toBeInTheDocument();
    expect(screen.getByTestId('chat-message-avatar')).toBeInTheDocument();
  });

  it('collapses the reasoning behind a "Thought for ..." summary instead of dumping it inline', () => {
    // Reasoning steps used to render as flat, always-open `ActionView` rows
    // under a bright "Thinking step" heading, so the whole chain of thought sat
    // above every answer. The production row shows ONE collapsed line.
    renderWithTheme(
      <ApplicationAnswer
        answer={withActions}
        messageId={withActions.id}
        toolActions={withActions.toolActions}
        author={{ participantName: 'Elitea' }}
      />,
    );

    const summary = screen.getByRole('button', { name: /Thought for/ });
    expect(summary).toBeInTheDocument();
    expect(summary.getAttribute('aria-expanded')).toBe('false');
    // The step text is still in the tree (MUI keeps a collapsed panel
    // mounted) but its panel is closed, so none of it is on screen — the
    // defect was that it had no panel at all and rendered flat above the
    // answer.
    const panel = screen.getByText('The user is greeting me.').closest('.MuiCollapse-root');
    expect(panel).not.toBeNull();
    expect(panel?.className).toContain('MuiCollapse-hidden');
  });

  it('still opens the reasoning panel of a turn the reader WATCHED stream', async () => {
    // The regression the toolkit journey caught, and the reason a click test
    // on a freshly-mounted row could not: this panel mounts with its first
    // action, which arrives while the turn is still streaming. MUI decides
    // controlled-vs-uncontrolled once, on that first render, so a panel that
    // took `expanded` only while `isStreaming` mounted CONTROLLED and stayed
    // controlled — and it was passed no `onChange`. When the turn settled the
    // panel closed and the summary became inert: every tool call and every
    // reasoning step of the turn was unreachable until a page reload.
    //
    // Reloading is what the earlier test above renders, which is why it kept
    // passing. The transition is the subject here, so the render starts
    // streaming and the flag is dropped exactly as the settle path drops it.
    const user = userEvent.setup();
    const { rerender } = renderWithTheme(
      <ApplicationAnswer
        answer={withActions}
        messageId={withActions.id}
        toolActions={withActions.toolActions}
        author={{ participantName: 'Elitea' }}
        status={{ isStreaming: true }}
      />,
    );

    // While the turn runs the panel is forced open, as in the baseline.
    expect(screen.getByRole('button', { name: /Thought for/ }).getAttribute('aria-expanded')).toBe('true');

    rerender(
      <ApplicationAnswer
        answer={withActions}
        messageId={withActions.id}
        toolActions={withActions.toolActions}
        author={{ participantName: 'Elitea' }}
        status={{ isStreaming: false }}
      />,
    );

    const summary = screen.getByRole('button', { name: /Thought for/ });
    expect(summary.getAttribute('aria-expanded')).toBe('false');

    await user.click(summary);
    expect(summary.getAttribute('aria-expanded')).toBe('true');
    expect(screen.getByText('The user is greeting me.')).toBeVisible();
  });
});

// Issue #625 item 1: per-word TTS highlight sync. `spokenRange` is one GLOBAL
// range (`useReadAloud`'s state, not per-message), so this row must gate it
// on `speakingMessageId` itself rather than trust the caller never to pass it
// for a row TTS is not reading.
describe('ApplicationAnswer TTS highlight gating', () => {
  const answer = {
    id: 'answer-tts',
    role: 'assistant',
    content: 'the quick brown fox',
  } as ChatMessage;

  it('highlights the spoken word when this row is the one speakingMessageId names', () => {
    const start = answer.content.indexOf('quick');
    renderWithTheme(
      <ApplicationAnswer
        answer={answer}
        messageId={answer.id}
        tts={{ speakingMessageId: answer.id, spokenRange: { start, end: start + 'quick'.length } }}
      />,
    );

    expect(screen.getByTestId('spoken-highlight')).toHaveTextContent('quick');
  });

  it('does not highlight when speakingMessageId names a DIFFERENT row', () => {
    const start = answer.content.indexOf('quick');
    renderWithTheme(
      <ApplicationAnswer
        answer={answer}
        messageId={answer.id}
        tts={{ speakingMessageId: 'some-other-message', spokenRange: { start, end: start + 'quick'.length } }}
      />,
    );

    expect(screen.queryByTestId('spoken-highlight')).not.toBeInTheDocument();
  });

  it('does not highlight while TTS is idle (no speakingMessageId, no spokenRange)', () => {
    renderWithTheme(<ApplicationAnswer answer={answer} messageId={answer.id} />);

    expect(screen.queryByTestId('spoken-highlight')).not.toBeInTheDocument();
  });
});
