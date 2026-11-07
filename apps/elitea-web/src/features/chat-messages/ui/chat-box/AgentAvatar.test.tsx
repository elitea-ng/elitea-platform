import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { MESSAGE_CLASS } from '@elitea/client-core/conversation';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { AgentAvatar } from './AgentAvatar';
import { AssistantCaption } from './MessageAvatar';

describe('AgentAvatar', () => {
  it('shows the agent icon as the still, and the brand mark without one', () => {
    const { container, rerender } = renderWithTheme(<AgentAvatar iconUrl="/api/v2/artifacts/icon.png" />);
    expect(container.querySelector('img')?.getAttribute('src')).toBe('/api/v2/artifacts/icon.png');
    expect(screen.getByTestId('chat-message-avatar')).toHaveAttribute('data-motion', 'still');
    expect(screen.queryByTestId('agent-avatar-motion')).toBeNull();

    rerender(<AgentAvatar />);
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('svg')).not.toBeNull();
  });

  it.each(['thinking', 'speaking', 'working', 'waiting', 'success', 'error', 'waking'] as const)('draws the %s motion over the still', (motion) => {
    renderWithTheme(<AgentAvatar motion={motion} />);
    expect(screen.getByTestId('chat-message-avatar')).toHaveAttribute('data-motion', motion);
    expect(screen.getByTestId('agent-avatar-motion')).toBeInTheDocument();
  });

  it('idle has no layer over the still', () => {
    renderWithTheme(<AgentAvatar motion="idle" />);
    expect(screen.queryByTestId('agent-avatar-motion')).toBeNull();
  });
});

describe('AssistantCaption', () => {
  it('shows the name, the persona role and the agent icon', () => {
    const { container } = renderWithTheme(<AssistantCaption name="Mira" personaRole="Release manager" iconUrl="/icon.png" motion="speaking" />);
    expect(screen.getByTestId('chat-message-author')).toHaveTextContent('Mira');
    expect(screen.getByTestId('chat-message-author-role')).toHaveTextContent('Release manager');
    expect(container.querySelector('img')?.getAttribute('src')).toBe('/icon.png');
    expect(screen.getByTestId('chat-message-avatar')).toHaveAttribute('data-motion', 'speaking');
  });

  it('marks an ephemeral sub-agent and a heartbeat, and nothing for a plain answer', () => {
    const { rerender } = renderWithTheme(<AssistantCaption name="CI Sleuth" messageClass={MESSAGE_CLASS.Ephemeral} />);
    expect(screen.getByTestId('chat-message-author-marker')).toHaveTextContent('temporary');
    expect(screen.getByTestId('chat-message-author')).toHaveAttribute('data-message-class', 'ephemeral');

    rerender(<AssistantCaption name="Mira" messageClass={MESSAGE_CLASS.Heartbeat} />);
    expect(screen.getByTestId('chat-message-author-marker')).toHaveTextContent('woke up');

    rerender(<AssistantCaption name="Mira" messageClass={MESSAGE_CLASS.AgentToHumans} />);
    expect(screen.queryByTestId('chat-message-author-marker')).toBeNull();
    expect(screen.queryByTestId('chat-message-author-role')).toBeNull();
  });
});
