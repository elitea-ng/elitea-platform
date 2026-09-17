import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { EMPTY_AGENT_DRAFT, type AgentDraft } from '../../lib/agentDraft';
import { GenerateAgentReviewForm, type GenerateAgentReviewFormProps } from './GenerateAgentReviewForm';

function baseProps(overrides: Partial<GenerateAgentReviewFormProps> = {}): GenerateAgentReviewFormProps {
  return {
    draft: EMPTY_AGENT_DRAFT,
    onChange: vi.fn(),
    onValidationChange: vi.fn(),
    selection: {
      toolkits: { selectedIds: new Set(), onToggle: vi.fn() },
      mcp: { selectedIds: new Set(), onToggle: vi.fn() },
      pipelines: { selectedIds: new Set(), onToggle: vi.fn() },
      agents: { selectedIds: new Set(), onToggle: vi.fn() },
      skills: { selectedIds: new Set(), onToggle: vi.fn() },
    },
    ...overrides,
  };
}

describe('GenerateAgentReviewForm', () => {
  it('renders the current draft field values', () => {
    const draft: AgentDraft = { ...EMPTY_AGENT_DRAFT, name: 'My Agent', description: 'A helper', instructions: 'Do things', welcome_message: 'Hi there' };
    renderWithTheme(<GenerateAgentReviewForm {...baseProps({ draft })} />);

    expect(screen.getByDisplayValue('My Agent')).toBeInTheDocument();
    expect(screen.getByDisplayValue('A helper')).toBeInTheDocument();
    expect(screen.getByDisplayValue('Do things')).toBeInTheDocument();
    expect(screen.getByDisplayValue('Hi there')).toBeInTheDocument();
  });

  /* elitea_issues: #5402 — Welcome Message is multiline/scrollable, matching Description and Instructions */
  it('renders the Welcome Message field as a multiline textarea, not a single-line input', () => {
    const draft: AgentDraft = { ...EMPTY_AGENT_DRAFT, welcome_message: 'Hi there' };
    renderWithTheme(<GenerateAgentReviewForm {...baseProps({ draft })} />);

    const welcomeInput = screen.getByTestId('agent-draft-welcome-message-input');
    expect(welcomeInput.tagName).toBe('TEXTAREA');
  });

  it('calls onChange with the updated name field', () => {
    const onChange = vi.fn();
    renderWithTheme(<GenerateAgentReviewForm {...baseProps({ onChange })} />);

    fireEvent.change(screen.getByTestId('agent-draft-name-input'), { target: { value: 'New Name' } });

    expect(onChange).toHaveBeenCalledWith({ ...EMPTY_AGENT_DRAFT, name: 'New Name' });
  });

  it('reports invalid when the draft fails validation and calls onValidationChange(false)', () => {
    const onValidationChange = vi.fn();
    renderWithTheme(<GenerateAgentReviewForm {...baseProps({ onValidationChange })} />);

    expect(onValidationChange).toHaveBeenCalledWith(false);
    expect(screen.getByText('Name is required')).toBeInTheDocument();
  });

  it('reports valid once name and description are non-blank', () => {
    const onValidationChange = vi.fn();
    const draft: AgentDraft = { ...EMPTY_AGENT_DRAFT, name: 'Agent', description: 'Desc' };
    renderWithTheme(<GenerateAgentReviewForm {...baseProps({ draft, onValidationChange })} />);

    expect(onValidationChange).toHaveBeenLastCalledWith(true);
  });

  it('does not render the starters section when there are no starters', () => {
    renderWithTheme(<GenerateAgentReviewForm {...baseProps()} />);
    expect(screen.queryByText('Conversation starters:')).not.toBeInTheDocument();
  });

  it('renders one row per conversation starter and adds a new one', () => {
    const onChange = vi.fn();
    const draft: AgentDraft = { ...EMPTY_AGENT_DRAFT, conversation_starters: ['Hello'] };
    renderWithTheme(<GenerateAgentReviewForm {...baseProps({ draft, onChange })} />);

    expect(screen.getByDisplayValue('Hello')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Starter'));
    expect(onChange).toHaveBeenCalledWith({ ...draft, conversation_starters: ['Hello', ''] });
  });

  it('removes a starter at its index', () => {
    const onChange = vi.fn();
    const draft: AgentDraft = { ...EMPTY_AGENT_DRAFT, conversation_starters: ['First', 'Second'] };
    renderWithTheme(<GenerateAgentReviewForm {...baseProps({ draft, onChange })} />);

    const [firstRemoveButton] = screen.getAllByLabelText('Remove starter');
    expect(firstRemoveButton).toBeDefined();
    fireEvent.click(firstRemoveButton as HTMLElement);

    expect(onChange).toHaveBeenCalledWith({ ...draft, conversation_starters: ['Second'] });
  });

  it('edits a starter value at its index', () => {
    const onChange = vi.fn();
    const draft: AgentDraft = { ...EMPTY_AGENT_DRAFT, conversation_starters: ['First', 'Second'] };
    renderWithTheme(<GenerateAgentReviewForm {...baseProps({ draft, onChange })} />);

    fireEvent.change(screen.getByDisplayValue('Second'), { target: { value: 'Updated' } });

    expect(onChange).toHaveBeenCalledWith({ ...draft, conversation_starters: ['First', 'Updated'] });
  });

  it('disables adding a starter once the max is reached', () => {
    const draft: AgentDraft = { ...EMPTY_AGENT_DRAFT, conversation_starters: ['a', 'b', 'c', 'd'] };
    renderWithTheme(<GenerateAgentReviewForm {...baseProps({ draft })} />);

    expect(screen.getByText('Starter').closest('button')).toBeDisabled();
  });

  it('renders suggested toolkits when present and reflects selection', () => {
    const draft: AgentDraft = {
      ...EMPTY_AGENT_DRAFT,
      suggested_toolkits: [{ id: 1, name: 'GitHub', type: 'github' }],
    };
    const props = baseProps({ draft });
    renderWithTheme(
      <GenerateAgentReviewForm
        {...props}
        selection={{ ...props.selection, toolkits: { selectedIds: new Set([1]), onToggle: vi.fn() } }}
      />,
    );

    expect(screen.getByText('Suggested Toolkits:')).toBeInTheDocument();
    expect(screen.getByRole('checkbox')).toBeChecked();
  });

  it('calls onToggleToolkit when a suggested toolkit is toggled', () => {
    const onToggleToolkit = vi.fn();
    const draft: AgentDraft = {
      ...EMPTY_AGENT_DRAFT,
      suggested_toolkits: [{ id: 1, name: 'GitHub', type: 'github' }],
    };
    const props = baseProps({ draft });
    renderWithTheme(
      <GenerateAgentReviewForm
        {...props}
        selection={{ ...props.selection, toolkits: { selectedIds: new Set(), onToggle: onToggleToolkit } }}
      />,
    );

    fireEvent.click(screen.getByRole('checkbox'));
    expect(onToggleToolkit).toHaveBeenCalledWith(1);
  });
});
