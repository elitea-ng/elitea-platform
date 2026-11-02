/**
 * SidebarFooter.test.tsx — the rail's foot: the account menu (Settings,
 * Credentials, the Catalog, Help Center, as on the desktop) and the support
 * assistant bar. The menu itself is covered in `widgets/account-menu`.
 */
import userEvent from '@testing-library/user-event';
import { screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../test/setup';
import { renderAtPath } from './testRouter';
import { SidebarAccount, SidebarFooterBar } from '../ui/SidebarFooter';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/social/author', () => HttpResponse.json({ id: 3, name: 'Alex K', email: 'alex@example.com' })),
    http.get('*/auth/permissions/administration', () => HttpResponse.json([])),
    http.get('*/.well-known/elitea-client', () => HttpResponse.json({ server_version: '1.72' })),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('SidebarAccount — the account menu at the rail\'s foot', () => {
  it('holds Settings, Credentials, the Catalog and Help Center', async () => {
    const user = userEvent.setup();
    await renderAtPath('/chat', <SidebarAccount collapsed={false} credentials />);
    await user.click(screen.getByTestId('shell-account'));
    const menu = await screen.findByRole('menu', { name: 'Account menu' });
    for (const name of ['Settings', 'Credentials', 'Catalog', 'Get help', 'Sign out']) {
      expect(within(menu).getByRole('menuitem', { name })).toBeInTheDocument();
    }
    expect(screen.getByTestId('account-menu-version')).toHaveTextContent('Version dev');
  });

  it('leaves Credentials out for a person who may not open them', async () => {
    const user = userEvent.setup();
    await renderAtPath('/chat', <SidebarAccount collapsed={false} />);
    await user.click(screen.getByTestId('shell-account'));
    const menu = await screen.findByRole('menu', { name: 'Account menu' });
    expect(within(menu).getByRole('menuitem', { name: 'Catalog' })).toBeInTheDocument();
    expect(within(menu).queryByRole('menuitem', { name: 'Credentials' })).not.toBeInTheDocument();
  });
});

/**
 * The 3.25rem "Support Bot | ?" bar under the account menu
 * (`SidebarBody.jsx`'s `styles.footerBlock`), there only when the deployment
 * has a support assistant.
 */
describe('SidebarFooterBar — the support assistant bar', () => {
  it('renders the Support Bot hit area and the help icon when an assistant handle is supplied', async () => {
    await renderAtPath(
      '/chat',
      <SidebarFooterBar
        collapsed={false}
        onToggleAssistant={() => undefined}
      />,
    );
    expect(screen.getByTestId('sidebar-support-assistant')).toBeInTheDocument();
    expect(screen.getByText('Support Bot')).toBeInTheDocument();
    // The "?" is an icon button here, not the full-width labelled row.
    expect(screen.queryByText('Help Center')).not.toBeInTheDocument();
    expect(screen.getByLabelText('Help Center')).toHaveAttribute('href', '/help-center');
  });

  it('invokes the assistant handle when the Support Bot area is clicked', async () => {
    const user = userEvent.setup();
    let opened = 0;
    await renderAtPath(
      '/chat',
      <SidebarFooterBar
        collapsed={false}
        onToggleAssistant={() => {
          opened += 1;
        }}
      />,
    );
    await user.click(screen.getByTestId('sidebar-support-assistant'));
    expect(opened).toBe(1);
  });

  it('renders nothing without an assistant: Help Center is in the account menu', async () => {
    for (const collapsed of [false, true]) {
      const { container, unmount } = await renderAtPath('/chat', <SidebarFooterBar collapsed={collapsed} />);
      expect(screen.queryByTestId('sidebar-support-assistant')).not.toBeInTheDocument();
      expect(container.querySelector('a[href="/help-center"]')).toBeNull();
      unmount();
    }
  });
});
