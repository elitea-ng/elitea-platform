import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen } from '@testing-library/react';

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
