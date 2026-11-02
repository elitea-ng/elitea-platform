import { describe, expect, it, vi } from 'vitest';
import userEvent from '@testing-library/user-event';
import { screen } from '@testing-library/react';

import { SidePanelSlotProvider, type SidePanelSlot } from '@/shared/desktop/inspectorSlot';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { renderHeaderRoute } from '../__tests__/testRouter';
import { PageToolbar } from './PageToolbar';

const TABS = [
  { value: 'all', label: 'All' },
  { value: 'latest', label: 'Latest (3)' },
];

function sidePanel(): SidePanelSlot {
  return { register: () => () => undefined, targets: new Map(), show: vi.fn() };
}

describe('PageToolbar — left cluster', () => {
  it('renders one tab per entry, with the caller test-id prefix and aria label', () => {
    renderWithTheme(<PageToolbar tabs={{ items: TABS, selectedIndex: 0, ariaLabel: 'Agents', testIdPrefix: 'agents-tab' }} />);

    expect(screen.getByRole('tablist', { name: 'Agents' })).toBeInTheDocument();
    expect(screen.getByTestId('agents-tab-all')).toHaveTextContent('All');
    expect(screen.getByTestId('agents-tab-latest')).toHaveTextContent('Latest (3)');
  });

  it('reports the clicked index to the caller', async () => {
    const onChangeTab = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(<PageToolbar tabs={{ items: TABS, selectedIndex: 0, onChange: onChangeTab, testIdPrefix: 'agents-tab' }} />);

    await user.click(screen.getByTestId('agents-tab-latest'));

    expect(onChangeTab).toHaveBeenCalledTimes(1);
    expect(onChangeTab.mock.calls[0]?.[1]).toBe(1);
  });

  it('selects no tab when the caller reports no match', () => {
    renderWithTheme(<PageToolbar tabs={{ items: TABS, selectedIndex: false, testIdPrefix: 'agents-tab' }} />);

    for (const tab of screen.getAllByRole('tab')) {
      expect(tab).toHaveAttribute('aria-selected', 'false');
    }
  });

  it('shows the summary where there are no tabs', () => {
    renderWithTheme(<PageToolbar summary="12 credentials" />);

    expect(screen.getByTestId('page-toolbar-summary')).toHaveTextContent('12 credentials');
  });

  /** The shell's title row names the page: the toolbar never draws a heading of its own. */
  it('draws no heading', () => {
    renderWithTheme(<PageToolbar tabs={{ items: TABS, selectedIndex: 0 }} />);

    expect(screen.queryByRole('heading')).toBeNull();
  });
});

describe('PageToolbar — right cluster', () => {
  it('orders search, filters, view toggle, secondary actions and the primary action', async () => {
    renderHeaderRoute(
      <SidePanelSlotProvider value={sidePanel()}>
        <PageToolbar
          summary="3 agents"
          search={{ 'data-testid': 'search' }}
          filters={{ activeCount: 0 }}
          viewToggle={<button type="button">view</button>}
          secondaryActions={<button type="button">import</button>}
          primaryAction={{ label: 'New agent', onClick: vi.fn() }}
        />
      </SidePanelSlotProvider>,
    );

    await screen.findByTestId('search');
    const order = [screen.getByTestId('page-search'), screen.getByTestId('page-toolbar-filters'), screen.getByText('view'), screen.getByText('import'), screen.getByRole('button', { name: 'New agent' })];
    order.slice(1).forEach((element, index) => {
      expect(order[index]!.compareDocumentPosition(element) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });
  });

  it('leads the row with the search on a type gallery (no tabs, no summary)', async () => {
    renderHeaderRoute(<PageToolbar search={{ 'data-testid': 'gallery-search' }} />);

    await screen.findByTestId('gallery-search');
    const toolbar = screen.getByTestId('page-toolbar');
    expect(toolbar.firstElementChild).toContainElement(screen.getByTestId('page-search'));
  });

  it('runs the primary action', async () => {
    const onClick = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(<PageToolbar primaryAction={{ label: 'New credential', testId: 'create', onClick }} />);

    await user.click(screen.getByTestId('create'));

    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('opens the side panel on Filters, badged with the active count', async () => {
    const slot = sidePanel();
    const user = userEvent.setup();
    renderWithTheme(
      <SidePanelSlotProvider value={slot}>
        <PageToolbar filters={{ activeCount: 2 }} />
      </SidePanelSlotProvider>,
    );

    expect(screen.getByText('2')).toBeInTheDocument();
    await user.click(screen.getByTestId('page-toolbar-filters'));

    expect(slot.show).toHaveBeenCalledWith('filters');
  });

  it('draws no Filters button where the shell has no side panel', () => {
    renderWithTheme(<PageToolbar filters={{ activeCount: 1 }} />);

    expect(screen.queryByTestId('page-toolbar-filters')).toBeNull();
  });
});
