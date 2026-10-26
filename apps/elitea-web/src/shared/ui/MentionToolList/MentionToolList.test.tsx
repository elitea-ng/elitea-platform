import { fireEvent } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { MentionToolList, type MentionTool } from '.';

const TOOLS: MentionTool[] = [
  { name: 'search_web', description: 'Searches the web' },
  { name: 'read_file', description: 'Reads a file' },
];

describe('MentionToolList', () => {
  it('renders the toolkit name and every tool', () => {
    const { getByText } = renderWithTheme(
      <MentionToolList
        tools={TOOLS}
        toolkitName="GitHub"
        onSelectTool={vi.fn()}
        highlightedIndex={-1}
      />,
    );
    expect(getByText(/GitHub/)).toBeInTheDocument();
    expect(getByText('search_web')).toBeInTheDocument();
    expect(getByText('read_file')).toBeInTheDocument();
  });

  it('titles each run of rows that share a group, once', () => {
    const grouped: MentionTool[] = [
      { name: 'one', group: 'Agent skills' },
      { name: 'two', group: 'Agent skills' },
      { name: 'three', group: 'Project skills' },
    ];
    const { getAllByText, getByText } = renderWithTheme(<MentionToolList tools={grouped} toolkitName="Skill" onSelectTool={vi.fn()} highlightedIndex={-1} />);
    expect(getAllByText('Agent skills')).toHaveLength(1);
    expect(getByText('Project skills')).toBeInTheDocument();
  });

  it('shows no section title for rows without a group', () => {
    const { queryByText } = renderWithTheme(<MentionToolList tools={TOOLS} toolkitName="GitHub" onSelectTool={vi.fn()} highlightedIndex={-1} />);
    expect(queryByText('Project skills')).toBeNull();
  });

  it('calls onSelectTool with the tool name when a row is clicked', () => {
    const onSelectTool = vi.fn();
    const { getByText } = renderWithTheme(
      <MentionToolList
        tools={TOOLS}
        toolkitName="GitHub"
        onSelectTool={onSelectTool}
        highlightedIndex={-1}
      />,
    );
    getByText('read_file').click();
    expect(onSelectTool).toHaveBeenCalledWith('read_file');
  });

  it('marks the row at highlightedIndex as highlighted', () => {
    const { getAllByRole } = renderWithTheme(
      <MentionToolList
        tools={TOOLS}
        toolkitName="GitHub"
        onSelectTool={vi.fn()}
        highlightedIndex={1}
      />,
    );
    const buttons = getAllByRole('button');
    expect(buttons[0]?.getAttribute('data-highlighted')).toBeNull();
    expect(buttons[1]?.getAttribute('data-highlighted')).toBe('true');
  });

  it('calls onSelectTool(null) on an outside click', async () => {
    const onSelectTool = vi.fn();
    renderWithTheme(
      <div>
        <button type="button">outside</button>
        <MentionToolList
          tools={TOOLS}
          toolkitName="GitHub"
          onSelectTool={onSelectTool}
          highlightedIndex={-1}
        />
      </div>,
    );
    // MUI's ClickAwayListener only "activates" after a `setTimeout(0)` past
    // mount (avoids reacting to the very click that opened it) — a
    // same-tick fireEvent is a no-op, so the test has to cross that tick.
    await new Promise((resolve) => setTimeout(resolve, 0));
    fireEvent.click(document.body);
    expect(onSelectTool).toHaveBeenCalledWith(null);
  });

  it('renders with an empty tools list without crashing', () => {
    const { getByText, queryAllByRole } = renderWithTheme(
      <MentionToolList
        tools={[]}
        toolkitName="GitHub"
        onSelectTool={vi.fn()}
        highlightedIndex={-1}
      />,
    );
    expect(getByText(/GitHub/)).toBeInTheDocument();
    expect(queryAllByRole('button')).toHaveLength(0);
  });

  it('does not crash when highlightedIndex points past an empty tools list (no highlighted element to scroll to)', () => {
    expect(() =>
      renderWithTheme(
        <MentionToolList
          tools={[]}
          toolkitName="GitHub"
          onSelectTool={vi.fn()}
          highlightedIndex={0}
        />,
      ),
    ).not.toThrow();
  });

  it('renders a tool with no description without passing an explicit undefined through', () => {
    const { getByText, queryByText } = renderWithTheme(
      <MentionToolList
        tools={[{ name: 'no_description_tool' }]}
        toolkitName="GitHub"
        onSelectTool={vi.fn()}
        highlightedIndex={-1}
      />,
    );
    expect(getByText('no_description_tool')).toBeInTheDocument();
    expect(queryByText('Searches the web')).not.toBeInTheDocument();
  });

  it('forwards data-testid', () => {
    const { getByTestId } = renderWithTheme(
      <MentionToolList
        tools={TOOLS}
        toolkitName="GitHub"
        onSelectTool={vi.fn()}
        highlightedIndex={-1}
        data-testid="mention-list"
      />,
    );
    expect(getByTestId('mention-list')).toBeInTheDocument();
  });

  // jsdom lays nothing out (every rect/offset/clientHeight defaults to 0),
  // so the scroll-adjustment branches in the highlight-tracking effect need
  // the geometry mocked directly to exercise either direction.
  it('scrolls up when the newly-highlighted item sits above the sticky header', () => {
    const { getByTestId, rerender } = renderWithTheme(
      <MentionToolList
        tools={TOOLS}
        toolkitName="GitHub"
        onSelectTool={vi.fn()}
        highlightedIndex={-1}
        data-testid="mention-list"
      />,
    );
    const container = getByTestId('mention-list');
    const header = container.firstElementChild as HTMLElement;
    const firstItem = container.querySelectorAll('button')[0] as HTMLElement;
    Object.defineProperty(header, 'offsetHeight', { value: 20, configurable: true });
    vi.spyOn(container, 'getBoundingClientRect').mockReturnValue({ top: 100 } as DOMRect);
    vi.spyOn(firstItem, 'getBoundingClientRect').mockReturnValue({ top: 105, bottom: 130 } as DOMRect);
    container.scrollTop = 50;

    rerender(
      <MentionToolList
        tools={TOOLS}
        toolkitName="GitHub"
        onSelectTool={vi.fn()}
        highlightedIndex={0}
        data-testid="mention-list"
      />,
    );

    // itemTopRelative (105-100=5) < headerHeight (20) -> scrollTop += 5-20
    expect(container.scrollTop).toBe(50 + (5 - 20));
  });

  it('scrolls down when the newly-highlighted item sits below the visible area', () => {
    const { getByTestId, rerender } = renderWithTheme(
      <MentionToolList
        tools={TOOLS}
        toolkitName="GitHub"
        onSelectTool={vi.fn()}
        highlightedIndex={-1}
        data-testid="mention-list"
      />,
    );
    const container = getByTestId('mention-list');
    const header = container.firstElementChild as HTMLElement;
    const secondItem = container.querySelectorAll('button')[1] as HTMLElement;
    Object.defineProperty(header, 'offsetHeight', { value: 20, configurable: true });
    Object.defineProperty(container, 'clientHeight', { value: 100, configurable: true });
    vi.spyOn(container, 'getBoundingClientRect').mockReturnValue({ top: 0 } as DOMRect);
    vi.spyOn(secondItem, 'getBoundingClientRect').mockReturnValue({ top: 150, bottom: 180 } as DOMRect);
    container.scrollTop = 10;

    rerender(
      <MentionToolList
        tools={TOOLS}
        toolkitName="GitHub"
        onSelectTool={vi.fn()}
        highlightedIndex={1}
        data-testid="mention-list"
      />,
    );

    // itemTopRelative (150) is not < headerHeight (20); itemBottomRelative
    // (180) > container.clientHeight (100) -> scrollTop += 180-100
    expect(container.scrollTop).toBe(10 + (180 - 100));
  });

  it('treats a missing sticky header as zero height instead of throwing', () => {
    const { getByTestId, rerender } = renderWithTheme(
      <MentionToolList
        tools={TOOLS}
        toolkitName="GitHub"
        onSelectTool={vi.fn()}
        highlightedIndex={-1}
        data-testid="mention-list"
      />,
    );
    const container = getByTestId('mention-list');
    const firstItem = container.querySelectorAll('button')[0] as HTMLElement;
    // `firstElementChild` typed as `Element | null` — the sticky header
    // Box always renders as one in practice, but the `instanceof
    // HTMLElement` guard exists for the type, not just for `null`.
    Object.defineProperty(container, 'firstElementChild', { value: null, configurable: true });
    vi.spyOn(container, 'getBoundingClientRect').mockReturnValue({ top: 0 } as DOMRect);
    vi.spyOn(firstItem, 'getBoundingClientRect').mockReturnValue({ top: -5, bottom: 20 } as DOMRect);
    container.scrollTop = 30;

    expect(() =>
      rerender(
        <MentionToolList
          tools={TOOLS}
          toolkitName="GitHub"
          onSelectTool={vi.fn()}
          highlightedIndex={0}
          data-testid="mention-list"
        />,
      ),
    ).not.toThrow();
    // headerHeight defaults to 0 -> itemTopRelative (-5) < 0 -> scrollTop += -5-0
    expect(container.scrollTop).toBe(30 + (-5 - 0));
  });
});
