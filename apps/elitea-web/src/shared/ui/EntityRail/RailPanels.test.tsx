import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { RailAuthorCardView } from './RailAuthorCard';
import { RailTagsPanelView } from './RailTagsPanel';
import { RailTrendingAuthorsView } from './RailTrendingAuthors';
import { railStatForKind, resolveRailStat } from './lib/railStatistics';

describe('RailTagsPanelView', () => {
  const tags = [
    { id: 1, name: 'alpha' },
    { id: 2, name: 'beta' },
  ];

  it('sorts the selected chip to the front and marks it pressed', () => {
    renderWithTheme(
      <RailTagsPanelView
        tags={tags}
        selectedTags={['beta']}
        onToggleTag={vi.fn()}
        onClearTags={vi.fn()}
        isLoading={false}
        isError={false}
      />,
    );
    const chips = screen.getAllByRole('button').filter((element) => element.getAttribute('data-testid')?.startsWith('tags-panel-chip-'));
    expect(chips.map((chip) => chip.textContent)).toEqual(['beta', 'alpha']);
    expect(screen.getByTestId('tags-panel-chip-beta')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('tags-panel-chip-alpha')).toHaveAttribute('aria-pressed', 'false');
  });

  // Issue 2049: the unselected chip used the rail's own surface colour, so it looked like bare text.
  it('paints an unselected chip with a tint, not the rail surface colour (issue 2049)', () => {
    renderWithTheme(
      <RailTagsPanelView
        tags={tags}
        selectedTags={[]}
        onToggleTag={vi.fn()}
        onClearTags={vi.fn()}
        isLoading={false}
        isError={false}
      />,
    );
    const background = getComputedStyle(screen.getByTestId('tags-panel-chip-alpha')).backgroundColor;
    expect(background).toContain('userInputBackground');
  });

  it('shows the clear-all control only while something is selected', async () => {
    const onClearTags = vi.fn();
    const view = renderWithTheme(
      <RailTagsPanelView
        tags={tags}
        selectedTags={[]}
        onToggleTag={vi.fn()}
        onClearTags={onClearTags}
        isLoading={false}
        isError={false}
      />,
    );
    expect(screen.queryByTestId('tags-panel-clear-all')).toBeNull();

    view.rerender(
      <RailTagsPanelView
        tags={tags}
        selectedTags={['alpha']}
        onToggleTag={vi.fn()}
        onClearTags={onClearTags}
        isLoading={false}
        isError={false}
      />,
    );
    await userEvent.click(screen.getByTestId('tags-panel-clear-all'));
    expect(onClearTags).toHaveBeenCalledTimes(1);
  });

  it('reports the clicked chip name', async () => {
    const onToggleTag = vi.fn();
    renderWithTheme(
      <RailTagsPanelView
        tags={tags}
        selectedTags={[]}
        onToggleTag={onToggleTag}
        onClearTags={vi.fn()}
        isLoading={false}
        isError={false}
      />,
    );
    await userEvent.click(screen.getByTestId('tags-panel-chip-alpha'));
    expect(onToggleTag).toHaveBeenCalledWith('alpha');
  });

  it('renders skeleton chips instead of the list while loading', () => {
    renderWithTheme(
      <RailTagsPanelView
        tags={[]}
        selectedTags={[]}
        onToggleTag={vi.fn()}
        onClearTags={vi.fn()}
        isLoading
        isError={false}
      />,
    );
    expect(screen.getAllByTestId('entity-rail-tag-skeleton')).toHaveLength(10);
    expect(screen.queryByText('No tags to display.')).toBeNull();
  });

  it('says what the caller filters by when there is nothing to show', () => {
    renderWithTheme(
      <RailTagsPanelView
        tags={[]}
        selectedTags={[]}
        onToggleTag={vi.fn()}
        onClearTags={vi.fn()}
        isLoading={false}
        isError={false}
        emptyText="No types to display."
      />,
    );
    expect(screen.getByText('No types to display.')).toBeInTheDocument();
    expect(screen.queryByText('No tags to display.')).toBeNull();
  });

  /*
   * DEFECT this pins. The panel took `flex: 1` whatever it held, so on a
   * project with no tags it claimed the whole rail for one line of text and
   * pushed `RailAuthorCard` onto the bottom edge of the viewport, ~600px below
   * the panel it belongs under. Absorbing the rail's free height is what a
   * FULL tag list does; an empty one has nothing to absorb it with.
   */
  it('gives up the rail\'s free height when it holds no chips, and takes it when it does', () => {
    const view = renderWithTheme(
      <RailTagsPanelView
        tags={[]}
        selectedTags={[]}
        onToggleTag={vi.fn()}
        onClearTags={vi.fn()}
        isLoading={false}
        isError={false}
      />,
    );
    expect(getComputedStyle(screen.getByTestId('entity-rail-tags')).flexGrow).toBe('0');

    view.rerender(
      <RailTagsPanelView
        tags={tags}
        selectedTags={[]}
        onToggleTag={vi.fn()}
        onClearTags={vi.fn()}
        isLoading={false}
        isError={false}
      />,
    );
    expect(getComputedStyle(screen.getByTestId('entity-rail-tags')).flexGrow).toBe('1');

    // The loading state holds ten skeleton chips, so it keeps the slack: the
    // card must not move once while tags load and again when they arrive.
    view.rerender(
      <RailTagsPanelView
        tags={[]}
        selectedTags={[]}
        onToggleTag={vi.fn()}
        onClearTags={vi.fn()}
        isLoading
        isError={false}
      />,
    );
    expect(getComputedStyle(screen.getByTestId('entity-rail-tags')).flexGrow).toBe('1');
  });
});

describe('RailAuthorCardView', () => {
  it('renders the route-selected statistic and its published row', () => {
    renderWithTheme(
      <RailAuthorCardView
        name="Ada Lovelace"
        statistic={resolveRailStat(railStatForKind('agents'), { total_applications: 7, public_applications: 3 })}
        isLoading={false}
      />,
    );
    // ONE line, `Agents:7Published:3` in the DOM — `AuthorStatistics.jsx`
    // separates label from value with a 0.25rem margin, not a text node, and
    // puts both pairs inside a single <Typography>. The port used to emit a
    // block row per statistic, which stacked them.
    expect(screen.getByTestId('entity-rail-author-total')).toHaveTextContent('Agents:7');
    expect(screen.getByTestId('entity-rail-author-published')).toHaveTextContent('Published:3');
    expect(
      screen.getByTestId('entity-rail-author-total').parentElement,
    ).toBe(screen.getByTestId('entity-rail-author-published').parentElement);
  });

  it('renders no published row for pipelines', () => {
    renderWithTheme(
      <RailAuthorCardView
        name="Ada Lovelace"
        statistic={resolveRailStat(railStatForKind('pipelines'), { total_pipelines: 5 })}
        isLoading={false}
      />,
    );
    expect(screen.getByTestId('entity-rail-author-total')).toHaveTextContent('Pipelines:5');
    expect(screen.queryByTestId('entity-rail-author-published')).toBeNull();
  });

  it('renders the optional Indexes row only when a count is supplied', () => {
    renderWithTheme(
      <RailAuthorCardView
        name="Ada Lovelace"
        statistic={resolveRailStat(railStatForKind('toolkits'), { total_toolkits: 2 })}
        indexesTotal={4}
        isLoading={false}
      />,
    );
    expect(screen.getByTestId('entity-rail-author-indexes')).toHaveTextContent('Indexes:4');
  });

  it('renders a skeleton while loading and nothing at all for a nameless author', () => {
    const view = renderWithTheme(
      <RailAuthorCardView
        name=""
        isLoading
      />,
    );
    expect(screen.getByTestId('entity-rail-author-skeleton')).toBeInTheDocument();

    view.rerender(
      <RailAuthorCardView
        name=""
        isLoading={false}
      />,
    );
    expect(screen.queryByTestId('entity-rail-author')).toBeNull();
  });
});

describe('RailTrendingAuthorsView', () => {
  it('lists the authors under the "Trending Authors" title', () => {
    renderWithTheme(
      <RailTrendingAuthorsView
        authors={[{ id: '1', name: 'Ada', email: 'ada@example.com' }]}
        isLoading={false}
        isError={false}
      />,
    );
    expect(screen.getByText('Trending Authors')).toBeInTheDocument();
    expect(screen.getAllByTestId('entity-rail-trending-author')).toHaveLength(1);
    expect(screen.getByText('ada@example.com')).toBeInTheDocument();
  });

  it('renders an empty message rather than an empty list', () => {
    renderWithTheme(
      <RailTrendingAuthorsView
        authors={[]}
        isLoading={false}
        isError={false}
      />,
    );
    expect(screen.getByText('No authors to display.')).toBeInTheDocument();
  });
});
