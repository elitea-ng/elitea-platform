import { INITIAL_DRAFT_STATE } from '@elitea/client-core/agent-builder';
import { fireEvent, render, screen } from '@testing-library/react-native';

import { AgentBuilderView } from '@/features/agentBuilder/AgentBuilderView';
import type { AgentBuilder } from '@/features/agentBuilder/useAgentBuilder';

import { BLUEPRINTS } from './BlueprintGallery';

jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);

function idleBuilder(): AgentBuilder {
  return {
    state: INITIAL_DRAFT_STATE,
    generate: jest.fn(),
    refine: jest.fn(),
    save: jest.fn(async () => null),
    saving: false,
    saveError: null,
    reset: jest.fn(),
  };
}

test('the gallery lists the agent and team blueprints', () => {
  render(<AgentBuilderView builder={idleBuilder()} onSaved={jest.fn()} />);
  fireEvent.press(screen.getByTestId('agent-builder-blueprints'));

  for (const agent of BLUEPRINTS.agents) expect(screen.getByTestId(`blueprint-${agent.slug}`)).toBeTruthy();
  fireEvent.press(screen.getByText('Teams'));
  const team = BLUEPRINTS.teams[0];
  expect(screen.getByTestId(`blueprint-${team.slug}`)).toBeTruthy();
  expect(screen.getByText(`Members: ${(team.members ?? []).join(', ')}`)).toBeTruthy();
});

test('Craft from it fills the builder with the brief, and Generate sends it', () => {
  const builder = idleBuilder();
  render(<AgentBuilderView builder={builder} onSaved={jest.fn()} />);
  fireEvent.press(screen.getByTestId('agent-builder-blueprints'));
  const agent = BLUEPRINTS.agents[0];

  fireEvent.press(screen.getByTestId(`blueprint-craft-${agent.slug}`));

  expect(screen.queryByTestId('blueprint-gallery')).toBeNull();
  expect(screen.getByTestId('agent-builder-request').props.value).toBe(agent.brief);
  expect(builder.generate).not.toHaveBeenCalled();
  fireEvent.press(screen.getByText('Generate'));
  expect(builder.generate).toHaveBeenCalledWith(agent.brief);
});
