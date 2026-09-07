import { describe, expect, it, vi } from 'vitest';
import userEvent from '@testing-library/user-event';
import { screen } from '@testing-library/react';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { PageHeader } from './PageHeader';

describe('PageHeader — title shape', () => {
  it('renders the title in the reference typography and carries the caller test id', () => {
    renderWithTheme(
      <PageHeader
        title="Credentials"
        titleTestId="credentials-page-header"
      />,
    );

    const title = screen.getByTestId('credentials-page-header');
    expect(title).toHaveTextContent('Credentials');
    expect(screen.getByTestId('page-header')).toContainElement(title);
  });

  it('appends the item count to the title', () => {
    renderWithTheme(
      <PageHeader
        title="Skills"
        count={7}
        titleTestId="skills-page-header"
      />,
    );

    expect(screen.getByTestId('skills-page-header')).toHaveTextContent('Skills (7)');
  });

  it('renders no action cluster when the caller passes no slot', () => {
    renderWithTheme(<PageHeader title="Skills" />);

    expect(screen.getByTestId('page-header').children).toHaveLength(1);
  });

  it('orders the right cluster search, filters, view toggle, actions', () => {
    renderWithTheme(
      <PageHeader
        title="Skills"
        slots={{
          search: <button type="button">search</button>,
          filters: <button type="button">filters</button>,
          viewToggle: <button type="button">view</button>,
          actions: <button type="button">create</button>,
        }}
      />,
    );

    const labels = screen.getAllByRole('button').map((element) => element.textContent);
    expect(labels).toEqual(['search', 'filters', 'view', 'create']);
  });
});

describe('PageHeader — tabs shape', () => {
  const TABS = [
    { value: 'all', label: 'All' },
    { value: 'latest', label: 'Latest (3)' },
  ];

  it('renders one tab per entry, with the caller test-id prefix and aria label', () => {
    renderWithTheme(
      <PageHeader tabs={{ items: TABS, selectedIndex: 0, ariaLabel: 'Agents', testIdPrefix: 'agents-tab' }} />,
    );

    expect(screen.getByRole('tablist', { name: 'Agents' })).toBeInTheDocument();
    expect(screen.getByTestId('agents-tab-all')).toHaveTextContent('All');
    expect(screen.getByTestId('agents-tab-latest')).toHaveTextContent('Latest (3)');
  });

  it('reports the clicked index to the caller', async () => {
    const onChangeTab = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(
      <PageHeader tabs={{ items: TABS, selectedIndex: 0, onChange: onChangeTab, testIdPrefix: 'agents-tab' }} />,
    );

    await user.click(screen.getByTestId('agents-tab-latest'));

    expect(onChangeTab).toHaveBeenCalledTimes(1);
    expect(onChangeTab.mock.calls[0]?.[1]).toBe(1);
  });

  /**
   * `selectedIndex={false}` is MUI's own "nothing matched" value. The pages
   * pass it while the router settles on a tab; a number would select a tab
   * the URL does not name, and MUI warns about an out-of-range value.
   */
  it('selects no tab when the caller reports no match', () => {
    renderWithTheme(
      <PageHeader tabs={{ items: TABS, selectedIndex: false, testIdPrefix: 'agents-tab' }} />,
    );

    for (const tab of screen.getAllByRole('tab')) {
      expect(tab).toHaveAttribute('aria-selected', 'false');
    }
  });

  it('drops the title when tabs are present', () => {
    renderWithTheme(
      <PageHeader
        title="Agents"
        tabs={{ items: TABS, selectedIndex: 0 }}
      />,
    );

    expect(screen.queryByText('Agents')).toBeNull();
  });
});
