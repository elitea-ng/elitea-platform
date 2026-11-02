import type { ReactNode } from 'react';

import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { PageChromeProvider, useShellCrumbs } from '../page-chrome';
import { DrawerPageHeader } from './DrawerPageHeader';

function CrumbProbe(): ReactNode {
  return <div data-testid="crumbs">{useShellCrumbs().map((crumb) => crumb.label).join(' / ')}</div>;
}

describe('DrawerPageHeader', () => {
  it('names the page in the shell title row and draws no heading of its own there', () => {
    renderWithTheme(
      <PageChromeProvider>
        <DrawerPageHeader title="Profile" />
        <CrumbProbe />
      </PageChromeProvider>,
    );

    expect(screen.getByTestId('crumbs')).toHaveTextContent('Profile');
    expect(screen.queryByRole('heading')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Back' })).toBeNull();
  });

  it('draws the title as the page heading where no shell title row exists', () => {
    renderWithTheme(<DrawerPageHeader title="Profile" />);

    expect(screen.getByRole('heading', { level: 1, name: 'Profile' })).toBeInTheDocument();
  });

  it('offers a plain "Search" placeholder by default', () => {
    renderWithTheme(
      <DrawerPageHeader
        title="Secrets"
        showSearchInput
        slotProps={{ searchInput: { search: '', onChangeSearch: () => undefined } }}
      />,
    );

    expect(screen.getByPlaceholderText('Search')).toBeInTheDocument();
  });
});
