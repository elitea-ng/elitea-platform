import type { ReactNode } from 'react';

import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { PageChromeProvider, PageCrumbFallback, usePageCrumbs, useShellCrumbs, type PageCrumb } from './PageChrome';

function Page({ crumbs }: { readonly crumbs: readonly PageCrumb[] }): ReactNode {
  usePageCrumbs(crumbs);
  return <PageCrumbFallback title={crumbs.at(-1)?.label ?? ''} />;
}

function TitleRow(): ReactNode {
  return <div data-testid="title-row">{useShellCrumbs().map((crumb) => crumb.label).join(' / ')}</div>;
}

describe('page chrome', () => {
  it('hands the page crumbs to the shell, and draws no heading in the page', () => {
    render(
      <PageChromeProvider>
        <TitleRow />
        <Page crumbs={[{ label: 'New toolkit' }]} />
      </PageChromeProvider>,
    );
    expect(screen.getByTestId('title-row')).toHaveTextContent('New toolkit');
    expect(screen.queryByRole('heading')).toBeNull();
  });

  it('follows the newest page, and withdraws its crumbs when it unmounts', () => {
    const view = render(
      <PageChromeProvider>
        <TitleRow />
        <Page crumbs={[{ label: 'First' }]} />
        <Page crumbs={[{ label: 'Second' }]} />
      </PageChromeProvider>,
    );
    expect(screen.getByTestId('title-row')).toHaveTextContent('Second');

    view.rerender(
      <PageChromeProvider>
        <TitleRow />
        <Page crumbs={[{ label: 'First' }]} />
      </PageChromeProvider>,
    );
    expect(screen.getByTestId('title-row')).toHaveTextContent('First');
  });

  it('draws the heading in the page where no shell draws it', () => {
    render(<Page crumbs={[{ label: 'Settings' }]} />);
    expect(screen.getByRole('heading', { level: 1, name: 'Settings' })).toBeInTheDocument();
  });
});
