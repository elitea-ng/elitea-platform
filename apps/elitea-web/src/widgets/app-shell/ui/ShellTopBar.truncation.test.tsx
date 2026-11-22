import { ThemeProvider } from '@mui/material/styles';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { PageChromeProvider, usePageCrumbs } from '@/shared/ui/page-chrome';

import { ShellTopBar } from './ShellTopBar';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function LongPipelineTitleHarness() {
  usePageCrumbs([{ label: 'Very Long Pipeline Name That Must Truncate Cleanly On Narrow Viewports Without Overlapping Controls' }]);
  return (
    <ShellTopBar
      sidePanel={{
        slot: {
          register: vi.fn(() => vi.fn()),
          targets: new Map(),
          show: vi.fn(),
        },
        tabs: [{ id: 'filters', title: 'Filters', subject: 'Filters' }],
        active: 'filters',
        shown: false,
        narrow: true,
        activate: vi.fn(),
        setOpen: vi.fn(),
        setBody: vi.fn(),
      }}
    />
  );
}

describe('ShellTopBar narrow-viewport truncation', () => {
  it('applies ellipsis truncation and minWidth: 0 to the breadcrumb container and heading', async () => {
    const rootRoute = createRootRoute({
      component: () => (
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <PageChromeProvider>
            <LongPipelineTitleHarness />
          </PageChromeProvider>
        </ThemeProvider>
      ),
    });
    const router = createRouter({
      routeTree: rootRoute,
      history: createMemoryHistory({ initialEntries: ['/pipelines/latest/42'] }),
    });
    render(<RouterProvider router={router} />);

    const heading = await screen.findByTestId('shell-title');
    const nav = screen.getByRole('navigation', { name: 'Breadcrumb' });
    const topBar = screen.getByTestId('shell-top-bar');

    expect(getComputedStyle(topBar).minWidth).toBe('0px');
    expect(getComputedStyle(nav).minWidth).toBe('0px');
    expect(getComputedStyle(heading).textOverflow).toBe('ellipsis');
    expect(getComputedStyle(heading).whiteSpace).toBe('nowrap');
    expect(getComputedStyle(heading).minWidth).toBe('0px');
  });
});
