import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ToolActionsSelector } from './ToolActionsSelector';

describe('ToolActionsSelector', () => {
  it('renders one chip per available tool, in the accordion view by default', () => {
    const { getByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={['google', 'wiki']}
        onChange={vi.fn()}
      />,
    );
    expect(getByText('Google')).toBeInTheDocument();
    expect(getByText('Wiki')).toBeInTheDocument();
    // #924/ELITEA-2816,2817: the accordion title carries an enabled/total count.
    expect(getByText('Tools 0/2')).toBeInTheDocument();
  });

  it('counts only VALID selections in the accordion title, live as tools/selection change', () => {
    const { getByText, rerender } = renderWithTheme(
      <ToolActionsSelector
        availableTools={['google', 'wiki']}
        onChange={vi.fn()}
        selectedTools={['google', 'stale_tool']}
      />,
    );
    expect(getByText('Tools 1/2')).toBeInTheDocument();

    rerender(
      <ToolActionsSelector
        availableTools={['google', 'wiki']}
        onChange={vi.fn()}
        selectedTools={['google', 'wiki']}
      />,
    );
    expect(getByText('Tools 2/2')).toBeInTheDocument();
  });

  it('renders flat (no accordion) when shouldUseAccordionView is false', () => {
    const { getByText, queryByRole } = renderWithTheme(
      <ToolActionsSelector
        availableTools={['google']}
        onChange={vi.fn()}
        shouldUseAccordionView={false}
      />,
    );
    expect(getByText('Google')).toBeInTheDocument();
    expect(queryByRole('button', { name: 'Tools' })).not.toBeInTheDocument();
  });

  it('calls onChange with the tool added when an unselected chip is clicked', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={['google']}
        onChange={onChange}
        selectedTools={[]}
      />,
    );
    await user.click(getByText('Google'));
    expect(onChange).toHaveBeenCalledWith(['google']);
  });

  it('calls onChange with the tool removed when a selected chip is clicked', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={['google']}
        onChange={onChange}
        selectedTools={['google']}
      />,
    );
    await user.click(getByText('Google'));
    expect(onChange).toHaveBeenCalledWith([]);
  });

  it('shows EmptyMcpTools when it is an MCP toolkit with no tools yet', () => {
    const { getByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={[]}
        onChange={vi.fn()}
        isRemoteMcp
      />,
    );
    expect(getByText(/No tools to display for now/)).toBeInTheDocument();
  });

  it('shows a "Load Tools" action for a remote MCP toolkit', () => {
    const { getByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={[]}
        onChange={vi.fn()}
        isRemoteMcp
        canLoadTools
      />,
    );
    expect(getByText('Load Tools')).toBeInTheDocument();
  });

  it('calls onLoadTools when the Load Tools action is clicked and loading is allowed', async () => {
    const user = userEvent.setup();
    const onLoadTools = vi.fn();
    const { getByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={[]}
        onChange={vi.fn()}
        isRemoteMcp
        canLoadTools
        onLoadTools={onLoadTools}
      />,
    );
    await user.click(getByText('Load Tools'));
    expect(onLoadTools).toHaveBeenCalledTimes(1);
  });

  it('shows "Loading..." while tools are being fetched', () => {
    const { getByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={[]}
        onChange={vi.fn()}
        isRemoteMcp
        isLoadingTools
      />,
    );
    expect(getByText('Loading...')).toBeInTheDocument();
  });

  it('renders extraProperties for a non-MCP toolkit', () => {
    const { getByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={['google']}
        onChange={vi.fn()}
        extraProperties={<div>extra field</div>}
      />,
    );
    expect(getByText('extra field')).toBeInTheDocument();
  });

  it('hides extraProperties for an MCP-like toolkit', () => {
    const { queryByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={[]}
        onChange={vi.fn()}
        isRemoteMcp
        extraProperties={<div>extra field</div>}
      />,
    );
    expect(queryByText('extra field')).not.toBeInTheDocument();
  });

  it('renders the caller-supplied mcpAuthModal slot', () => {
    const { getByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={[]}
        onChange={vi.fn()}
        mcpAuthModal={<div>auth modal</div>}
      />,
    );
    expect(getByText('auth modal')).toBeInTheDocument();
  });

  it('renders a warning chip for a selected tool no longer available', () => {
    const { getByText } = renderWithTheme(
      <ToolActionsSelector
        availableTools={['google']}
        onChange={vi.fn()}
        selectedTools={['stale_tool']}
      />,
    );
    expect(getByText('stale_tool')).toBeInTheDocument();
  });
});

/**
 * ELITEA-2684 … ELITEA-2697 — the grouped Tools picker.
 *
 * The flat list is NOT removed: it is what a toolkit whose tools carry no
 * served classification still gets, and the last test here is the one that
 * keeps that true.
 */
describe('ToolActionsSelector — grouped tools', () => {
  const availableTools = ['read_file', 'list_files', 'get_file_metadata', 'create_file', 'delete_file', 'execute_generic_rq'];
  const toolGroups = {
    read_file: 'read',
    list_files: 'read',
    get_file_metadata: 'read',
    create_file: 'create_update',
    delete_file: 'delete',
    execute_generic_rq: 'execute',
  };

  function renderGrouped(overrides: Partial<React.ComponentProps<typeof ToolActionsSelector>> = {}) {
    return renderWithTheme(
      <ToolActionsSelector
        availableTools={availableTools}
        onChange={vi.fn()}
        selectedTools={[]}
        toolGroups={toolGroups}
        toolGroupOrder={['read', 'create_update', 'delete', 'execute']}
        shouldUseAccordionView={false}
        {...overrides}
      />,
    );
  }

  it('renders the four groups in the fixed order, each with a label, a badge and a count (ELITEA-2684)', () => {
    const { getByTestId, getAllByTestId } = renderGrouped();
    const rendered = getAllByTestId(/^tool-group-(read|create-update|delete|execute)$/).map((node) => node.dataset['testid']);
    expect(rendered).toEqual(['tool-group-read', 'tool-group-create-update', 'tool-group-delete', 'tool-group-execute']);
    expect(getByTestId('tool-group-badge-read')).toHaveTextContent('Read-only');
    expect(getByTestId('tool-group-badge-create-update')).toHaveTextContent('Changes data');
    expect(getByTestId('tool-group-badge-delete')).toHaveTextContent('Destructive');
    expect(getByTestId('tool-group-badge-execute')).toHaveTextContent('Unrestricted');
    expect(getByTestId('tool-group-count-read')).toHaveTextContent('0 / 3');
  });

  it('a group header selects every tool of its group and leaves the others alone (ELITEA-2685)', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByTestId } = renderGrouped({ selectedTools: ['delete_file'], onChange });
    await user.click(getByTestId('tool-group-toggle-read').querySelector('input') as HTMLInputElement);
    expect(onChange).toHaveBeenCalledWith(['delete_file', 'read_file', 'list_files', 'get_file_metadata']);
  });

  it('a fully-selected group header deselects the whole group', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByTestId } = renderGrouped({ selectedTools: ['read_file', 'list_files', 'get_file_metadata', 'delete_file'], onChange });
    expect(getByTestId('tool-group-count-read')).toHaveTextContent('3 / 3');
    await user.click(getByTestId('tool-group-toggle-read').querySelector('input') as HTMLInputElement);
    expect(onChange).toHaveBeenCalledWith(['delete_file']);
  });

  it('filters across groups by display name and raw tool name, and keeps the counts whole (ELITEA-2690, ELITEA-2695)', async () => {
    const user = userEvent.setup();
    const { getByTestId, queryByTestId, getByText, queryByText } = renderGrouped({ selectedTools: ['read_file', 'list_files'] });
    expect(getByTestId('tool-group-count-read')).toHaveTextContent('2 / 3');
    await user.type(getByTestId('tool-group-search'), 'metadata');
    // Only the Read group still has a match; the other three headers are gone.
    expect(queryByTestId('tool-group-delete')).not.toBeInTheDocument();
    expect(queryByText('Create file')).not.toBeInTheDocument();
    expect(getByText('Get file metadata')).toBeInTheDocument();
    // …and the count still answers for the whole group, not the filtered view.
    expect(getByTestId('tool-group-count-read')).toHaveTextContent('2 / 3');
  });

  it('says so, inline, when nothing matches (ELITEA-2695 steps 9-10)', async () => {
    const user = userEvent.setup();
    const { getByTestId, queryByTestId } = renderGrouped();
    await user.type(getByTestId('tool-group-search'), 'xyznonexistent123');
    expect(getByTestId('tool-group-no-matches')).toHaveTextContent('No tools match "xyznonexistent123"');
    expect(queryByTestId('tool-group-read')).not.toBeInTheDocument();
  });

  it('keeps an UNAVAILABLE tool above the groups and out of the search (ELITEA-2691)', async () => {
    const user = userEvent.setup();
    const { getByText, getByTestId } = renderGrouped({ selectedTools: ['retired_tool'] });
    expect(getByText('retired_tool')).toBeInTheDocument();
    await user.type(getByTestId('tool-group-search'), 'metadata');
    expect(getByText('retired_tool')).toBeInTheDocument();
  });

  it('falls back to the flat list when the toolkit carries NO group metadata (ELITEA-2688)', () => {
    const { getByText, queryByTestId } = renderWithTheme(
      <ToolActionsSelector
        availableTools={['read_file', 'delete_file']}
        onChange={vi.fn()}
        selectedTools={[]}
        shouldUseAccordionView={false}
      />,
    );
    expect(getByText('Read file')).toBeInTheDocument();
    expect(getByText('Delete file')).toBeInTheDocument();
    expect(queryByTestId('tool-groups')).not.toBeInTheDocument();
    expect(queryByTestId('tool-group-search')).not.toBeInTheDocument();
  });

  it('renders the trailing MCP control AFTER the groups (ELITEA-2687)', () => {
    const { getByTestId } = renderGrouped({ trailingProperties: <div data-testid="mcp-slot">mcp</div> });
    const groups = getByTestId('tool-groups');
    const slot = getByTestId('mcp-slot');
    expect(groups.compareDocumentPosition(slot) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
