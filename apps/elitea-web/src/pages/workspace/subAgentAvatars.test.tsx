/**
 * #1910: a desktop LOCAL sub-agent lane shows the spawn profile's name and,
 * when the profile named one, its avatar (from `subagent_started`), as the
 * cloud lane does.
 */
import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { TurnTranscript } from '@/features/workspace';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ProfileAvatar, TemporaryAvatar } from './subAgentAvatars';

type TurnView = Parameters<typeof TurnTranscript>[0]['view'];

const view = (profile: { displayName?: string; avatar?: string }): TurnView => ({
  phase: 'running',
  approvals: [],
  items: [{ type: 'tool', key: 'cc1', callId: 'c1', tool: 'spawn_agent', argsSummary: 'Review', remote: false }],
  subAgents: [{ agentId: 'a_1', parentCallId: 'c1', ordinal: 1, status: 'running', items: [], ...profile }],
});

describe('sub-agent lane avatars', () => {
  it("draws a local child's profile name and avatar", () => {
    renderWithTheme(<TurnTranscript view={view({ displayName: 'Reviewer', avatar: 'default:owl' })} busy subAgentAvatar={ProfileAvatar} />);
    const lane = screen.getByTestId('subagent-lane');
    expect(lane).toHaveTextContent('Reviewer');
    expect(within(lane).getByTestId('chat-message-avatar')).toBeInTheDocument();
    expect(within(lane).getByTestId('default-avatar')).toHaveAttribute('data-avatar-name', 'owl');
    expect(within(lane).queryByTestId('subagent-temporary')).toBeNull();
  });

  it('draws the brand mark for a key outside the default set', () => {
    renderWithTheme(<TurnTranscript view={view({ displayName: 'Reviewer', avatar: 'default:dragon' })} busy subAgentAvatar={ProfileAvatar} />);
    const lane = screen.getByTestId('subagent-lane');
    expect(within(lane).getByTestId('chat-message-avatar')).toBeInTheDocument();
    expect(within(lane).queryByTestId('default-avatar')).toBeNull();
  });

  it('draws no avatar for a local child whose profile named none', () => {
    renderWithTheme(<TurnTranscript view={view({})} busy subAgentAvatar={ProfileAvatar} />);
    const lane = screen.getByTestId('subagent-lane');
    expect(lane).toHaveTextContent('Sub-agent 1');
    expect(within(lane).queryByTestId('chat-message-avatar')).toBeNull();
  });

  it("draws a cloud child's dashed ring whatever its profile", () => {
    renderWithTheme(<TurnTranscript view={view({})} busy subAgentAvatar={TemporaryAvatar} />);
    const lane = screen.getByTestId('subagent-lane');
    expect(within(lane).getByTestId('chat-message-avatar')).toBeInTheDocument();
    expect(within(lane).queryByTestId('default-avatar')).toBeNull();
  });

  it("draws the image of a cloud child's avatar key inside the ring", () => {
    renderWithTheme(<TurnTranscript view={view({ avatar: 'default:fox' })} busy subAgentAvatar={TemporaryAvatar} />);
    expect(within(screen.getByTestId('subagent-lane')).getByTestId('default-avatar')).toHaveAttribute('data-avatar-name', 'fox');
  });
});
