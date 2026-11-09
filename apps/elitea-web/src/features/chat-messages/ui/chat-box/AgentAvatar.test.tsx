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

  it('plays a pack slot as its own motion, and the still with the default motion for an empty slot', () => {
    const media = (ext: string) => `/api/v2/agent_media/1/${'ab'.repeat(32)}.${ext}`;
    const pack = {
      schema: 'elitea.avatar_pack/v1',
      slots: { still: { url: media('png') }, speaking: { url: media('webm'), poster_url: media('png') }, thinking: { url: media('webp') } },
    };
    const { container, rerender } = renderWithTheme(<AgentAvatar iconUrl="/icons/1/old.png" pack={pack} motion="speaking" />);
    const video = screen.getByTestId('agent-avatar-video') as HTMLVideoElement;
    expect(video.getAttribute('src')).toBe(media('webm'));
    expect(video.getAttribute('poster')).toBe(media('png'));
    expect(video.muted).toBe(true);
    expect(video.loop).toBe(true);
    expect(screen.queryByTestId('agent-avatar-motion')).toBeNull();

    rerender(<AgentAvatar iconUrl="/icons/1/old.png" pack={pack} motion="thinking" />);
    expect(container.querySelector('img')?.getAttribute('src')).toBe(media('webp'));
    expect(screen.queryByTestId('agent-avatar-motion')).toBeNull();

    rerender(<AgentAvatar iconUrl="/icons/1/old.png" pack={pack} motion="working" />);
    expect(container.querySelector('img')?.getAttribute('src')).toBe(media('png'));
    expect(screen.getByTestId('agent-avatar-motion')).toBeInTheDocument();

    rerender(<AgentAvatar iconUrl="/icons/1/old.png" pack={pack} motion="still" />);
    expect(screen.queryByTestId('agent-avatar-video')).toBeNull();
    expect(container.querySelector('img')?.getAttribute('src')).toBe(media('png'));
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
