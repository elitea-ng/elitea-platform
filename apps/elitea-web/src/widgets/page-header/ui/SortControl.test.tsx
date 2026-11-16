import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { render as renderBare, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { SortControl } from './ListSidebarControls';

const FIELDS = {
  items: [
    { value: 'folder', label: 'Folder order', directionless: true },
    { value: 'name', label: 'Name' },
  ],
  defaultField: 'folder',
};

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function render(ui: ReactElement): void {
  renderBare(
    <ThemeProvider
      theme={theme}
      defaultMode={DEFAULT_COLOR_SCHEME}
    >
      {ui}
    </ThemeProvider>,
  );
}

describe('SortControl', () => {
  it('has no direction switch for a directionless field (a folder\'s own order)', () => {
    render(
      <SortControl
        fields={FIELDS}
        search={{}}
        patch={vi.fn()}
        testIdPrefix="agent"
      />,
    );
    expect(screen.getByTestId('agent-sort-field')).toHaveValue('folder');
    expect(screen.queryByTestId('agent-sort-direction')).not.toBeInTheDocument();
  });

  it('keeps the direction switch for an ordinary field', () => {
    render(
      <SortControl
        fields={FIELDS}
        search={{ sort_by: 'name' }}
        patch={vi.fn()}
        testIdPrefix="agent"
      />,
    );
    expect(screen.getByTestId('agent-sort-direction')).toBeInTheDocument();
  });
});
