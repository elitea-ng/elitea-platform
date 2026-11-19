import { act, fireEvent } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { toolkitValidation } from '@/entities/toolkit';

import { renderWithProviders } from '../__tests__/testUtils';
import type { SlashParticipantToolkit } from '../lib/hooks/useSlashMention';

import { SlashSuggestionList } from './SlashSuggestionList';
import type { SlashSuggestionListProps } from './SlashSuggestionList';
import type { UseValidateToolkitQuery } from './ToolkitValidator';

const GITHUB: SlashParticipantToolkit = { id: 'tk-1', projectId: 'p-1', type: 'github', name: 'GitHub' };
const JIRA: SlashParticipantToolkit = { id: 'tk-2', projectId: 'p-1', type: 'jira', name: 'Jira' };
const MCP: SlashParticipantToolkit = { id: 'tk-3', projectId: 'p-1', type: 'mcp', name: 'My MCP' };

const noopValidate: UseValidateToolkitQuery = () => ({ isError: false, error: undefined });

function baseProps(overrides: Partial<SlashSuggestionListProps> = {}): SlashSuggestionListProps {
  return {
    phase: 'idle',
    toolkitQuery: '',
    toolQuery: '',
    selectedToolkit: null,
    isQueryFinal: false,
    onSelectToolkit: vi.fn(),
    onSelectTool: vi.fn(),
    onClose: vi.fn(),
    participantToolkits: [GITHUB, JIRA],
    isMcpVisible: true,
    activeIndex: -1,
    setActiveIndex: vi.fn(),
    itemCountRef: { current: 0 },
    onConfirmActiveRef: { current: null },
    useValidateToolkitQuery: noopValidate,
    useToolkitDetailsQuery: () => ({ tools: [], isFetching: false }),
    ...overrides,
  };
}

function resetValidationStore(): void {
  const keys = Object.keys(toolkitValidation.useToolkitValidationStore.getState().infoByKey);
  for (const key of keys) {
    toolkitValidation.useToolkitValidationStore.getState().setToolkitValidationInfo(key, []);
  }
}

beforeEach(() => {
  resetValidationStore();
});

describe('SlashSuggestionList', () => {
  it('renders nothing in idle phase', () => {
    const { container } = renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'idle' })} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('renders the toolkit dropdown in toolkit phase, with the MCP-aware title', () => {
    const { getByText } = renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit', isMcpVisible: true })} />);
    expect(getByText('Mention Toolkit or MCP')).toBeInTheDocument();
    expect(getByText('GitHub')).toBeInTheDocument();
    expect(getByText('Jira')).toBeInTheDocument();
  });

  it('uses the non-MCP title and excludes MCP toolkits when isMcpVisible is false', () => {
    const { getByText, queryByText } = renderWithProviders(
      <SlashSuggestionList {...baseProps({ phase: 'toolkit', isMcpVisible: false, participantToolkits: [GITHUB, MCP] })} />,
    );
    expect(getByText('Mention Toolkit')).toBeInTheDocument();
    expect(getByText('GitHub')).toBeInTheDocument();
    expect(queryByText('My MCP')).not.toBeInTheDocument();
  });

  it('lists a first Skills entry, with no toolkit in the chat, and a pick hands over to the skill picker', () => {
    const onSelectSkills = vi.fn();
    const { getByTestId, getByText } = renderWithProviders(
      <SlashSuggestionList {...baseProps({ phase: 'toolkit', participantToolkits: [], onSelectSkills })} />,
    );
    expect(getByText('Skills')).toBeInTheDocument();
    fireEvent.click(getByTestId('slash-skills-entry'));
    expect(onSelectSkills).toHaveBeenCalledTimes(1);
  });

  it('puts Skills before the toolkits, and Enter on the first row picks it', () => {
    const onSelectSkills = vi.fn();
    const onSelectToolkit = vi.fn();
    const props = baseProps({ phase: 'toolkit', onSelectSkills, onSelectToolkit });
    const { container } = renderWithProviders(<SlashSuggestionList {...props} />);
    const rows = [...container.querySelectorAll('button')].map((row) => row.textContent);
    expect(rows[0]).toMatch(/^Skills/);
    expect(rows[1]).toMatch(/^GitHub/);
    // The list counts the Skills row, so Enter on index 0 is Skills and on index 1 the first toolkit.
    expect(props.itemCountRef.current).toBe(3);
    props.onConfirmActiveRef.current?.(0);
    expect(onSelectSkills).toHaveBeenCalledTimes(1);
    props.onConfirmActiveRef.current?.(1);
    expect(onSelectToolkit).toHaveBeenCalledWith(GITHUB);
  });

  it('shows Skills only while what was typed starts "skill", and not once it is all of it', () => {
    const onSelectSkills = vi.fn();
    const shown = (toolkitQuery: string): boolean => {
      const view = renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit', toolkitQuery, onSelectSkills })} />);
      const isShown = view.queryByTestId('slash-skills-entry') !== null;
      view.unmount();
      return isShown;
    };
    expect(shown('')).toBe(true);
    expect(shown('sk')).toBe(true);
    expect(shown('skill')).toBe(false);
    expect(shown('git')).toBe(false);
  });

  it('has no Skills entry when the chat cannot take a skill', () => {
    const { queryByTestId, getByText } = renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit' })} />);
    expect(queryByTestId('slash-skills-entry')).not.toBeInTheDocument();
    expect(getByText('GitHub')).toBeInTheDocument();
  });

  it('filters the toolkit dropdown by toolkitQuery (case-insensitive substring)', () => {
    const { getByText, queryByText } = renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit', toolkitQuery: 'git' })} />);
    expect(getByText('GitHub')).toBeInTheDocument();
    expect(queryByText('Jira')).not.toBeInTheDocument();
  });

  it('excludes a toolkit that has recorded validation errors', () => {
    // Unique id (not GITHUB/JIRA's tk-1/tk-2) so setting this key in the
    // shared, module-scope validation store cannot leak `forceSkip: true`
    // into any other test — the store has no key-delete API to reset with.
    const brokenToolkit: SlashParticipantToolkit = { id: 'tk-broken', projectId: 'p-broken', type: 'github', name: 'Broken' };
    toolkitValidation.useToolkitValidationStore.getState().setToolkitValidationInfo('p-broken_tk-broken', [{ msg: 'broken' }]);
    const { getByText, queryByText } = renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit', participantToolkits: [GITHUB, brokenToolkit] })} />);
    expect(queryByText('Broken')).not.toBeInTheDocument();
    expect(getByText('GitHub')).toBeInTheDocument();
  });

  it('renders one ToolkitValidator per participant toolkit, forwarding the injected query', () => {
    // Unique ids for the same reason as the test above.
    const tkA: SlashParticipantToolkit = { id: 'tk-a', projectId: 'p-a', type: 'github', name: 'A' };
    const tkB: SlashParticipantToolkit = { id: 'tk-b', projectId: 'p-a', type: 'jira', name: 'B' };
    const useValidateToolkitQuery = vi.fn<UseValidateToolkitQuery>(() => ({ isError: false, error: undefined }));
    renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit', participantToolkits: [tkA, tkB], useValidateToolkitQuery })} />);
    expect(useValidateToolkitQuery).toHaveBeenCalledWith({ projectId: 'p-a', toolkitId: 'tk-a', forceSkip: false });
    expect(useValidateToolkitQuery).toHaveBeenCalledWith({ projectId: 'p-a', toolkitId: 'tk-b', forceSkip: false });
  });

  it('auto-selects the unique matching toolkit when isQueryFinal becomes true', () => {
    const onSelectToolkit = vi.fn();
    renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit', toolkitQuery: 'git', isQueryFinal: true, onSelectToolkit })} />);
    expect(onSelectToolkit).toHaveBeenCalledWith(GITHUB);
  });

  it('calls onClose when isQueryFinal is true but no participant matches the query', () => {
    const onClose = vi.fn();
    renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit', toolkitQuery: 'nope', isQueryFinal: true, onClose })} />);
    expect(onClose).toHaveBeenCalled();
  });

  it('closes (does not re-select) when the resolved match is already the selected toolkit', () => {
    // Faithful port of the baseline's own effect: `match && (match differs
    // from selectedToolkit)` is the ONLY branch that calls onSelectToolkit;
    // a match that's already the current selection falls through to
    // onClose() instead (matches `SlashSuggestionList.jsx`'s own `if
    // (match && (...)) { onSelectToolkit } else { onClose }`).
    const onSelectToolkit = vi.fn();
    const onClose = vi.fn();
    renderWithProviders(
      <SlashSuggestionList
        {...baseProps({ phase: 'toolkit', toolkitQuery: 'git', isQueryFinal: true, selectedToolkit: { id: 'tk-1', projectId: 'p-1', name: 'GitHub', type: 'github' }, onSelectToolkit, onClose })}
      />,
    );
    expect(onSelectToolkit).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalled();
  });

  it('keeps itemCountRef in sync with the currently visible list and resets activeIndex', () => {
    const itemCountRef = { current: -1 };
    const setActiveIndex = vi.fn();
    renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit', itemCountRef, setActiveIndex })} />);
    expect(itemCountRef.current).toBe(2);
    expect(setActiveIndex).toHaveBeenCalledWith(0);
  });

  it('registers onConfirmActiveRef.current for the toolkit phase, selecting the toolkit at the given index', () => {
    const onSelectToolkit = vi.fn();
    const onConfirmActiveRef: { current: ((index: number) => void) | null } = { current: null };
    renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit', onConfirmActiveRef, onSelectToolkit })} />);
    act(() => onConfirmActiveRef.current?.(1));
    expect(onSelectToolkit).toHaveBeenCalledWith(JIRA);
  });

  it('registers onConfirmActiveRef.current for the tool phase, selecting the tool at the given index', () => {
    const onSelectTool = vi.fn();
    const onConfirmActiveRef: { current: ((index: number) => void) | null } = { current: null };
    renderWithProviders(
      <SlashSuggestionList
        {...baseProps({
          phase: 'tool',
          selectedToolkit: { id: 'tk-1', projectId: 'p-1', name: 'GitHub', type: 'github' },
          onConfirmActiveRef,
          onSelectTool,
          useToolkitDetailsQuery: () => ({ tools: [{ name: 'create_issue' }, { name: 'close_issue' }], isFetching: false }),
        })}
      />,
    );
    act(() => onConfirmActiveRef.current?.(1));
    expect(onSelectTool).toHaveBeenCalledWith('close_issue');
  });

  it('clears onConfirmActiveRef.current in idle phase', () => {
    const onConfirmActiveRef: { current: ((index: number) => void) | null } = { current: () => {} };
    renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'idle', onConfirmActiveRef })} />);
    expect(onConfirmActiveRef.current).toBeNull();
  });

  describe('tool phase', () => {
    const selectedToolkit = { id: 'tk-1', projectId: 'p-1', name: 'GitHub', type: 'github' };

    it('renders the tool list with the injected details', () => {
      const { getByText } = renderWithProviders(
        <SlashSuggestionList
          {...baseProps({ phase: 'tool', selectedToolkit, useToolkitDetailsQuery: () => ({ tools: [{ name: 'create_issue', description: 'Create an issue' }], isFetching: false }) })}
        />,
      );
      expect(getByText('create_issue')).toBeInTheDocument();
      expect(getByText('Create an issue')).toBeInTheDocument();
    });

    it('filters tools by toolQuery', () => {
      const { getByText, queryByText } = renderWithProviders(
        <SlashSuggestionList
          {...baseProps({
            phase: 'tool',
            selectedToolkit,
            toolQuery: 'create',
            useToolkitDetailsQuery: () => ({ tools: [{ name: 'create_issue' }, { name: 'close_issue' }], isFetching: false }),
          })}
        />,
      );
      expect(getByText('create_issue')).toBeInTheDocument();
      expect(queryByText('close_issue')).not.toBeInTheDocument();
    });

    it('hides the list entirely when the filter matches nothing and the details fetch is done', () => {
      const { container } = renderWithProviders(
        <SlashSuggestionList
          {...baseProps({
            phase: 'tool',
            selectedToolkit,
            toolQuery: 'nomatch',
            useToolkitDetailsQuery: () => ({ tools: [{ name: 'create_issue' }], isFetching: false }),
          })}
        />,
      );
      expect(container).toBeEmptyDOMElement();
    });

    it('keeps the list visible (does not hide) while the details fetch is still in flight, even with zero current matches', () => {
      const { container } = renderWithProviders(
        <SlashSuggestionList
          {...baseProps({
            phase: 'tool',
            selectedToolkit,
            toolQuery: 'nomatch',
            useToolkitDetailsQuery: () => ({ tools: [], isFetching: true }),
          })}
        />,
      );
      expect(container).not.toBeEmptyDOMElement();
    });

    it('calls the injected useToolkitDetailsQuery with skip: true outside tool phase', () => {
      const useToolkitDetailsQuery = vi.fn(() => ({ tools: [], isFetching: false }));
      renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'toolkit', useToolkitDetailsQuery })} />);
      expect(useToolkitDetailsQuery).toHaveBeenCalledWith({ projectId: undefined, toolkitId: undefined, skip: true });
    });

    it('calls the injected useToolkitDetailsQuery with skip: false and the selected toolkit ids in tool phase', () => {
      const useToolkitDetailsQuery = vi.fn(() => ({ tools: [], isFetching: false }));
      renderWithProviders(<SlashSuggestionList {...baseProps({ phase: 'tool', selectedToolkit, useToolkitDetailsQuery })} />);
      expect(useToolkitDetailsQuery).toHaveBeenCalledWith({ projectId: 'p-1', toolkitId: 'tk-1', skip: false });
    });
  });
});
